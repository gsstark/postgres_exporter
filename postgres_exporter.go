package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v2"

	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
)

var Version string = "0.0.1"

var (
	listenAddress = flag.String(
		"web.listen-address", ":9113",
		"Address to listen on for web interface and telemetry.",
	)
	metricPath = flag.String(
		"web.telemetry-path", "/metrics",
		"Path under which to expose metrics.",
	)
	queriesPath = flag.String(
		"extend.query-path", "",
		"Path to custom queries to run.",
	)
	onlyDumpMaps = flag.Bool(
		"dumpmaps", false,
		"Do not run, simply dump the maps.",
	)
)

// Metric name parts.
const (
	// Namespace for all metrics.
	namespace = "pg"
	// Subsystems.
	exporter = "exporter"
)

// landingPage contains the HTML served at '/'.
// TODO: Make this nicer and more informative.
var landingPage = []byte(`<html>
<head><title>Postgres exporter</title></head>
<body>
<h1>Postgres exporter</h1>
<p><a href='` + *metricPath + `'>Metrics</a></p>
</body>
</html>
`)

type ColumnUsage int

const (
	DISCARD      ColumnUsage = iota // Ignore this column
	LABEL        ColumnUsage = iota // Use this column as a label
	COUNTER      ColumnUsage = iota // Use this column as a counter
	GAUGE        ColumnUsage = iota // Use this column as a gauge
	MAPPEDMETRIC ColumnUsage = iota // Use this column with the supplied mapping of text values
	DURATION     ColumnUsage = iota // This column should be interpreted as a text duration (and converted to milliseconds)
)

// User-friendly representation of a prometheus descriptor map
type ColumnMapping struct {
	usage       ColumnUsage
	description string
	mapping     map[string]float64 // Optional column mapping for MAPPEDMETRIC
}

// Groups metric maps under a shared set of labels
type MetricMapNamespace struct {
	labels         []string             // Label names for this namespace
	columnMappings map[string]MetricMap // Column mappings in this namespace
}

// Stores the prometheus metric description which a given column will be mapped
// to by the collector
type MetricMap struct {
	discard    bool                              // Should metric be discarded during mapping?
	vtype      prometheus.ValueType              // Prometheus valuetype
	desc       *prometheus.Desc                  // Prometheus descriptor
	conversion func(interface{}) (float64, bool) // Conversion function to turn PG result into float64
}

type MetricQueryRecipe struct {
	// basename is typically the name of a table to query
	basename string
	// sqlquery is what should be executed
	sqlquery string
	// resultmap maps column names in the resultset to the ColumnMapping that should be used to build a metric
	resultmap map[string]ColumnMapping
}

func dumpMaps(recipes []MetricQueryRecipe) {
	for _, recipe := range recipes {
		fmt.Println(recipe.basename)
		fmt.Printf("  %s\n", recipe.sqlquery)
		for column, details := range recipe.resultmap {
			fmt.Printf("    %-40s %v\n", column, details)
		}
		fmt.Println()
	}
}

func getRecipes(queriesPath string) ([]MetricQueryRecipe, error) {
	var yamldata map[string]interface{}

	content, err := ioutil.ReadFile(queriesPath)
	if err != nil {
		return nil, err
	}

	err = yaml.Unmarshal(content, &yamldata)
	if err != nil {
		return nil, err
	}

	var recipes []MetricQueryRecipe
	for basename, specs := range yamldata {
		query := "select * from " + basename
		metric_map := make(map[string]ColumnMapping)

		for key, value := range specs.(map[interface{}]interface{}) {
			switch key.(string) {
			case "query":
				query = value.(string)

			case "metrics":
				for _, c := range value.([]interface{}) {
					column := c.(map[interface{}]interface{})

					for n, a := range column {
						var cmap ColumnMapping

						name := n.(string)

						for attr_key, attr_val := range a.(map[interface{}]interface{}) {
							switch attr_key.(string) {
							case "usage":
								usage, err := stringToColumnUsage(attr_val.(string))
								if err != nil {
									return nil, err
								}
								cmap.usage = usage
							case "description":
								cmap.description = attr_val.(string)
							}
						}

						cmap.mapping = nil

						metric_map[name] = cmap
					}
				}
			}
		}
		recipes = append(recipes, MetricQueryRecipe{
			basename:  basename,
			sqlquery:  query,
			resultmap: metric_map,
		})
	}

	return recipes, nil
}

