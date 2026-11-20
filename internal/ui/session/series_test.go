package session

import (
	"fmt"
	"strings"
	"testing"
)

func TestSeriesFromSQL(t *testing.T) {
	many := make([]string, MaxSeriesPoints+1)
	for i := range many {
		many[i] = fmt.Sprintf(`["k%d",%d]`, i, i)
	}
	cases := []struct {
		name, in, code string
	}{
		{"柱状", `{"columns":["month","paid_cents"],"rows":[["2026-08",14009839],["2026-09",12104587]],"row_count":2,"truncated":false}`, ""},
		{"跳过非数值列", `{"columns":["city","note","n"],"rows":[["上海","a",3],["杭州",null,1.5]],"row_count":2,"truncated":false}`, ""},
		{"数字做分类", `{"columns":["year","n"],"rows":[[2025,1],[2026,-2]],"row_count":2,"truncated":false}`, ""},
		{"截断照画", `{"columns":["c","n"],"rows":[["a",1]],"row_count":1,"truncated":true,"truncate_reason":"bytes"}`, ""},
		{"空结果", `{"columns":["c","n"],"rows":[],"row_count":0,"truncated":false}`, CodeSeriesEmpty},
		{"缺 row_count", `{"columns":["c","n"],"rows":[["a",1]]}`, CodeSeriesMissingField},
		{"缺 rows", `{"columns":["c","n"],"row_count":0,"truncated":false}`, CodeSeriesMissingField},
		{"rows 为 null", `{"columns":["c","n"],"rows":null,"row_count":0,"truncated":false}`, CodeSeriesMissingField},
		{"row_count 为负", `{"columns":["c","n"],"rows":[],"row_count":-1,"truncated":false}`, CodeSeriesMissingField},
		{"缺 columns", `{"rows":[["a",1]],"row_count":1}`, CodeSeriesMissingField},
		{"不是 JSON", `返回 2 行`, CodeSeriesMissingField},
		{"只有一列", `{"columns":["c"],"rows":[["a"]],"row_count":1,"truncated":false}`, CodeSeriesMissingField},
		{"没有数值列", `{"columns":["c","d"],"rows":[["a","1"]],"row_count":1,"truncated":false}`, CodeSeriesMissingField},
		{"数值列有空值", `{"columns":["c","n"],"rows":[["a",1],["b",null]],"row_count":2,"truncated":false}`, CodeSeriesNotPlottable},
		{"数值列混文本", `{"columns":["c","n"],"rows":[["a",1],["b","2"]],"row_count":2,"truncated":false}`, CodeSeriesNotPlottable},
		{"分类为空", `{"columns":["c","n"],"rows":[[null,1]],"row_count":1,"truncated":false}`, CodeSeriesNotPlottable},
		{"分类是编码文本", `{"columns":["c","n"],"rows":[[{"encoding":"base64","data":"/w=="},1]],"row_count":1,"truncated":false}`, CodeSeriesNotPlottable},
		{"行宽不一致", `{"columns":["c","n"],"rows":[["a"]],"row_count":1,"truncated":false}`, CodeSeriesNotPlottable},
		{"行数不符", `{"columns":["c","n"],"rows":[["a",1]],"row_count":3,"truncated":false}`, CodeSeriesNotPlottable},
		{"点太多", `{"columns":["c","n"],"rows":[` + strings.Join(many, ",") + `],"row_count":` + fmt.Sprint(len(many)) + `,"truncated":false}`, CodeSeriesNotPlottable},
		{"超出 float64", `{"columns":["c","n"],"rows":[["a",1e999]],"row_count":1,"truncated":false}`, CodeSeriesNotPlottable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, code, reason := SeriesFromSQL(tc.in)
			if code != tc.code {
				t.Fatalf("code=%q reason=%q，期望 %q", code, reason, tc.code)
			}
			if tc.code != "" {
				if s != nil || reason == "" {
					t.Fatalf("不能画时不该有序列且要有原因：%+v %q", s, reason)
				}
				return
			}
			if s == nil || len(s.Labels) != len(s.Values) || len(s.Raw) != len(s.Values) || len(s.Values) == 0 {
				t.Fatalf("序列=%+v", s)
			}
		})
	}

	s, _, _ := SeriesFromSQL(cases[1].in)
	if s.YName != "n" || s.Values[1] != 1.5 || s.Labels[0] != "上海" || s.Raw[1] != "1.5" {
		t.Fatalf("取错了列：%+v", s)
	}
	s, _, _ = SeriesFromSQL(cases[3].in)
	if !s.Truncated || s.TruncateReason != "bytes" {
		t.Fatalf("截断元数据丢了：%+v", s)
	}
	s, _, _ = SeriesFromSQL(cases[0].in)
	if s.Raw[1] != "12104587" || s.Values[1] != 12104587 {
		t.Fatalf("数值不是原文：%+v", s)
	}
}
