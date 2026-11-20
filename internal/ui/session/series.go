package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

const (
	// CodeSeriesEmpty 表示 run_sql 成功但没有返回行。
	CodeSeriesEmpty = "chart_empty"
	// CodeSeriesMissingField 表示结果缺少分类列或数值列。
	CodeSeriesMissingField = "chart_missing_field"
	// CodeSeriesNotPlottable 表示列齐全但值不能画成柱形，例如空值、文本混在数值列或点太多。
	CodeSeriesNotPlottable = "chart_not_plottable"

	// MaxSeriesPoints 是一张结构化柱状图最多的柱数，超过时不画，避免挤成一片。
	MaxSeriesPoints = 50
)

// Series 是从一次成功 run_sql 的真实结果里取出的柱状图数据：第一列做分类，
// 之后第一个出现数值的列做高度。Labels 与 Values 一一对应，Raw 是数值在结果里的原文。
// Truncated 和 TruncateReason 原样来自 run_sql，界面要一并标出，图只含返回的行。
type Series struct {
	XName          string
	YName          string
	Labels         []string
	Values         []float64
	Raw            []string
	Truncated      bool
	TruncateReason string
}

// SQLChart 是某次运行里一次成功 run_sql 的结构化图表。Series 为 nil 时 Code/Reason 说明为什么不画。
type SQLChart struct {
	CallID string
	SQL    string
	Series *Series
	Code   string
	Reason string
}

// SeriesFromSQL 解析 run_sql 交给模型的同一份 JSON 结果。不能画时返回分类码和原因，不补值也不猜坐标。
func SeriesFromSQL(content string) (*Series, string, string) {
	var res struct {
		Columns   []string          `json:"columns"`
		Rows      []json.RawMessage `json:"rows"`
		RowCount  *int              `json:"row_count"`
		Truncated bool              `json:"truncated"`
		Reason    string            `json:"truncate_reason"`
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(content)))
	if err := dec.Decode(&res); err != nil || res.Columns == nil || res.RowCount == nil {
		return nil, CodeSeriesMissingField, "结果缺少 columns、rows 或 row_count"
	}
	if len(res.Rows) == 0 {
		return nil, CodeSeriesEmpty, "查询结果为空"
	}
	if *res.RowCount != len(res.Rows) {
		return nil, CodeSeriesNotPlottable, "row_count 与返回行数不一致"
	}
	if len(res.Rows) > MaxSeriesPoints {
		return nil, CodeSeriesNotPlottable, fmt.Sprintf("结果有 %d 行，超过 %d 个柱", len(res.Rows), MaxSeriesPoints)
	}
	if len(res.Columns) < 2 {
		return nil, CodeSeriesMissingField, "结果只有一列，缺少分类列或数值列"
	}
	rows := make([][]any, len(res.Rows))
	for i, raw := range res.Rows {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if err := d.Decode(&rows[i]); err != nil || len(rows[i]) != len(res.Columns) {
			return nil, CodeSeriesNotPlottable, "结果行和列数不一致"
		}
	}
	col := -1
	for j := 1; j < len(res.Columns) && col < 0; j++ {
		for _, row := range rows {
			if _, ok := row[j].(json.Number); ok {
				col = j
				break
			}
		}
	}
	if col < 0 {
		return nil, CodeSeriesMissingField, "结果没有数值列"
	}
	s := &Series{XName: res.Columns[0], YName: res.Columns[col], Truncated: res.Truncated, TruncateReason: res.Reason}
	for i, row := range rows {
		label, ok := cellLabel(row[0])
		if !ok {
			return nil, CodeSeriesNotPlottable, fmt.Sprintf("第 %d 行的 %s 不是文本或数字", i+1, res.Columns[0])
		}
		n, ok := row[col].(json.Number)
		if !ok {
			return nil, CodeSeriesNotPlottable, fmt.Sprintf("第 %d 行的 %s 不是数字", i+1, res.Columns[col])
		}
		v, err := strconv.ParseFloat(n.String(), 64)
		if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
			return nil, CodeSeriesNotPlottable, fmt.Sprintf("第 %d 行的 %s 超出可画范围", i+1, res.Columns[col])
		}
		s.Labels = append(s.Labels, label)
		s.Values = append(s.Values, v)
		s.Raw = append(s.Raw, n.String())
	}
	return s, "", ""
}

func cellLabel(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	}
	return "", false
}

// SeriesLine 是不画结构化图时的一行说明。
func SeriesLine(t SQLChart) string {
	return fmt.Sprintf("结果不可绘图：%s（%s）", t.Reason, t.Code)
}