// Turn the MetricMap column mapping into a prometheus descriptor mapping.
func makeDescMap(recipes []MetricQueryRecipe) map[string]MetricMapNamespace {
	var metricMap = make(map[string]MetricMapNamespace)

	for _, recipe := range recipes {
		namespace := recipe.basename
		thisMap := make(map[string]MetricMap)

		// Get the constant labels
		var constLabels []string
		for columnName, columnMapping := range recipe.resultmap {
			if columnMapping.usage == LABEL {
				constLabels = append(constLabels, columnName)
			}
		}

		for columnName, columnMapping := range recipe.resultmap {
			switch columnMapping.usage {
			case DISCARD, LABEL:
				thisMap[columnName] = MetricMap{
					discard: true,
					conversion: func(in interface{}) (float64, bool) {
						return math.NaN(), true
					},
				}
			case COUNTER:
				thisMap[columnName] = MetricMap{
					vtype: prometheus.CounterValue,
					desc:  prometheus.NewDesc(fmt.Sprintf("%s_%s", namespace, columnName), columnMapping.description, constLabels, nil),
					conversion: func(in interface{}) (float64, bool) {
						return dbToFloat64(in)
					},
				}
			case GAUGE:
				thisMap[columnName] = MetricMap{
					vtype: prometheus.GaugeValue,
					desc:  prometheus.NewDesc(fmt.Sprintf("%s_%s", namespace, columnName), columnMapping.description, constLabels, nil),
					conversion: func(in interface{}) (float64, bool) {
						return dbToFloat64(in)
					},
				}
			case MAPPEDMETRIC:
				thisMap[columnName] = MetricMap{
					vtype: prometheus.GaugeValue,
					desc:  prometheus.NewDesc(fmt.Sprintf("%s_%s", namespace, columnName), columnMapping.description, constLabels, nil),
					conversion: func(in interface{}) (float64, bool) {
						text, ok := in.(string)
						if !ok {
							return math.NaN(), false
						}

						val, ok := columnMapping.mapping[text]
						if !ok {
							return math.NaN(), false
						}
						return val, true
					},
				}
			case DURATION:
				thisMap[columnName] = MetricMap{
					vtype: prometheus.GaugeValue,
					desc:  prometheus.NewDesc(fmt.Sprintf("%s_%s_milliseconds", namespace, columnName), columnMapping.description, constLabels, nil),
					conversion: func(in interface{}) (float64, bool) {
						var durationString string
						switch t := in.(type) {
						case []byte:
							durationString = string(t)
						case string:
							durationString = t
						default:
							log.Errorln("DURATION conversion metric was not a string")
							return math.NaN(), false
						}

						if durationString == "-1" {
							return math.NaN(), false
						}

						d, err := time.ParseDuration(durationString)
						if err != nil {
							log.Errorln("Failed converting result to metric:", columnName, in, err)
							return math.NaN(), false
						}
						return float64(d / time.Millisecond), true
					},
				}
			}
		}

		metricMap[namespace] = MetricMapNamespace{constLabels, thisMap}
	}

	return metricMap
}

// convert a string to the corresponding ColumnUsage
func stringToColumnUsage(s string) (u ColumnUsage, err error) {
	switch s {
	case "DISCARD":
		u = DISCARD

	case "LABEL":
		u = LABEL

	case "COUNTER":
		u = COUNTER

	case "GAUGE":
		u = GAUGE

	case "MAPPEDMETRIC":
		u = MAPPEDMETRIC

	case "DURATION":
		u = DURATION

	default:
		err = fmt.Errorf("wrong ColumnUsage given : %s", s)
	}

	return
}

