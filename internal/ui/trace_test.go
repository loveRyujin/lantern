package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	mygoui "github.com/egoist/mygo/ui"
	"github.com/loveRyujin/lantern/core/workspace"
	"github.com/loveRyujin/lantern/internal/ui/session"
)

// 演示运行器走真实 agent.Loop：确认单、步骤、事件和审计都由 Core 写进 runs.db。
// 轨迹视图只从库里只读查询，测试不写任何轨迹行。
func TestTraceViewShowsRealLoopRunAndRestart(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	for _, id := range []string{"星禾", "云杉"} {
		h, err := workspace.Create(ctx, root, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
	}
	wss := []session.Workspace{{ID: "星禾"}, {ID: "云杉"}}
	runner := session.NewDemoRunner(root, session.NewWaiter())
	s, err := newShell(ctx, Options{Root: root, Workspaces: wss, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	tool := runner.Tool.(*session.CountTool)
	tt := traceTester(s)

	approved, _, approvedDone := askUntilDialog(t, tt, s, runner, "星禾", "导出客户")
	if err := tt.Click("批准"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tt, func() bool { return tool.N() == 1 && askFinished(s, approved.RunID) })
	deferred, _, deferredDone := askUntilDialog(t, tt, s, runner, "星禾", "再导出")
	tt.Key(0, mygoui.KeyEnter)
	waitAskDone(t, tt, s, deferred.RunID, session.StatusAwaiting)
	<-approvedDone
	<-deferredDone
	if tool.N() != 1 {
		t.Fatalf("暂缓后执行了 %d 次", tool.N())
	}
	_, hash, _ := approvalOf(t, root, "星禾", approved.RunID)

	// 成功：从回答下方的按钮进入，按时间显示状态迁移、模型调用、确认事件和工具步骤。
	tt.Frame()
	if err := tt.Click("查看轨迹"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	ordered := []string{
		"状态 创建为 已创建",
		"状态 已创建 → 运行中",
		"模型调用 · 成功 · 请求工具 export_data",
		"确认请求 · export_data",
		"确认决定 · 批准 · export_data",
		"确认消费 · 执行前核销 · export_data",
		"工具 export_data · 完成",
		"模型调用 · 成功 · 最终回答",
		"状态 运行中 → 已完成",
	}
	assertInOrder(t, tt, ordered)
	for _, want := range []string{
		"运行 " + approved.RunID + " · 状态 已完成",
		"冻结参数哈希 " + hash,
		"参数哈希 " + hash,
		"结果 已执行",
	} {
		if !tt.HasText(want) {
			t.Fatalf("缺少 %q，现有 %q", want, tt.Texts())
		}
	}
	// 参数里的 Markdown 和代码块按原文显示，没有被渲染。
	if !tt.HasText(`**不要执行**\n`+"```sql") || !tt.HasText("DROP TABLE users") {
		t.Fatalf("参数没有按纯文本显示：%q", tt.Texts())
	}
	for _, text := range tt.Texts() {
		if text == "不要执行" || text == "DROP TABLE users" {
			t.Fatalf("参数被当成 Markdown 渲染：%q", tt.Texts())
		}
	}
	if tool.N() != 1 {
		t.Fatal("查看轨迹触发了工具")
	}

	// 失败：切到另一个工作区查同一个 run_id，只给出不存在，不显示任何内容。
	if err := tt.Click("云杉"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText("运行轨迹 · 云杉") || !tt.HasText("还没有运行") || tt.HasText(approved.RunID) {
		t.Fatalf("切换工作区后仍显示上一区：%q", tt.Texts())
	}
	queryTrace(t, tt, approved.RunID)
	if !tt.HasText("工作区 云杉 里没有运行 "+approved.RunID) || tt.HasText(hash) || tt.HasText("确认请求") {
		t.Fatalf("跨工作区查询：%q", tt.Texts())
	}
	queryTrace(t, tt, "run-nope")
	if !tt.HasText("工作区 云杉 里没有运行 run-nope") {
		t.Fatalf("未知运行：%q", tt.Texts())
	}

	// 恢复：重开窗口（新进程只读库，不跑 Recover），从最近运行里找到历史 run。
	restarted, err := newShell(ctx, Options{Root: root, Workspaces: wss, Runner: session.NewDemoRunner(root, session.NewWaiter())})
	if err != nil {
		t.Fatal(err)
	}
	rt := traceTester(restarted)
	rt.Frame()
	if err := rt.Click("运行轨迹"); err != nil {
		t.Fatal(err)
	}
	rt.Frame()
	if err := rt.Click(deferred.RunID + " · awaiting_approval"); err != nil {
		t.Fatalf("%v：%q", err, rt.Texts())
	}
	rt.Frame()
	for _, want := range []string{"状态 等待确认", "等待确认：运行仍挂起", "当前 等待决定", "状态 运行中 → 等待确认"} {
		if !rt.HasText(want) {
			t.Fatalf("待确认运行缺少 %q：%q", want, rt.Texts())
		}
	}
	if rt.HasText("确认决定") || rt.HasText("工具 export_data · 完成") {
		t.Fatalf("待确认运行显示了未发生的步骤：%q", rt.Texts())
	}
	if err := rt.Click(approved.RunID + " · completed"); err != nil {
		t.Fatal(err)
	}
	rt.Frame()
	assertInOrder(t, rt, ordered)
	if err := rt.Click("返回对话"); err != nil {
		t.Fatal(err)
	}
	rt.Frame()
	if rt.HasText("运行轨迹 · ") || !rt.HasText("在这个工作区里提问") {
		t.Fatalf("没有回到对话：%q", rt.Texts())
	}

	var out bytes.Buffer
	if err := WriteTrace(ctx, &out, root, "星禾", approved.RunID); err != nil {
		t.Fatal(err)
	}
	cli := out.String()
	for _, want := range append(ordered, "冻结参数哈希 "+hash, `**不要执行**\n`) {
		if !strings.Contains(cli, want) {
			t.Fatalf("命令行缺少 %q：\n%s", want, cli)
		}
	}
	if err := WriteTrace(ctx, &out, root, "云杉", approved.RunID); err == nil || !strings.Contains(err.Error(), "没有运行") || strings.Contains(err.Error(), hash) {
		t.Fatalf("命令行跨工作区 err=%v", err)
	}
	if tool.N() != 1 {
		t.Fatal("重启后查看轨迹触发了工具")
	}
}

func traceTester(s *shell) *mygoui.Tester {
	return mygoui.NewTester(s.view, 1280, 2400)
}

func queryTrace(t *testing.T, tt *mygoui.Tester, runID string) {
	t.Helper()
	if err := tt.Click("轨迹运行标识"); err != nil {
		t.Fatal(err)
	}
	for range 64 {
		tt.Key(0, mygoui.KeyBackspace)
	}
	tt.Type(runID)
	tt.Frame()
	if err := tt.Click("查询轨迹"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
}

func assertInOrder(t *testing.T, tt *mygoui.Tester, wants []string) {
	t.Helper()
	texts := tt.Texts()
	at := 0
	for _, want := range wants {
		found := false
		for at < len(texts) {
			if strings.Contains(texts[at], want) {
				found = true
				at++
				break
			}
			at++
		}
		if !found {
			t.Fatalf("没有按顺序找到 %q，现有 %q", want, tt.Texts())
		}
	}
}

func askFinished(s *shell, runID string) bool {
	th := s.state.Thread(s.state.ActiveID())
	for _, m := range th.Messages {
		if m.RunID == runID && m.Done {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, tt *mygoui.Tester, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		tt.Frame()
		if time.Now().After(deadline) {
			t.Fatalf("等待超时：%q", tt.Texts())
		}
		time.Sleep(5 * time.Millisecond)
	}
	tt.Frame()
}

func approvalOf(t *testing.T, root, ws, runID string) (id, hash, decision string) {
	t.Helper()
	db := openRunsRO(t, root, ws)
	defer db.Close()
	if err := db.QueryRowContext(t.Context(), `SELECT id, params_hash, decision FROM approvals WHERE run_id = ?`, runID).Scan(&id, &hash, &decision); err != nil {
		t.Fatal(err)
	}
	return id, hash, decision
}
