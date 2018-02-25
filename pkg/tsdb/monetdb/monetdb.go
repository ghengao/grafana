package monetdb

import (
	"container/list"
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	_ "github.com/ghengao/go-monetdb"
	"github.com/go-xorm/core"
	"github.com/grafana/grafana/pkg/components/null"
	"github.com/grafana/grafana/pkg/components/simplejson"
	"github.com/grafana/grafana/pkg/log"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/tsdb"
)

var logger = log.New("tsdb.monetdb")

func init() {
	tsdb.RegisterTsdbQueryEndpoint("ghengao-monetdb-datasource", NewMonetDbQueryEndpoint)
}

// SQLEngine implements the grafana SqlEngine interface
type SQLEngine struct {
	DbEngine    *sql.DB
	MacroEngine tsdb.SqlMacroEngine
}

type engineCacheType struct {
	cache    map[int64]*sql.DB
	versions map[int64]int
	sync.Mutex
}

var engineCache = engineCacheType{
	cache:    make(map[int64]*sql.DB),
	versions: make(map[int64]int),
}

// InitEngine creates the db connection and inits the monetdb engine or loads it from the engine cache
func (e *SQLEngine) InitEngine(driverName string, dsInfo *models.DataSource, cnnstr string) error {
	engineCache.Lock()
	defer engineCache.Unlock()

	if engine, present := engineCache.cache[dsInfo.Id]; present {
		if version, _ := engineCache.versions[dsInfo.Id]; version == dsInfo.Version {
			if engine.Stats().OpenConnections > 0 {
				e.DbEngine = engine
				// logger.Debug("Engine created by cache", "id", dsInfo.Id)
				logger.Info("Engine created by cache", "id", dsInfo.Id)
				return nil
			}
		}
	}

	engine, err := sql.Open(driverName, cnnstr)
	if err != nil {
		return err
	}

	engine.SetMaxOpenConns(10)
	engine.SetMaxIdleConns(10)

	engineCache.cache[dsInfo.Id] = engine
	e.DbEngine = engine
	// logger.Debug("Engine created", "id", dsInfo.Id)
	logger.Info("Engine created", "id", dsInfo.Id)

	return nil
}

// Query is a default implementation of the Query method for an SQL data source.
// The caller of this function must implement transformToTimeSeries and transformToTable and
// pass them in as parameters.
func (e *SQLEngine) Query(
	ctx context.Context,
	dsInfo *models.DataSource,
	tsdbQuery *tsdb.TsdbQuery,
	transformToTimeSeries func(query *tsdb.Query, rows *core.Rows, result *tsdb.QueryResult) error,
	transformToTable func(query *tsdb.Query, rows *core.Rows, result *tsdb.QueryResult) error,
) (*tsdb.Response, error) {
	result := &tsdb.Response{
		Results: make(map[string]*tsdb.QueryResult),
	}

	defer e.DbEngine.Close()
	db := e.DbEngine

	for _, query := range tsdbQuery.Queries {
		rawSql := query.Model.Get("rawSql").MustString()
		if rawSql == "" {
			continue
		}

		queryResult := &tsdb.QueryResult{Meta: simplejson.New(), RefId: query.RefId}
		result.Results[query.RefId] = queryResult

		rawSql, err := e.MacroEngine.Interpolate(tsdbQuery.TimeRange, rawSql)
		if err != nil {
			queryResult.Error = err
			continue
		}

		logger.Info("Executing query", "rawSql", rawSql)

		queryResult.Meta.Set("sql", rawSql)

		rows, err := db.Query(rawSql)
		if err != nil {
			queryResult.Error = err
			logger.Error("query error", "error", err)
			continue
		}

		defer rows.Close()

		format := query.Model.Get("format").MustString("time_series")
		wrapRows := &core.Rows{
			Rows:   rows,
			Mapper: core.SameMapper{},
		}
		switch format {
		case "time_series":
			err := transformToTimeSeries(query, wrapRows, queryResult)
			if err != nil {
				queryResult.Error = err
				logger.Error("transform time series error", "error", err)
				continue
			}
		case "table":
			err := transformToTable(query, wrapRows, queryResult)
			if err != nil {
				queryResult.Error = err
				logger.Error("transform table error", "error", err)
				continue
			}
		}
	}

	return result, nil
}

