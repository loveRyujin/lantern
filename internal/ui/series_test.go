package ui

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	mygoui "github.com/egoist/mygo/ui"
	"github.com/loveRyujin/lantern/core/events"
	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/pyrt"
	"github.com/loveRyujin/lantern/internal/ui/session"
)

func sqlCall(id, query string, limit int) llm.ToolCall {
	args := map[string]any{"sql": query}
	if limit > 0 {
		args["limit"] = limit
	}
	b, _ := json.Marshal(args)
	return llm.ToolCall{ID: id, Name: "run_sql", Arguments: string(b)}
}

// 结构化图表的入口：真实 agent.Loop 调真实 run_sql，结果经 Analysis 的记录包装进入 ui/chart。
// 柱的数量、顺序和相对高度都要和模型收到的那份工具结果一致；不能画的结果只给原因、不出柱。
func TestStructuredChartFromRealSQLResult(t *testing.T) {
	h := newChartHarness(t, pyrt.Runtime{})
	tt := h.tt
	h.lr.NewProvider = func(runID string) llm.Provider {
		return llm.NewMock(llm.MustNewScript(
			llm.CallTools(sqlCall("sql-"+runID, session.ChartDemoSQL, 0)),
			llm.Say("见下方图表。"),
		))
	}
	h.rec.wrap(h.lr)
	ask(t, tt, "华东 8、9 月已支付金额")
	tt.Frame()
	run1 := h.lastRun(t)
	call := "sql-" + run1

	var seen struct {
		Columns []string `json:"columns"`
		Rows    [][]any  `json:"rows"`
	}
	if err := json.Unmarshal([]byte(observationInner(t, "run_sql", h.rec.observation(t, call))), &seen); err != nil {
		t.Fatal(err)
	}
	if len(seen.Rows) != 2 || seen.Rows[1][0] != "2026-09" || seen.Rows[1][1] != float64(12104587) {
		t.Fatalf("前提不成立，run_sql 没有读到种子库：%+v", seen)
	}
	rc, ok := h.an.Charts("星禾", run1)
	if !ok || len(rc.Tables) != 1 || rc.Tables[0].CallID != call || rc.Tables[0].SQL != session.ChartDemoSQL || rc.Tables[0].Series == nil {
		t.Fatalf("记录=%+v", rc)
	}
	sr := rc.Tables[0].Series
	if sr.XName != seen.Columns[0] || sr.YName != seen.Columns[1] || len(sr.Values) != len(seen.Rows) {
		t.Fatalf("序列与工具结果不一致：%+v vs %+v", sr, seen)
	}
	var prev mygoui.Rect
	for i, row := range seen.Rows {
		if sr.Labels[i] != row[0] || sr.Values[i] != row[1] {
			t.Fatalf("第 %d 个点 %s=%v，工具结果是 %v=%v", i, sr.Labels[i], sr.Values[i], row[0], row[1])
		}
		r := findScrolled(t, tt, barLabel(sr.Labels[i], strconv.FormatFloat(row[1].(float64), 'f', -1, 64)))
		if i > 0 {
			if r.X <= prev.X || math.Abs(float64(r.Y+r.H-(prev.Y+prev.H))) > 1 {
				t.Fatalf("柱的顺序或底边不对：%+v 之后 %+v", prev, r)
			}
			got := float64(r.H) / float64(prev.H)
			want := row[1].(float64) / seen.Rows[i-1][1].(float64)
			if math.Abs(got-want) > 0.02 {
				t.Fatalf("柱高之比 %.3f，工具结果之比 %.3f", got, want)
			}
		}
		prev = r
	}
	// 柱顶和柱底要落在窗口里画出的纵轴上：柱底在 0，柱顶在工具结果的数值处。
	area := findScrolled(t, tt, seriesLabel(call))
	at := axisAt(t, tt, area)
	for i, row := range seen.Rows {
		r, _ := tt.Find(barLabel(sr.Labels[i], strconv.FormatFloat(row[1].(float64), 'f', -1, 64)))
		top, bottom := at(row[1].(float64)), at(0)
		if math.Abs(float64(r.Y)-top) > 1.5 || math.Abs(float64(r.Y+r.H)-bottom) > 1.5 {
			t.Fatalf("柱 %s 画在 %v..%v，纵轴上应在 %.1f..%.1f", sr.Labels[i], r.Y, r.Y+r.H, top, bottom)
		}
	}
	if tt.HasText(session.ChartDemoSQL) {
		t.Fatal("SQL 默认就展开了")
	}
	trigger := findScrolled(t, tt, "SQL · "+call)
	tt.ClickAt(trigger.X+trigger.W/2, trigger.Y+trigger.H/2)
	tt.Advance(300 * time.Millisecond)
	tt.Frame()
	if !tt.HasText(session.ChartDemoSQL) {
		t.Fatal("展开后看不到 SQL")
	}

	// 切到云杉看不到星禾的柱；切回后从同一份记录重新画出。
	if err := tt.Click("云杉"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if _, found := tt.Find(seriesLabel(call)); found {
		t.Fatal("切到云杉还画着星禾的结构化图表")
	}
	if _, ok := h.an.Charts("云杉", run1); ok {
		t.Fatal("云杉拿到了星禾运行的记录")
	}
	if err := tt.Click("星禾"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	findScrolled(t, tt, seriesLabel(call))

	// 空结果、缺数值列、数值列有空值、截断：前三种只给原因，截断的照画并标出。
	empty := "SELECT u.city, COUNT(*) AS n FROM users u WHERE 1=0 GROUP BY u.city"
	oneCol := "SELECT DISTINCT u.city FROM users u ORDER BY u.city"
	withNull := "SELECT u.city, CASE WHEN u.city='上海' THEN NULL ELSE COUNT(*) END AS n FROM users u GROUP BY u.city ORDER BY u.city"
	cut := "SELECT u.city, COUNT(*) AS n FROM users u GROUP BY u.city ORDER BY u.city"
	huge := "SELECT 'a' AS k, 1e308 AS v UNION ALL SELECT 'b', -1e308"
	tiny := "SELECT 'a' AS k, 1e-300 AS v UNION ALL SELECT 'b', 2e-300"
	broken := "SELECT no_such_column FROM users"
	h.lr.NewProvider = func(runID string) llm.Provider {
		return llm.NewMock(llm.MustNewScript(
			llm.CallTools(
				sqlCall("e-"+runID, empty, 0),
				sqlCall("o-"+runID, oneCol, 0),
				sqlCall("n-"+runID, withNull, 0),
				sqlCall("c-"+runID, cut, 2),
				sqlCall("x-"+runID, huge, 0),
				sqlCall("s-"+runID, tiny, 0),
				sqlCall("f-"+runID, broken, 0),
			),
			llm.Say("这些结果不一定能画。"),
		))
	}
	h.rec.wrap(h.lr)
	ask(t, tt, "几种不能画的结果")
	tt.Frame()
	run2 := h.lastRun(t)
	rc2, _ := h.an.Charts("星禾", run2)
	if len(rc2.Tables) != 6 {
		t.Fatalf("记录=%+v", rc2)
	}
	// 失败的 run_sql 不进图表记录，窗口里也不多出一行原因。
	if obs := h.rec.observation(t, "f-"+run2); !strings.Contains(obs, "sql_rejected") {
		t.Fatalf("前提不成立，坏 SQL 没有失败：%s", obs)
	}
	for _, tc := range rc2.Tables {
		if tc.CallID == "f-"+run2 {
			t.Fatalf("失败的 run_sql 进了图表记录：%+v", tc)
		}
	}
	// 数值能解析但量级极端：记录里有序列，窗口只给 chart_not_plottable，不画坐标。
	for _, tc := range rc2.Tables[4:] {
		_, _, reason := barScale(seriesValues(tc.Series))
		if tc.Series == nil || reason == "" {
			t.Fatalf("%s 前提不成立：%+v %q", tc.CallID, tc.Series, reason)
		}
		line := session.SeriesLine(session.SQLChart{Code: session.CodeSeriesNotPlottable, Reason: reason})
		if !tt.HasText(line) {
			t.Fatalf("%s 没有给出 %q：%q", tc.CallID, line, tt.Texts())
		}
		if _, found := tt.Find(seriesLabel(tc.CallID)); found {
			t.Fatalf("%s 量级极端还画了图表区", tc.CallID)
		}
	}
	refusals := 0
	for _, s := range tt.Texts() {
		if strings.HasPrefix(s, "结果不可绘图：") {
			refusals++
		}
	}
	if refusals != 5 {
		t.Fatalf("应有 5 行不可绘图原因，实际 %d：%q", refusals, tt.Texts())
	}
	want := map[string]string{
		"e-" + run2: "结果不可绘图：查询结果为空（chart_empty）",
		"o-" + run2: "结果不可绘图：结果只有一列，缺少分类列或数值列（chart_missing_field）",
		"n-" + run2: "结果不可绘图：第 1 行的 n 不是数字（chart_not_plottable）",
	}
	for _, tc := range rc2.Tables {
		line, refused := want[tc.CallID]
		if !refused {
			continue
		}
		if tc.Series != nil || !tt.HasText(line) {
			t.Fatalf("%s 应只给原因 %q：series=%+v texts=%q", tc.CallID, line, tc.Series, tt.Texts())
		}
		if _, found := tt.Find(seriesLabel(tc.CallID)); found {
			t.Fatalf("%s 不该有图表区", tc.CallID)
		}
	}
	cutRec := rc2.Tables[3]
	if cutRec.CallID != "c-"+run2 || cutRec.Series == nil || !cutRec.Series.Truncated || len(cutRec.Series.Values) != 2 {
		t.Fatalf("截断结果=%+v", cutRec)
	}
	if !tt.HasText("结果已截断（limit），图只含返回的 2 行") {
		t.Fatalf("截断没有标出：%q", tt.Texts())
	}
	findScrolled(t, tt, seriesLabel(cutRec.CallID))

	// 运行没有完成：run_sql 成功，但脚本随后耗尽，不画柱。
	h.lr.NewProvider = func(runID string) llm.Provider {
		return llm.NewMock(llm.MustNewScript(llm.CallTools(sqlCall("h-"+runID, session.ChartDemoSQL, 0))))
	}
	h.rec.wrap(h.lr)
	ask(t, tt, "画到一半")
	tt.Frame()
	m := latestAssistant(h.s.state)
	if m.Status == events.DoneCompleted {
		t.Fatalf("脚本耗尽的运行不该是 completed：%+v", m)
	}
	if rc3, _ := h.an.Charts("星禾", m.RunID); len(rc3.Tables) != 1 || rc3.Tables[0].Series == nil {
		t.Fatalf("前提不成立：%+v", rc3)
	}
	if _, found := tt.Find(seriesLabel("h-" + m.RunID)); found || !tt.HasText("运行没有完成，不展示这次的图表。") {
		t.Fatal("未完成的运行画了结构化图表")
	}
}

// 值域溢出或刻度长到挤掉绘图区时不给坐标；常见量级照画，柱都落在纵轴内。
func TestBarScaleRefusesExtremes(t *testing.T) {
	for _, tc := range []struct {
		vs     []float64
		reason string
	}{
		{[]float64{1e308, -1e308}, "数值范围太大，纵轴无法取整"},
		{[]float64{1.7e308, 1}, "纵轴刻度 308 位，绘图区放不下"},
		{[]float64{-1.7e308}, "纵轴刻度 310 位，绘图区放不下"},
		{[]float64{1e-300, 2e-300}, "纵轴刻度 304 位，绘图区放不下"},
		{[]float64{1e60, 2e60}, "纵轴刻度 60 位，绘图区放不下"},
	} {
		if _, ticks, reason := barScale(tc.vs); reason != tc.reason || ticks != nil {
			t.Fatalf("%v 应拒画并给出 %q：%q ticks=%v", tc.vs, tc.reason, reason, ticks)
		}
	}
	for _, vs := range [][]float64{
		{14002311, 12104587},
		{-5, 3},
		{1e-15, 3e-15},
		{1e15, 9e15},
		{0, 0},
	} {
		y, ticks, reason := barScale(vs)
		if reason != "" || len(ticks) < 2 {
			t.Fatalf("%v 应能画：%q %v", vs, reason, ticks)
		}
		for _, v := range vs {
			if at := y.At(v); at < 0 || at > 1 {
				t.Fatalf("%v 的 %v 落在纵轴外：%v", vs, v, at)
			}
		}
	}
}

// axisAt 读出窗口里纵轴的刻度文字，按它们的位置把数值换成窗口 Y 坐标。
// 只用画出来的刻度，不复用被测的比例尺。
func axisAt(t *testing.T, tt *mygoui.Tester, area mygoui.Rect) func(float64) float64 {
	t.Helper()
	type mark struct{ v, y float64 }
	var lo, hi *mark
	for _, s := range tt.Texts() {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		r, _ := tt.Find(s)
		cy := float64(r.Y + r.H/2)
		if r.X+r.W > area.X+1 || cy < float64(area.Y)-2 || cy > float64(area.Y+area.H)+2 {
			continue
		}
		m := &mark{v, cy}
		if lo == nil || v < lo.v {
			lo = m
		}
		if hi == nil || v > hi.v {
			hi = m
		}
	}
	if lo == nil || hi == nil || lo.v == hi.v {
		t.Fatalf("图表区 %+v 左侧读不到两个刻度：%q", area, tt.Texts())
	}
	return func(v float64) float64 { return lo.y + (v-lo.v)/(hi.v-lo.v)*(hi.y-lo.y) }
}

// findScrolled 滚动对话区，直到 label 整个落在对话滚动区内，返回它的位置。
// 滚动区下边是提问栏，被它裁掉的元素仍在窗口里，但点击会落到提问栏上。
func findScrolled(t *testing.T, tt *mygoui.Tester, label string) mygoui.Rect {
	t.Helper()
	input, ok := tt.Find("提问框")
	if !ok {
		t.Fatal("找不到提问框")
	}
	const top = 48
	bottom := input.Y - 16
	r, found := tt.Find(label)
	inView := func() bool { return found && r.H > 0 && r.Y >= top && r.Y+r.H <= bottom }
	for i := 0; !inView() && i < 120; i++ {
		dy := float32(60)
		if found && r.Y < top {
			dy = -60
		}
		tt.Wheel(600, 360, 0, dy)
		tt.Advance(300 * time.Millisecond)
		tt.Frame()
		r, found = tt.Find(label)
	}
	if !inView() {
		t.Fatalf("对话区里看不全 %q：%+v（可见范围 %v..%v）%q", label, r, top, bottom, tt.Texts())
	}
	return r
}