// Convert database.sql types to float64s for Prometheus consumption. Null types are mapped to NaN. string and []byte
// types are mapped as NaN and !ok
func dbToFloat64(t interface{}) (float64, bool) {
	switch v := t.(type) {
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case time.Time:
		return float64(v.Unix()), true
	case []byte:
		// Try and convert to string and then parse to a float64
		strV := string(v)
		result, err := strconv.ParseFloat(strV, 64)
		if err != nil {
			return math.NaN(), false
		}
		return result, true
	case string:
		result, err := strconv.ParseFloat(v, 64)
		if err != nil {
			log.Infoln("Could not parse string:", err)
			return math.NaN(), false
		}
		return result, true
	case nil:
		return math.NaN(), true
	default:
		return math.NaN(), false
	}
}

// Convert database.sql to string for Prometheus labels. Null types are mapped to empty strings.
func dbToString(t interface{}) (string, bool) {
	switch v := t.(type) {
	case int64:
		return fmt.Sprintf("%v", v), true
	case float64:
		return fmt.Sprintf("%v", v), true
	case time.Time:
		return fmt.Sprintf("%v", v.Unix()), true
	case nil:
		return "", true
	case []byte:
		// Try and convert to string
		return string(v), true
	case string:
		return v, true
	default:
		return "", false
	}
}

// Exporter collects Postgres metrics. It implements prometheus.Collector.
type Exporter struct {
	dsn             string
	duration, error prometheus.Gauge
	totalScrapes    prometheus.Counter
	recipes         []MetricQueryRecipe
	metricMap       map[string]MetricMapNamespace
}

// NewExporter returns a new exporter for the provided DSN.
func NewExporter(dsn string, recipes []MetricQueryRecipe) *Exporter {
	return &Exporter{
		dsn: dsn,
		duration: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: exporter,
			Name:      "last_scrape_duration_seconds",
			Help:      "Duration of the last scrape of metrics from PostgresSQL.",
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: exporter,
			Name:      "scrapes_total",
			Help:      "Total number of times PostgresSQL was scraped for metrics.",
		}),
		error: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: exporter,
			Name:      "last_scrape_error",
			Help:      "Whether the last scrape of metrics from PostgreSQL resulted in an error (1 for error, 0 for success).",
		}),
		metricMap: makeDescMap(recipes),
		recipes:   recipes,
	}
}

// Describe implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	// We cannot know in advance what metrics the exporter will generate
	// from Postgres. So we use the poor man's describe method: Run a collect
	// and send the descriptors of all the collected metrics. The problem
	// here is that we need to connect to the Postgres DB. If it is currently
	// unavailable, the descriptors will be incomplete. Since this is a
	// stand-alone exporter and not used as a library within other code
	// implementing additional metrics, the worst that can happen is that we
	// don't detect inconsistent metrics created by this exporter
	// itself. Also, a change in the monitored Postgres instance may change the
	// exported metrics during the runtime of the exporter.

	metricCh := make(chan prometheus.Metric)
	doneCh := make(chan struct{})

	go func() {
		for m := range metricCh {
			ch <- m.Desc()
		}
		close(doneCh)
	}()

	e.Collect(metricCh)
	close(metricCh)
	<-doneCh
}

// Collect implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.scrape(ch)

	ch <- e.duration
	ch <- e.totalScrapes
	ch <- e.error
}

func newDesc(subsystem, name, help string) *prometheus.Desc {
	return prometheus.NewDesc(
		prometheus.BuildFQName(namespace, subsystem, name),
		help, nil, nil,
	)
}