type MonetDbQueryEndpoint struct {
	sqlEngine tsdb.SqlEngine
	log       log.Logger
}

func NewMonetDbQueryEndpoint(datasource *models.DataSource) (tsdb.TsdbQueryEndpoint, error) {
	endpoint := &MonetDbQueryEndpoint{
		log: log.New("tsdb.monetdb"),
	}

	endpoint.sqlEngine = &SQLEngine{
		MacroEngine: NewMonetDbMacroEngine(),
	}

	cnnstr := generateConnectionString(datasource)
	endpoint.log.Debug("getEngine", "connection", cnnstr)

	if err := endpoint.sqlEngine.InitEngine("monetdb", datasource, cnnstr); err != nil {
		return nil, err
	}

	return endpoint, nil
}

func generateConnectionString(datasource *models.DataSource) string {
	password := ""
	for key, value := range datasource.SecureJsonData.Decrypt() {
		if key == "password" {
			password = value
			break
		}
	}

	// sslmode := datasource.JsonData.Get("sslmode").MustString("verify-full")
	return fmt.Sprintf("%s:%s@%s/%s", url.PathEscape(datasource.User), url.PathEscape(password), url.PathEscape(datasource.Url), url.PathEscape(datasource.Database))
}

func (e *MonetDbQueryEndpoint) Query(ctx context.Context, dsInfo *models.DataSource, tsdbQuery *tsdb.TsdbQuery) (*tsdb.Response, error) {
	return e.sqlEngine.Query(ctx, dsInfo, tsdbQuery, e.transformToTimeSeries, e.transformToTable)
}

func (e MonetDbQueryEndpoint) transformToTable(query *tsdb.Query, rows *core.Rows, result *tsdb.QueryResult) error {

	columnNames, err := rows.Columns()
	if err != nil {
		return err
	}

	table := &tsdb.Table{
		Columns: make([]tsdb.TableColumn, len(columnNames)),
		Rows:    make([]tsdb.RowValues, 0),
	}

	for i, name := range columnNames {
		table.Columns[i].Text = name
	}

	rowLimit := 1000000
	rowCount := 0
	timeIndex := -1

	// check if there is a column named time
	for i, col := range columnNames {
		switch col {
		case "time":
			timeIndex = i
		}
	}

	for ; rows.Next(); rowCount++ {
		if rowCount > rowLimit {
			return fmt.Errorf("PostgreSQL query row limit exceeded, limit %d", rowLimit)
		}

		values, err := e.getTypedRowData(rows)
		if err != nil {
			return err
		}

		// convert column named time to unix timestamp to make
		// native datetime postgres types work in annotation queries
		if timeIndex != -1 {
			switch value := values[timeIndex].(type) {
			case time.Time:
				values[timeIndex] = float64(value.UnixNano() / 1e9)
			}
		}

		table.Rows = append(table.Rows, values)
	}

	result.Tables = append(result.Tables, table)
	result.Meta.Set("rowCount", rowCount)
	return nil
}

func (e MonetDbQueryEndpoint) getTypedRowData(rows *core.Rows) (tsdb.RowValues, error) {

	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}

	values := make([]interface{}, len(types))
	valuePtrs := make([]interface{}, len(types))

	for i := 0; i < len(types); i++ {
		valuePtrs[i] = &values[i]
	}

	if err := rows.Scan(valuePtrs...); err != nil {
		return nil, err
	}

	// convert types not handled by lib/pq
	// unhandled types are returned as []byte
	for i := 0; i < len(types); i++ {
		if value, ok := values[i].([]byte); ok == true {
			switch types[i].DatabaseTypeName() {
			case "NUMERIC":
				if v, err := strconv.ParseFloat(string(value), 64); err == nil {
					values[i] = v
				} else {
					e.log.Debug("Rows", "Error converting numeric to float", value)
				}
			case "UNKNOWN", "CIDR", "INET", "MACADDR":
				// char literals have type UNKNOWN
				values[i] = string(value)
			default:
				e.log.Debug("Rows", "Unknown database type", types[i].DatabaseTypeName(), "value", value)
				values[i] = string(value)
			}
		}
	}

	return values, nil
}

