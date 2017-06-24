package elasticsql

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/xwb1989/sqlparser"
)

// msi stands for map[string]interface{}
type msi map[string]interface{}

func handleFuncInSelectAgg(funcExprArr []*sqlparser.FuncExpr) msi {

	var innerAggMap = make(msi)
	for _, v := range funcExprArr {
		//func expressions will use the same parent bucket

		aggName := strings.ToUpper(string(v.Name)) + `(` + sqlparser.String(v.Exprs) + `)`
		switch string(v.Name) {
		case "count":
			//count need to distinguish * and normal field name
			if sqlparser.String(v.Exprs) == "*" {
				innerAggMap[aggName] = msi{
					"value_count": msi{
						"field": "_index",
					},
				}
			} else {
				// support count(distinct field)
				if v.Distinct {
					innerAggMap[aggName] = msi{
						"cardinality": msi{
							"field": sqlparser.String(v.Exprs),
						},
					}
				} else {
					innerAggMap[aggName] = msi{
						"value_count": msi{
							"field": sqlparser.String(v.Exprs),
						},
					}
				}
			}
		default:
			// support min/avg/max
			innerAggMap[aggName] = msi{
				string(v.Name): msi{
					"field": sqlparser.String(v.Exprs),
				},
			}
		}

	}

	return innerAggMap

}

func handleGroupByColName(colName *sqlparser.ColName, index int, child msi) msi {
	innerMap := make(msi)
	if index == 0 {
		innerMap["terms"] = msi{
			"field": string(colName.Name),
			"size":  200, // this size may need to change ?
		}
	} else {
		innerMap["terms"] = msi{
			"field": string(colName.Name),
			"size":  0,
		}
	}

	if len(child) > 0 {
		innerMap["aggregations"] = child
	}
	return msi{string(colName.Name): innerMap}
}

func handleGroupByFuncExprDateHisto(funcExpr *sqlparser.FuncExpr) (msi, error) {
	innerMap := make(msi)
	var (
		// default
		field    = ""
		interval = "1h"
		format   = "yyyy-MM-dd HH:mm:ss"
	)

	//get field/interval and format
	for _, expr := range funcExpr.Exprs {
		// the expression in date_histogram must be like a = b format
		switch expr.(type) {
		case *sqlparser.NonStarExpr:
			nonStarExpr := expr.(*sqlparser.NonStarExpr)
			comparisonExpr, ok := nonStarExpr.Expr.(*sqlparser.ComparisonExpr)
			if !ok {
				return nil, errors.New("elasticsql: unsupported expression in date_histogram")
			}
			left, ok := comparisonExpr.Left.(*sqlparser.ColName)
			if !ok {
				return nil, errors.New("elaticsql: param error in date_histogram")
			}
			rightStr := sqlparser.String(comparisonExpr.Right)
			rightStr = strings.Replace(rightStr, `'`, ``, -1)
			if string(left.Name) == "field" {
				field = rightStr
			}
			if string(left.Name) == "interval" {
				interval = rightStr
			}
			if string(left.Name) == "format" {
				format = rightStr
			}

			innerMap["date_histogram"] = msi{
				"field":    field,
				"interval": interval,
				"format":   format,
			}
		default:
			return nil, errors.New("elasticsql: unsupported expression in date_histogram")
		}
	}
	return innerMap, nil
}

func handleGroupByFuncExprRange(funcExpr *sqlparser.FuncExpr) (msi, error) {
	if len(funcExpr.Exprs) < 3 {
		return nil, errors.New("elasticsql: length of function range params must be > 3")
	}

	var innerMap = make(msi)
	rangeMapList := make([]msi, len(funcExpr.Exprs)-2)

	for i := 1; i < len(funcExpr.Exprs)-1; i++ {
		valFrom := sqlparser.String(funcExpr.Exprs[i])
		valTo := sqlparser.String(funcExpr.Exprs[i+1])
		rangeMapList[i-1] = msi{
			"from": valFrom,
			"to":   valTo,
		}
	}
	innerMap[string(funcExpr.Name)] = msi{
		"field":  sqlparser.String(funcExpr.Exprs[0]),
		"ranges": rangeMapList,
	}

	return innerMap, nil
}

func handleGroupByFuncExprDateRange(funcExpr *sqlparser.FuncExpr) (msi, error) {
	var innerMap msi
	return innerMap, errors.New("elasticsql: date range not supported yet")
}

func handleGroupByFuncExpr(funcExpr *sqlparser.FuncExpr, child msi) (msi, error) {

	var innerMap msi
	var err error

	switch string(funcExpr.Name) {
	case "date_histogram":
		innerMap, err = handleGroupByFuncExprDateHisto(funcExpr)
	case "range":
		innerMap, err = handleGroupByFuncExprRange(funcExpr)
	case "date_range":
		innerMap, err = handleGroupByFuncExprDateRange(funcExpr)
	default:
		return nil, errors.New("elasticsql: unsupported group by functions" + sqlparser.String(funcExpr))
	}

	if err != nil {
		return nil, err
	}

	if len(child) > 0 && innerMap != nil {
		innerMap["aggregations"] = child
	}

	stripedFuncExpr := sqlparser.String(funcExpr)
	stripedFuncExpr = strings.Replace(stripedFuncExpr, " ", "", -1)
	stripedFuncExpr = strings.Replace(stripedFuncExpr, "'", "", -1)
	return msi{stripedFuncExpr: innerMap}, nil
}

func handleGroupByAgg(groupBy sqlparser.GroupBy, innerMap msi) (msi, error) {

	var aggMap = make(msi)

	var child = innerMap

	for i := len(groupBy) - 1; i >= 0; i-- {
		v := groupBy[i]

		switch item := v.(type) {
		case *sqlparser.ColName:
			currentMap := handleGroupByColName(item, i, child)
			child = currentMap

		case *sqlparser.FuncExpr:
			currentMap, err := handleGroupByFuncExpr(item, child)
			if err != nil {
				return nil, err
			}
			child = currentMap
		}
	}
	aggMap = child

	return aggMap, nil
}

func buildAggs(sel *sqlparser.Select) (string, error) {

	funcExprArr, _, funcErr := extractFuncAndColFromSelect(sel.SelectExprs)
	innerAggMap := handleFuncInSelectAgg(funcExprArr)

	if funcErr != nil {
	}

	aggMap, err := handleGroupByAgg(sel.GroupBy, innerAggMap)
	if err != nil {
		return "", err
	}

	mapJSON, _ := json.Marshal(aggMap)

	return string(mapJSON), nil
}

// extract func expressions from select exprs
func extractFuncAndColFromSelect(sqlSelect sqlparser.SelectExprs) ([]*sqlparser.FuncExpr, []*sqlparser.ColName, error) {
	var colArr []*sqlparser.ColName
	var funcArr []*sqlparser.FuncExpr
	for _, v := range sqlSelect {
		// non star expressioin means column name
		// or some aggregation functions
		expr, ok := v.(*sqlparser.NonStarExpr)
		if !ok {
			// no need to handle, star expression * just skip is ok
			continue
		}

		// NonStarExpr start
		switch expr.Expr.(type) {
		case *sqlparser.FuncExpr:
			funcExpr := expr.Expr.(*sqlparser.FuncExpr)
			funcArr = append(funcArr, funcExpr)

		case *sqlparser.ColName:
			continue
		default:
			//ignore
		}

		//starExpression like *, table.* should be ignored
		//'cause it is meaningless to set fields in elasticsearch aggs
	}
	return funcArr, colArr, nil
}
