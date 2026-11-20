//go:build linux && amd64

package ui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	mygoui "github.com/egoist/mygo/ui"
	"github.com/loveRyujin/lantern/core/events"
	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/pyrt"
	"github.com/loveRyujin/lantern/core/pythonx"
	"github.com/loveRyujin/lantern/core/sandbox"
	"github.com/loveRyujin/lantern/internal/ui/session"
)

// 真实链路：种子库 → 本运行的 run_sql → 已准备的 pandas/Agg 在 Linux 沙箱里出图 →
// Session 保留字节 → 窗口经 Analysis.Fetch 取副本画出来。测试不注入任何图表字节。
func TestChartRealSandboxSuccessFailureRecovery(t *testing.T) {
	if os.Getenv("SANDBOX_REAL_PACKAGES") != "1" {
		t.Skip("真实出图需要 SANDBOX_REAL_PACKAGES=1；默认测试不安装依赖")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	rt, err := (pyrt.Config{Root: t.TempDir()}).Ensure(ctx)
	if err != nil {
		t.Fatalf("需要真实 Python 运行时：%v", err)
	}
	h := newChartHarness(t, rt)
	h.an.KeepOpen = 4
	tt := h.tt
	demo := h.lr.NewProvider

	// 成功：场景 1 在星禾跑完，图表画在这条回答下。
	ask(t, tt, session.ChartDemoPrompt)
	tt.Frame()
	run1 := h.lastRun(t)
	if msg := latestAssistant(h.s.state); msg.Status != events.DoneCompleted {
		t.Fatalf("场景 1 没有完成：%+v", msg)
	}
	rc, ok := h.an.Charts("星禾", run1)
	if !ok || len(rc.Failures) != 0 || len(rc.Charts) != 1 {
		t.Fatalf("记录=%+v", rc)
	}
	chart := rc.Charts[0]
	manifest := pythonManifest(t, h.rec.observation(t, "py-"+run1))
	if len(manifest.Artifacts) != 1 || manifest.Artifacts[0].ID != chart.ArtifactID || chart.CallID != "py-"+run1 ||
		manifest.Artifacts[0].Bytes != chart.Bytes || chart.Name != "gmv_east.png" {
		t.Fatalf("界面记录与模型看到的 run_python 清单不一致：chart=%+v manifest=%+v", chart, manifest)
	}
	if !strings.Contains(manifest.Stdout, "2026-09=121045.87") {
		t.Fatalf("Python 没有用到 SQL 的真实结果：%q", manifest.Stdout)
	}
	if chart.SQL != session.ChartDemoSQL || chart.SQLCallID != "sql-"+run1 || chart.Code != session.ChartDemoCode {
		t.Fatalf("图表没有带上本运行的 SQL 与代码：%+v", chart)
	}
	label := chartLabel(chart)
	first := assertChartDrawn(t, h, tt, "星禾", run1, chart)
	var seen struct {
		Rows [][]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(observationInner(t, "run_sql", h.rec.observation(t, "sql-"+run1))), &seen); err != nil {
		t.Fatal(err)
	}
	if len(rc.Tables) != 1 || rc.Tables[0].Series == nil || rc.Tables[0].CallID != "sql-"+run1 || len(seen.Rows) != 2 {
		t.Fatalf("场景 1 没有结构化图表：%+v", rc.Tables)
	}
	for i, row := range seen.Rows {
		if sr := rc.Tables[0].Series; sr.Labels[i] != row[0] || sr.Values[i] != row[1] {
			t.Fatalf("结构化图表第 %d 点 %s=%v，run_sql 结果是 %v", i, sr.Labels[i], sr.Values[i], row)
		}
	}
	findScrolled(t, tt, seriesLabel("sql-"+run1))
	findScrolled(t, tt, barLabel("2026-09", "12104587"))

	trigger := "SQL 与代码 · " + chart.CallID
	if tt.HasText("SUM(o.amount_cents)") {
		t.Fatal("SQL 默认就展开了")
	}
	findScrolled(t, tt, trigger)
	if err := tt.Click(trigger); err != nil {
		t.Fatal(err)
	}
	tt.Advance(300 * time.Millisecond)
	tt.Frame()
	if !tt.HasText("SUM(o.amount_cents)") || !tt.HasText("gmv_east.png") || !tt.HasText("SQL · sql-"+run1) {
		t.Fatalf("展开后看不到 SQL 和代码：%q", tt.Texts())
	}

	// 恢复：切到云杉看不到星禾的图；切回后重新从 Session 取，字节一致。
	if err := tt.Click("云杉"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if _, found := tt.Find(label); found || len(h.s.chartViews) != 0 {
		t.Fatalf("切到云杉还留着星禾的图表：%q", tt.Texts())
	}
	if _, err := h.an.Fetch("云杉", run1, chart.ArtifactID); code(err) != pythonx.CodeScope {
		t.Fatalf("从云杉取星禾的图 err=%v", err)
	}
	if err := tt.Click("星禾"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if again := assertChartDrawn(t, h, tt, "星禾", run1, chart); again != first {
		t.Fatalf("切回后取到的字节变了：%s != %s", again, first)
	}

	// 失败：Python 报错时这条回答只显示分类错误，不显示上一条的图。
	h.lr.NewProvider = scriptedChart(`raise ValueError("controlled-chart-error")`)
	h.rec.wrap(h.lr)
	ask(t, tt, "再画一次")
	tt.Frame()
	run2 := h.lastRun(t)
	rc2, _ := h.an.Charts("星禾", run2)
	if len(rc2.Charts) != 0 || len(rc2.Failures) != 1 || rc2.Failures[0].Code != "error" {
		t.Fatalf("失败运行记录=%+v", rc2)
	}
	if !tt.HasText("Python 失败 · 代码执行出错（error）：Python 执行失败，请检查代码和输入") || tt.HasText("controlled-chart-error") {
		t.Fatalf("失败提示不对或漏出了 traceback：%q", tt.Texts())
	}
	if n := countLabels(tt, "图表 "); n != 1 {
		t.Fatalf("应只剩第一问的那张图，实际 %d 张：%q", n, tt.Texts())
	}

	// 失败：run_python 已出图，但运行没有以 completed 结束时不取也不画。
	h.lr.NewProvider = func(runID string) llm.Provider {
		sqlArgs, _ := json.Marshal(map[string]string{"sql": session.ChartDemoSQL})
		pyArgs, _ := json.Marshal(map[string]string{"code": session.ChartDemoCode, "sql_call_id": "sql-" + runID})
		return llm.NewMock(llm.MustNewScript(
			llm.CallTools(llm.ToolCall{ID: "sql-" + runID, Name: "run_sql", Arguments: string(sqlArgs)}),
			llm.CallTools(llm.ToolCall{ID: "py-" + runID, Name: "run_python", Arguments: string(pyArgs)}),
		))
	}
	h.rec.wrap(h.lr)
	ask(t, tt, "画到一半")
	tt.Frame()
	runHalf := h.lastRun(t)
	if msg := latestAssistant(h.s.state); msg.Status == events.DoneCompleted {
		t.Fatalf("脚本耗尽的运行不该是 completed：%+v", msg)
	}
	if rcHalf, _ := h.an.Charts("星禾", runHalf); len(rcHalf.Charts) != 1 {
		t.Fatalf("前提不成立，run_python 应已成功出图：%+v", rcHalf)
	}
	for k := range h.s.chartViews {
		if strings.Contains(k, runHalf) {
			t.Fatalf("未完成的运行取了图：%q", k)
		}
	}
	if !tt.HasText("运行没有完成，不展示这次的图表。") || countLabels(tt, "图表 ") != 1 {
		t.Fatalf("未完成的运行画了图：%q", tt.Texts())
	}

	// 不支持的格式和超限：同一运行出 PNG 与 CSV，界面上限压低后两件都给出占位，不去读字节。
	h.an.MaxChartBytes = 1000
	h.lr.NewProvider = scriptedChart(`import os
import matplotlib.pyplot as plt
fig, ax = plt.subplots()
ax.bar(['a', 'b'], [1, 2])
fig.savefig(os.environ['OUTPUT_DIR'] + '/big.png')
open(os.environ['OUTPUT_DIR'] + '/rows.csv', 'w').write('a,b\n1,2\n')
`)
	h.rec.wrap(h.lr)
	ask(t, tt, "导出图和表")
	tt.Frame()
	run3 := h.lastRun(t)
	rc3, _ := h.an.Charts("星禾", run3)
	if len(rc3.Charts) != 2 {
		t.Fatalf("产物记录=%+v", rc3)
	}
	for _, ch := range rc3.Charts {
		v := h.s.chartViews[chartKey("星禾", run3, ch.ArtifactID)]
		if v == nil || v.bitmap != nil || v.sum != "" {
			t.Fatalf("%s 不该被画出：%+v", ch.Name, v)
		}
		want := map[string]string{"big.png": "（chart_too_large）", "rows.csv": "产物格式 csv 不能作为图表显示（chart_format_unsupported）"}[ch.Name]
		if !strings.HasSuffix(v.line, want) || !tt.HasText(v.line) {
			t.Fatalf("%s 占位=%q 期望结尾 %q", ch.Name, v.line, want)
		}
	}
	h.an.MaxChartBytes = 0
	h.s.dropCharts()
	tt.Frame()
	for _, ch := range rc3.Charts {
		if ch.Kind == sandbox.KindPNG {
			assertChartDrawn(t, h, tt, "星禾", run3, ch)
		}
	}

	// Session 关闭：再问一次，最早的运行超出 KeepOpen 被关闭。不切换工作区也立刻丢掉缓存的图，切回后同样提示不可取回。
	h.lr.NewProvider = demo
	h.rec.wrap(h.lr)
	ask(t, tt, session.ChartDemoPrompt)
	tt.Frame()
	closed := "图表 gmv_east.png 不可显示：分析会话已关闭，产物不可取回（analysis_session_closed）"
	if v := h.s.chartViews[chartKey("星禾", run1, chart.ArtifactID)]; v == nil || v.bitmap != nil || !tt.HasText(closed) {
		t.Fatalf("Session 淘汰后仍显示缓存的旧图：%+v %q", v, tt.Texts())
	}
	if err := tt.Click("云杉"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("星禾"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if rc, _ := h.an.Charts("星禾", run1); !rc.Closed {
		t.Fatal("第一问的 Session 没有被关闭")
	}
	if !tt.HasText(closed) {
		t.Fatalf("缺少关闭提示：%q", tt.Texts())
	}
	if v := h.s.chartViews[chartKey("星禾", run1, chart.ArtifactID)]; v == nil || v.bitmap != nil {
		t.Fatalf("关闭后仍画出了旧图：%+v", v)
	}
	if n := countLabels(tt, "图表 gmv_east.png · "); n != 1 {
		t.Fatalf("最新一问应画出 1 张场景 1 图表，实际 %d", n)
	}
}

// assertChartDrawn 核对窗口里的图来自 Session：缓存的字节哈希等于再取一次的副本，
// PNG 尺寸和清单一致，且画面上那块区域确实有图。
func assertChartDrawn(t *testing.T, h *chartHarness, tt *mygoui.Tester, ws, runID string, ch session.Chart) string {
	t.Helper()
	v := h.s.chartViews[chartKey(ws, runID, ch.ArtifactID)]
	if v == nil || v.bitmap == nil {
		t.Fatalf("图表没有画出：%+v 现有 %q", v, tt.Texts())
	}
	art, err := h.an.Fetch(ws, runID, ch.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(art.Data)
	if hex.EncodeToString(sum[:]) != v.sum || len(art.Data) != ch.Bytes {
		t.Fatalf("窗口里的字节不是 Session 的副本")
	}
	img, err := png.Decode(bytes.NewReader(art.Data))
	if err != nil {
		t.Fatal(err)
	}
	if w, hgt := v.bitmap.Size(); w != img.Bounds().Dx() || hgt != img.Bounds().Dy() {
		t.Fatalf("位图尺寸 %dx%d 与 PNG %v 不一致", w, hgt, img.Bounds())
	}
	r, found := tt.Find(chartLabel(ch))
	for i := 0; found && r.H < 200 && i < 60; i++ {
		tt.Wheel(600, 360, 0, 150)
		tt.Advance(300 * time.Millisecond)
		tt.Frame()
		r, found = tt.Find(chartLabel(ch))
	}
	if !found || r.W < 100 || r.H < 50 {
		t.Fatalf("窗口里找不到图表 %q：%+v", chartLabel(ch), r)
	}
	frame := tt.Image()
	colors := map[[3]uint8]bool{}
	scale := float32(frame.Bounds().Dx()) / 1080
	for y := r.Y; y < r.Y+r.H; y += 4 {
		for x := r.X; x < r.X+r.W; x += 4 {
			c := frame.RGBAAt(int(x*scale), int(y*scale))
			colors[[3]uint8{c.R, c.G, c.B}] = true
		}
	}
	if len(colors) < 8 {
		t.Fatalf("图表区域几乎是纯色，没画出来：%d 种颜色", len(colors))
	}
	return v.sum
}

func scriptedChart(code string) func(string) llm.Provider {
	return func(runID string) llm.Provider {
		sqlArgs, _ := json.Marshal(map[string]string{"sql": session.ChartDemoSQL})
		pyArgs, _ := json.Marshal(map[string]string{"code": code, "sql_call_id": "sql-" + runID})
		return llm.NewMock(llm.MustNewScript(
			llm.CallTools(llm.ToolCall{ID: "sql-" + runID, Name: "run_sql", Arguments: string(sqlArgs)}),
			llm.CallTools(llm.ToolCall{ID: "py-" + runID, Name: "run_python", Arguments: string(pyArgs)}),
			llm.Say("结论见下。"),
		))
	}
}

type pythonObservation struct {
	Stdout    string             `json:"stdout"`
	Artifacts []pythonx.Manifest `json:"artifacts"`
}

func pythonManifest(t *testing.T, wrapped string) pythonObservation {
	t.Helper()
	var obs pythonObservation
	if err := json.Unmarshal([]byte(observationInner(t, "run_python", wrapped)), &obs); err != nil {
		t.Fatalf("run_python 观察值不是清单 JSON：%s", wrapped)
	}
	return obs
}

func countLabels(tt *mygoui.Tester, prefix string) int {
	n := 0
	for _, s := range tt.Texts() {
		if strings.HasPrefix(s, prefix) && !strings.Contains(s, "不可显示") {
			n++
		}
	}
	return n
}