func (e MonetDbQueryEndpoint) transformToTimeSeries(query *tsdb.Query, rows *core.Rows, result *tsdb.QueryResult) error {
	pointsBySeries := make(map[string]*tsdb.TimeSeries)
	seriesByQueryOrder := list.New()

	columnNames, err := rows.Columns()
	if err != nil {
		return err
	}

	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		return err
	}

	rowLimit := 1000000
	rowCount := 0
	timeIndex := -1
	metricIndex := -1

	// check columns of resultset: a column named time is mandatory
	// the first text column is treated as metric name unless a column named metric is present
	for i, col := range columnNames {
		e.log.Debug("column loop", "name", col)
		switch col {
		case "time":
			timeIndex = i
		case "metric":
			metricIndex = i
		default:
			if metricIndex == -1 {
				switch columnTypes[i].DatabaseTypeName() {
				case "UNKNOWN", "TEXT", "VARCHAR", "CHAR":
					metricIndex = i
				}
			}
		}
	}

	if timeIndex == -1 {
		return fmt.Errorf("Found no column named time")
	}

	for rows.Next() {
		var timestamp float64
		var value null.Float
		var metric string

		if rowCount > rowLimit {
			return fmt.Errorf("PostgreSQL query row limit exceeded, limit %d", rowLimit)
		}

		values, err := e.getTypedRowData(rows)
		if err != nil {
			return err
		}

		switch columnValue := values[timeIndex].(type) {
		case int64:
			timestamp = float64(columnValue * 1000)
		case float64:
			timestamp = columnValue * 1000
		case time.Time:
			timestamp = float64(columnValue.UnixNano() / 1e6)
		default:
			return fmt.Errorf("Invalid type for column time, must be of type timestamp or unix timestamp, got: %T %v", columnValue, columnValue)
		}

		if metricIndex >= 0 {
			if columnValue, ok := values[metricIndex].(string); ok == true {
				metric = columnValue
			} else {
				return fmt.Errorf("Column metric must be of type char,varchar or text, got: %T %v", values[metricIndex], values[metricIndex])
			}
		}

		for i, col := range columnNames {
			if i == timeIndex || i == metricIndex {
				continue
			}

			switch columnValue := values[i].(type) {
			case int64:
				value = null.FloatFrom(float64(columnValue))
			case float64:
				value = null.FloatFrom(columnValue)
			case nil:
				value.Valid = false
			default:
				return fmt.Errorf("Value column must have numeric datatype, column: %s type: %T value: %v", col, columnValue, columnValue)
			}
			if metricIndex == -1 {
				metric = col
			}
			e.appendTimePoint(pointsBySeries, seriesByQueryOrder, metric, timestamp, value)
			rowCount++

		}
	}

	for elem := seriesByQueryOrder.Front(); elem != nil; elem = elem.Next() {
		key := elem.Value.(string)
		result.Series = append(result.Series, pointsBySeries[key])
	}

	result.Meta.Set("rowCount", rowCount)
	return nil
}

func (e MonetDbQueryEndpoint) appendTimePoint(pointsBySeries map[string]*tsdb.TimeSeries, seriesByQueryOrder *list.List, metric string, timestamp float64, value null.Float) {
	if series, exist := pointsBySeries[metric]; exist {
		series.Points = append(series.Points, tsdb.TimePoint{value, null.FloatFrom(timestamp)})
	} else {
		series := &tsdb.TimeSeries{Name: metric}
		series.Points = append(series.Points, tsdb.TimePoint{value, null.FloatFrom(timestamp)})
		pointsBySeries[metric] = series
		seriesByQueryOrder.PushBack(metric)
	}
	e.log.Debug("Rows", "metric", metric, "time", timestamp, "value", value)
}
