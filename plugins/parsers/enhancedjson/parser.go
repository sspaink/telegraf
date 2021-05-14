package enhancedjson

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/metric"
	"github.com/tidwall/gjson"
)

type TimeFunc func() time.Time

type Parser struct {
	Configs     []Config
	DefaultTags map[string]string
	Log         telegraf.Logger
	TimeFunc    func() time.Time
}

type Config struct {
	MetricSelection string `toml:"metric_selection"`
	MetricName      string `toml:"metric_name"`
	BasicFields     []BasicField
	ObjectFields    []ObjectField
}

type BasicField struct {
	Query string `toml:"query"` // REQUIRED
	Name  string `toml:"name"`  // OPTIONAL
	Type  string `toml:"type"`  // OPTIONAL
	// TODO: add ignore_objects boolean field
}

type ObjectField struct {
	Query   string            `toml:"query"`    // REQUIRED
	NameMap map[string]string `toml:"name_map"` // OPTIONAL
	TypeMap map[string]string `toml:"type_map"` // OPTIONAL
}

type MetricNode struct {
	RootFieldName string
	DesiredType   string
	Metric        telegraf.Metric
	gjson.Result
}

func (p *Parser) Parse(input []byte) ([]telegraf.Metric, error) {
	// This is used to set a default time in the unit tests
	if p.TimeFunc == nil {
		p.TimeFunc = time.Now
	}

	// Only valid JSON is supported
	if !gjson.Valid(string(input)) {
		return nil, fmt.Errorf("Invalid JSON provided, unable to parse")
	}

	var t []telegraf.Metric

	for _, config := range p.Configs {
		// Process all `basic_fields` configurations
		basicMetrics, err := p.processBasicFields(config.MetricName, config.BasicFields, input)
		if err != nil {
			return nil, err
		}
		if len(basicMetrics) != 0 {
			t = append(t, basicMetrics...)
		}

		objectMetrics, err := p.processObjectFields(config.MetricName, config.ObjectFields, input)
		if err != nil {
			return nil, err
		}
		if len(objectMetrics) != 0 {
			t = append(t, objectMetrics...)
		}
	}

	return t, nil
}

func (p *Parser) processBasicFields(metricName string, basicFields []BasicField, input []byte) ([]telegraf.Metric, error) {
	// For each basic_field configuration, get all the metric data returned from the query
	// Keep the metric data per field separate so all results from each query can be combined
	var metricFields [][]telegraf.Metric
	for _, field := range basicFields {
		result := gjson.GetBytes(input, field.Query)

		// TODO: implement `ignore_objects` config key to ignore this error
		if result.IsObject() {
			return nil, fmt.Errorf("use object_field")
		}

		// TODO: Handle invalid input characters, are spaces allowed?? probably not
		fieldName := field.Name
		// Default to the last query word, should be the upper key name
		// TODO: figure out what to do with special characters, probably ok to remove any special characters?
		if fieldName == "" {
			s := strings.Split(field.Query, ".")
			fieldName = s[len(s)-1]
		}

		// Store result into a MetricNode to keep metadata together
		mNode := MetricNode{
			RootFieldName: fieldName,
			DesiredType:   field.Type,
			Metric: metric.New(
				metricName,
				map[string]string{},
				map[string]interface{}{},
				p.TimeFunc(),
			),
			Result: result,
		}
		// Expand all array's and nested arrays into separate metrics
		nodes, err := p.expandArray(mNode, metricName)
		if err != nil {
			return nil, err
		}

		var metricField []telegraf.Metric
		for _, n := range nodes {
			// v, err := p.convertType(n.Value(), n.DesiredType, n.Metric.Name())
			// if err != nil {
			// 	return nil, err
			// }
			// m := metric.New(
			// 	metricName,
			// 	map[string]string{},
			// 	map[string]interface{}{
			// 		n.MetricName: v,
			// 	},
			// 	p.TimeFunc(),
			// )
			metricField = append(metricField, n.Metric)
		}
		metricFields = append(metricFields, metricField)
	}

	var t []telegraf.Metric

	sort.Slice(metricFields, func(i, j int) bool {
		return len(metricFields[i]) < len(metricFields[j])
	})

	if len(metricFields) > 1 {
		// Combine metrics!
		for i := 1; i < len(metricFields); i++ {
			// merge the current row into the next row and so on
			// TODO: figure out if duplicates should be removed??? probably not? user config error?

			//Loop over previous metric fields, and add them to the next
			for _, p := range metricFields[i-1] {
				// Loop over all the current metrics
				for _, c := range metricFields[i] {
					// For each field in the current metric, add it to the next metric
					for _, f := range p.FieldList() {
						c.AddField(f.Key, f.Value)
					}
				}
			}
		}
	}

	if len(metricFields) > 0 && len(metricFields[len(metricFields)-1]) > 0 {
		for i := 0; i < len(metricFields[len(metricFields)-1]); i++ {
			t = append(t, metricFields[len(metricFields)-1][i])
		}
	}

	return t, nil
}