func (e *Exporter) scrape(ch chan<- prometheus.Metric) {
	defer func(begun time.Time) {
		e.duration.Set(time.Since(begun).Seconds())
	}(time.Now())

	e.error.Set(0)
	e.totalScrapes.Inc()

	db, err := sql.Open("postgres", e.dsn)
	if err != nil {
		log.Infoln("Error opening connection to database:", err)
		e.error.Set(1)
		return
	}
	defer db.Close()

	for _, recipe := range e.recipes {
		namespace := recipe.basename
		log.Debugln("Querying namespace: ", namespace)
		func() {
			// Don't fail on a bad scrape of one metric
			rows, err := db.Query(recipe.sqlquery)
			if err != nil {
				log.Infof("Error running query <%s> for '%s' on database: %v", recipe.sqlquery, namespace, err)
				e.error.Set(1)
				return
			}
			defer rows.Close()

			var columnNames []string
			columnNames, err = rows.Columns()
			if err != nil {
				log.Infoln("Error retrieving column list for: ", namespace, err)
				e.error.Set(1)
				return
			}

			// Make a lookup map for the column indices
			var columnIdx = make(map[string]int, len(columnNames))
			for i, n := range columnNames {
				columnIdx[n] = i
			}

			var columnData = make([]interface{}, len(columnNames))
			var scanArgs = make([]interface{}, len(columnNames))
			for i := range columnData {
				scanArgs[i] = &columnData[i]
			}

			for rows.Next() {
				err = rows.Scan(scanArgs...)
				if err != nil {
					log.Infoln("Error retrieving rows:", namespace, err)
					e.error.Set(1)
					return
				}

				// Get the label values for this row
				mapping := e.metricMap[namespace]
				var labels = make([]string, len(mapping.labels))
				for idx, columnName := range mapping.labels {
					labels[idx], _ = dbToString(columnData[columnIdx[columnName]])
				}

				// Loop over column names, and match to scan data. Unknown columns
				// will be filled with an untyped metric number *if* they can be
				// converted to float64s. NULLs are allowed and treated as NaN.
				for idx, columnName := range columnNames {
					if metricMapping, ok := mapping.columnMappings[columnName]; ok {
						// Is this a metricy metric?
						if metricMapping.discard {
							continue
						}

						value, ok := dbToFloat64(columnData[idx])
						if !ok {
							e.error.Set(1)
							log.Errorln("Unexpected error parsing column: ", namespace, columnName, columnData[idx])
							continue
						}

						// Generate the metric
						ch <- prometheus.MustNewConstMetric(metricMapping.desc, metricMapping.vtype, value, labels...)
					} else {
						// Unknown metric. Report as untyped if scan to float64 works, else note an error too.
						desc := prometheus.NewDesc(fmt.Sprintf("%s_%s", namespace, columnName), fmt.Sprintf("Unknown metric from %s", namespace), nil, nil)

						// Its not an error to fail here, since the values are
						// unexpected anyway.
						value, ok := dbToFloat64(columnData[idx])
						if !ok {
							log.Warnln("Unparseable column type - discarding: ", namespace, columnName, err)
							continue
						}

						ch <- prometheus.MustNewConstMetric(desc, prometheus.UntypedValue, value, labels...)
					}
				}

			}
		}()
	}
}

func main() {
	flag.Parse()

	if *queriesPath == "" {
		log.Fatalf("-extend.query-path is a required argument")
	}

	recipes, err := getRecipes(*queriesPath)
	if err != nil {
		log.Fatalf("error parsing file %q: %v", *queriesPath, err)
	}

	if *onlyDumpMaps {
		dumpMaps(recipes)
		return
	}

	dsn := os.Getenv("DATA_SOURCE_NAME")
	if len(dsn) == 0 {
		log.Fatal("couldn't find environment variable DATA_SOURCE_NAME")
	}

	exporter := NewExporter(dsn, recipes)
	prometheus.MustRegister(exporter)

	http.Handle(*metricPath, prometheus.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(landingPage)
	})

	log.Infof("Starting Server: %s", *listenAddress)
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}
