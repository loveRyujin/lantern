package ui

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mygoui "github.com/egoist/mygo/ui"
	"github.com/loveRyujin/lantern/core/run"
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
	// 工具步骤显示调用参数（run_steps 里已有的参数原文，转义后的纯文本）和结果摘要，
	// 不只显示哈希。提问原文不进轨迹。
	var params string
	for _, text := range tt.Texts() {
		if strings.HasPrefix(text, "参数 {") {
			params = text
		}
	}
	if !strings.Contains(params, `"object":"**不要执行**\n`+"```sql"+`\nDROP TABLE users\n`+"```"+`"`) {
		t.Fatalf("工具参数没有显示：%q", tt.Texts())
	}
	for _, text := range tt.Texts() {
		if strings.Contains(text, "导出客户") || strings.Contains(text, "再导出") {
			t.Fatalf("轨迹里出现了提问原文：%q", text)
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
	for _, want := range append(ordered, "冻结参数哈希 "+hash, "    参数 {", `**不要执行**\n`, "    结果 已执行") {
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

// 轨迹入口严格只读：窗口、会话层和命令行查看一个没升级的工作区时，只提示需升级，
// 不补迁移、不跑 Recover，库文件逐字节不变，也不把缺表当成空记录。
func TestTraceEntriesStayReadOnlyOnOutdatedWorkspace(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h, err := workspace.Create(ctx, root, "星禾")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, q := range []string{
		`INSERT INTO runs (id, state, version, context, created_at, updated_at) VALUES ('run-live', 'running', 0, '', ?1, ?1)`,
		`INSERT INTO run_steps (id, run_id, ordinal, status, tool_name, created_at, updated_at) VALUES ('step-1', 'run-live', 0, 'executing', 'export_data', ?1, ?1)`,
	} {
		if _, err := h.Runs.ExecContext(ctx, q, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := newShell(ctx, Options{Root: root, Workspaces: []session.Workspace{{ID: "星禾"}}, Runner: session.ScriptedRunner{}})
	if err != nil {
		t.Fatal(err)
	}

	// 窗口已经开着，再把库降回迁移 006 之前，模拟一个还没升级的工作区。
	path := filepath.Join(root, "workspaces", "星禾", "runs.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP TRIGGER runs_record_created`, `DROP TRIGGER runs_record_transition`,
		`DROP TRIGGER run_transitions_forbid_update`, `DROP TRIGGER run_transitions_forbid_delete`,
		`DROP TRIGGER run_events_forbid_update`, `DROP TRIGGER run_events_forbid_delete`,
		`DROP TABLE run_transitions`, `DELETE FROM schema_migrations WHERE version = 6`,
	} {
		if _, err := raw.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const upgrade = "工作区 星禾 需升级后查看"

	tt := traceTester(s)
	tt.Frame()
	if err := tt.Click("运行轨迹"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText(upgrade) || tt.HasText("还没有运行") {
		t.Fatalf("窗口最近运行：%q", tt.Texts())
	}
	queryTrace(t, tt, "run-live")
	if !tt.HasText(upgrade) || tt.HasText("没有运行 run-live") || tt.HasText("状态 运行中") {
		t.Fatalf("窗口查询：%q", tt.Texts())
	}
	if _, err := session.ListRuns(ctx, root, "星禾", 10); !errors.Is(err, run.ErrTraceSchema) {
		t.Fatalf("session.ListRuns err=%v", err)
	}
	var out bytes.Buffer
	if err := WriteTrace(ctx, &out, root, "星禾", "run-live"); err == nil || !strings.Contains(err.Error(), upgrade) || out.Len() != 0 {
		t.Fatalf("命令行 err=%v out=%q", err, out.String())
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("查看轨迹改动了 runs.db")
	}
	db := openRunsRO(t, root, "星禾")
	defer db.Close()
	var tables, version, audits int
	var state, step string
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'run_transitions'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT r.state, s.status FROM runs r JOIN run_steps s ON s.run_id = r.id WHERE r.id = 'run-live'`).Scan(&state, &step); err != nil {
		t.Fatal(err)
	}
	if tables != 0 || version != 5 || audits != 0 || state != "running" || step != "executing" {
		t.Fatalf("查看后被补迁移或恢复：tables=%d version=%d audits=%d state=%s step=%s", tables, version, audits, state, step)
	}

	// 目录不存在报不存在；目录在而 runs.db 不在报需升级，都不显示成空记录。
	if err := WriteTrace(ctx, &out, root, "雪松", "run-live"); err == nil || !strings.Contains(err.Error(), "工作区 雪松 不存在") {
		t.Fatalf("不存在的工作区 err=%v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "workspaces", "空壳"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteTrace(ctx, &out, root, "空壳", "run-live"); err == nil || !strings.Contains(err.Error(), "工作区 空壳 需升级后查看") {
		t.Fatalf("缺 runs.db err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "workspaces", "空壳", "runs.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("查看轨迹创建了 runs.db：%v", err)
	}
}

// 轨迹入口不经过会补迁移或恢复的路径。
func TestTraceEntriesNeverOpenWritablePaths(t *testing.T) {
	for _, file := range []string{"trace.go", filepath.Join("session", "trace.go")} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var code strings.Builder
		for _, line := range strings.Split(string(src), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "//") {
				code.WriteString(line + "\n")
			}
		}
		for _, banned := range []string{"workspace.Open", "OpenPrepared", "workspace.Create", ".Recover(", ".Resume(", "mode=rw"} {
			if strings.Contains(code.String(), banned) {
				t.Fatalf("%s 引用了 %s", file, banned)
			}
		}
	}
}