// expandArray will recursively create a new MetricNode for each element in a JSON array
func (p *Parser) expandArray(result MetricNode, metricName string) ([]MetricNode, error) {
	var results []MetricNode

	if result.IsObject() {
		return nil, fmt.Errorf("encountered object")
	}

	if result.IsArray() {
		var err error
		result.ForEach(func(_, val gjson.Result) bool {
			// TODO: implement `ignore_objects` config key to ignore this error
			if val.IsObject() {
				err = fmt.Errorf("encountered object")
				return false
			}

			m := metric.New(
				metricName,
				map[string]string{},
				map[string]interface{}{},
				p.TimeFunc(),
			)

			for _, f := range result.Metric.FieldList() {
				m.AddField(f.Key, f.Value)
			}

			if val.IsArray() {
				n := MetricNode{
					RootFieldName: result.RootFieldName,
					Metric:        m,
					Result:        val,
				}
				r, err := p.expandArray(n, metricName)
				if err != nil {
					return false
				}
				results = append(results, r...)
			} else {
				v, err := p.convertType(val.Value(), result.DesiredType, result.RootFieldName)
				if err != nil {
					return false
				}

				m.AddField(result.RootFieldName, v)

				for _, f := range result.Metric.FieldList() {
					m.AddField(f.Key, f.Value)
				}

				n := MetricNode{
					RootFieldName: result.RootFieldName,
					Metric:        m,
					Result:        val,
				}
				results = append(results, n)
			}
			return true
		})
		if err != nil {
			return nil, err
		}
	} else {
		if result.Exists() {
			v, err := p.convertType(result.Value(), result.DesiredType, result.RootFieldName)
			if err != nil {
				return nil, err
			}
			result.Metric.AddField(result.RootFieldName, v)
			results = append(results, result)
		}
	}

	return results, nil
}

func (p *Parser) processObjectFields(metricName string, objectFields []ObjectField, input []byte) ([]telegraf.Metric, error) {
	var t []telegraf.Metric
	for _, field := range objectFields {
		result := gjson.GetBytes(input, field.Query)

		// TODO: Figoure out how to handle root fieldname, will be blank
		// Default to the last query word, should be the upper key name
		// TODO: figure out what to do with special characters, probably ok to remove any special characters?
		s := strings.Split(field.Query, ".")
		fieldName := s[len(s)-1]

		rootObject := MetricNode{
			RootFieldName: fieldName,
			Metric: metric.New(
				metricName,
				map[string]string{},
				map[string]interface{}{},
				p.TimeFunc(),
			),
			Result: result,
		}
		metrics, err := p.combineObject(rootObject, field.NameMap, field.TypeMap)
		if err != nil {
			return nil, err
		}
		for _, m := range metrics {
			t = append(t, m.Metric)
		}
	}

	return t, nil
}

func (p *Parser) combineObject(result MetricNode, nameMap map[string]string, typeMap map[string]string) ([]MetricNode, error) {

	var metrics []MetricNode
	result.ForEach(func(key, val gjson.Result) bool {
		// Update key with user configuration
		fieldName := key.String()
		if fieldName != "" {
			if newName, ok := nameMap[fieldName]; ok {
				fieldName = newName
			}
			//Sanitize fieldname
			fieldName = strings.ReplaceAll(fieldName, " ", "")
		} else {
			fieldName = result.RootFieldName
		}

		if val.IsArray() {
			arrayNode := MetricNode{
				RootFieldName: key.String(),
				Metric:        result.Metric,
				Result:        val,
			}

			m, err := p.expandArray(arrayNode, result.Metric.Name())
			if err != nil {
				return false
			}
			// TODO: THIS IS WRONG, where to put newly expanded metrics?!?!?
			metrics = append(metrics, m...)
		} else if val.IsObject() {
			arrayNode := MetricNode{
				RootFieldName: key.String(),
				Metric:        result.Metric,
				Result:        val,
			}
			_, err := p.combineObject(arrayNode, nameMap, typeMap)
			if err != nil {
				return false
			}
		} else {
			fieldValue := val.Value()
			if desiredType, ok := typeMap[key.String()]; ok {
				var err error
				// TODO: Return this error
				fieldValue, err = p.convertType(val.Value(), desiredType, key.String())
				if err != nil {
					return false
				}
			}

			result.Metric.AddField(fieldName, fieldValue)
		}

		return true
	})

	metrics = append(metrics, result)

	return metrics, nil
}

func (p *Parser) ParseLine(line string) (telegraf.Metric, error) {
	return nil, fmt.Errorf("ParseLine is designed for parsing influx line protocol, therefore not implemented for JSONquery")
}

func (p *Parser) SetDefaultTags(tags map[string]string) {

}

func (p *Parser) SetTimeFunc(fn TimeFunc) {
	p.TimeFunc = fn
}

// convertType will convert the value parsed from the input JSON to the specified type in the config
func (p *Parser) convertType(input interface{}, desiredType string, name string) (interface{}, error) {
	switch inputType := input.(type) {
	case string:
		if desiredType != "string" {
			switch desiredType {
			case "int":
				r, err := strconv.Atoi(inputType)
				if err != nil {
					return nil, fmt.Errorf("Unable to convert field '%s' to type int: %v", name, err)
				}
				return r, nil
			case "float":
				r, err := strconv.ParseFloat(inputType, 64)
				if err != nil {
					return nil, fmt.Errorf("Unable to convert field '%s' to type float: %v", name, err)
				}
				return r, nil
			case "bool":
				r, err := strconv.ParseBool(inputType)
				if err != nil {
					return nil, fmt.Errorf("Unable to convert field '%s' to type bool: %v", name, err)
				}
				return r, nil
			}
		}
	case bool:
		if desiredType != "bool" {
			switch desiredType {
			case "string":
				return strconv.FormatBool(inputType), nil
			case "int":
				if inputType {
					return int64(1), nil
				}

				return int64(0), nil
			}
		}
	case float64:
		if desiredType != "float" {
			switch desiredType {
			case "string":
				return fmt.Sprint(inputType), nil
			case "int":
				return int64(inputType), nil
			case "bool":
				if inputType == 0 {
					return false, nil
				} else if inputType == 1 {
					return true, nil
				} else {
					return nil, fmt.Errorf("Unable to convert field '%s' to type bool", name)
				}
			}
		}
	default:
		return nil, fmt.Errorf("unknown format '%T' for field  '%s'", inputType, name)
	}

	return input, nil
}
