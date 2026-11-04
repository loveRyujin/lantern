package eval

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/loveRyujin/lantern/core/agent"
	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/audit"
	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/policy"
	"github.com/loveRyujin/lantern/core/run"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

const confirmRunID = "run-1"

// readApproverScript 读取用例目录下的脚本。文件不存在时返回 nil，调用方只用用例里的动作。
func readApproverScript(root string) (map[string]string, error) {
	if root == "" {
		return nil, nil
	}
	path := filepath.Join(root, "contracts", "approver-script.json")
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("eval: 读取确认脚本: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("eval: 确认脚本不是普通文件: %s", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: 读取确认脚本: %w", err)
	}
	var file struct {
		Scripts []struct {
			Action string `json:"action"`
			CaseID string `json:"case_id"`
		} `json:"scripts"`
	}
	if err := json.Unmarshal(body, &file); err != nil {
		return nil, fmt.Errorf("eval: 确认脚本不是合法 JSON: %w", err)
	}
	if len(file.Scripts) == 0 {
		return nil, fmt.Errorf("eval: 确认脚本没有条目")
	}
	out := make(map[string]string, len(file.Scripts))
	for _, script := range file.Scripts {
		if script.CaseID == "" || script.Action == "" {
			return nil, fmt.Errorf("eval: 确认脚本缺少 case_id 或 action")
		}
		if _, exists := out[script.CaseID]; exists {
			return nil, fmt.Errorf("eval: 确认脚本重复用例 %s", script.CaseID)
		}
		out[script.CaseID] = script.Action
	}
	return out, nil
}

// confirmationPlan 核对脚本动作。跳过原因非空时不执行。
// 脚本文件不存在时只信用例里的 approver_action。
func confirmationPlan(item Case, scripts map[string]string) (string, error) {
	action, ok := inputString(item.Input, "approver_action")
	if !ok || action == "" {
		return "", fmt.Errorf("confirmation %s: 缺少 approver_action", item.ID)
	}
	if scripts != nil {
		want, exists := scripts[item.ID]
		if !exists {
			return "", fmt.Errorf("confirmation %s: approver-script.json 没有这条用例", item.ID)
		}
		if want != action {
			return "", fmt.Errorf("confirmation %s: 脚本动作 %s 与用例 %s 不一致", item.ID, want, action)
		}
	}
	if inputBool(item.Input, "eval_skip") || action == "restart_done" {
		reason, _ := inputString(item.Input, "eval_skip_reason")
		if reason == "" {
			reason = "W6-01 步骤间 done 断点续跑未实现，本轮不评分"
		}
		return reason, nil
	}
	return "", nil
}

// executeConfirmation 用脚本化 Approver 跑真实 Core 路径，返回观测。
// 它不读取 input.mock。
func executeConfirmation(ctx context.Context, item Case) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	action, _ := inputString(item.Input, "approver_action")
	task, _ := inputString(item.Input, "task")
	if task == "" {
		task = item.ID
	}
	obs, err := runConfirmationAction(ctx, action, task)
	if err != nil {
		return nil, fmt.Errorf("confirmation %s: %w", item.ID, err)
	}
	return obs, nil
}

func runConfirmationAction(ctx context.Context, action, task string) (map[string]any, error) {
	switch action {
	case "approve":
		if task == "确认提高 Python 超时" {
			return runLoopCase(ctx, loopCase{
				task: task, spec: tools.SpecRunPython,
				args:    `{"code":"print(1)","limits":{"wall_ms":45000}}`,
				name:    "run_python",
				approve: []approval.Response{{Action: approval.ActionApprove}},
				say:     true,
			})
		}
		if task == "确认下架一个商品" {
			return runLoopCase(ctx, loopCase{
				task: task, spec: tools.SpecWriteAction,
				args:    `{"action":"unpublish","product_ids":["sku-1"]}`,
				name:    "write_action",
				approve: []approval.Response{{Action: approval.ActionApprove}},
				say:     true,
			})
		}
		return runLoopCase(ctx, loopCase{
			task: task, spec: tools.SpecExportData,
			args:    `{"object":"contacts","fields":["phone"]}`,
			name:    "export_data",
			approve: []approval.Response{{Action: approval.ActionApprove}},
			say:     true,
		})
	case "reject":
		if task == "申请提高内存上限" {
			return runLoopCase(ctx, loopCase{
				task: task, spec: tools.SpecRunPython,
				args:    `{"code":"print(1)","limits":{"memory_bytes":805306368}}`,
				name:    "run_python",
				approve: []approval.Response{{Action: approval.ActionReject}},
			})
		}
		return runLoopCase(ctx, loopCase{
			task: task, spec: tools.SpecExportData,
			args:    `{"object":"contacts","fields":["phone"]}`,
			name:    "export_data",
			approve: []approval.Response{{Action: approval.ActionReject}},
		})
	case "timeout":
		return runLoopCase(ctx, loopCase{
			task: task, spec: tools.SpecExportData,
			args:    `{"object":"contacts","fields":["phone"]}`,
			name:    "export_data",
			approve: []approval.Response{{Wait: true}},
			ttl:     40 * time.Millisecond,
		})
	case "escape":
		return runLoopCase(ctx, loopCase{
			task: task, spec: tools.SpecExportData,
			args:    `{"object":"contacts"}`,
			name:    "export_data",
			approve: []approval.Response{{Action: approval.ActionDefer}},
		})
	case "hold":
		if task == "申请额外只读目录" {
			return runLoopCase(ctx, loopCase{
				task: task, spec: tools.SpecRunPython,
				args:    `{"code":"print(1)","extra_read_only":["/data/ro"]}`,
				name:    "run_python",
				approve: []approval.Response{{Action: approval.ActionDefer}},
			})
		}
		return runLoopCase(ctx, loopCase{
			task: task, spec: tools.SpecExportData,
			args:    `{"object":"contacts","fields":["email"]}`,
			name:    "export_data",
			approve: []approval.Response{{Action: approval.ActionDefer}},
		})
	case "tamper":
		return runLoopCase(ctx, loopCase{
			task: task, spec: tools.SpecExportData,
			args: `{"object":"contacts"}`,
			name: "export_data",
			approve: []approval.Response{{
				Action: approval.ActionApprove,
				Params: []byte(`{"object":"tampered"}`),
			}},
		})
	case "approve_twice":
		return runThen(ctx, loopCase{
			task: task, spec: tools.SpecExportData,
			args:    `{"object":"contacts"}`,
			name:    "export_data",
			approve: []approval.Response{{Action: approval.ActionApprove}},
			say:     true,
		}, func(ctx context.Context, box *caseBox) error {
			_, err := box.machine.Decide(ctx, box.ticketID, approval.Decision{Action: approval.ActionApprove})
			return err
		})
	case "approve_then_reject":
		return runThen(ctx, loopCase{
			task: task, spec: tools.SpecExportData,
			args:    `{"object":"contacts"}`,
			name:    "export_data",
			approve: []approval.Response{{Action: approval.ActionApprove}},
			say:     true,
		}, func(ctx context.Context, box *caseBox) error {
			_, err := box.machine.Decide(ctx, box.ticketID, approval.Decision{Action: approval.ActionReject})
			box.extraErr = err
			return nil
		})
	case "network_request":
		return runLoopCase(ctx, loopCase{
			task: task, spec: tools.SpecRunPython,
			args:    `{"code":"print(1)","network":true}`,
			name:    "run_python",
			approve: nil,
			say:     true,
		})
	case "restart_awaiting":
		return runThen(ctx, loopCase{
			task: task, spec: tools.SpecExportData,
			args:    `{"object":"contacts","fields":["phone"]}`,
			name:    "export_data",
			approve: []approval.Response{{Action: approval.ActionDefer}},
		}, func(ctx context.Context, box *caseBox) error {
			return box.reopen(ctx)
		})
	case "restart_executing":
		return runExecutingRestart(ctx)
	case "concurrent_approve":
		return runConcurrentApprove(ctx, task)
	case "approve_after_timeout":
		return runApproveAfterTimeout(ctx, task)
	default:
		return nil, fmt.Errorf("未知确认动作 %s", action)
	}
}

type loopCase struct {
	task    string
	spec    tools.Spec
	args    string
	name    string
	approve []approval.Response
	say     bool
	ttl     time.Duration
}

type caseBox struct {
	root     string
	ws       *workspace.Handle
	tool     *observeTool
	machine  *approval.Machine
	ticketID string
	outcome  agent.Outcome
	runErr   error
	extraErr error
}

func (b *caseBox) close() {
	if b == nil {
		return
	}
	if b.ws != nil {
		_ = b.ws.Close()
		b.ws = nil
	}
	if b.root != "" {
		_ = os.RemoveAll(b.root)
	}
}

func (b *caseBox) reopen(ctx context.Context) error {
	if b.ws != nil {
		if err := b.ws.Close(); err != nil {
			return err
		}
		b.ws = nil
	}
	h, err := workspace.Open(ctx, b.root, "ws-1")
	if err != nil {
		return err
	}
	b.ws = h
	b.machine = approval.New(h.Runs)
	return nil
}

func runLoopCase(ctx context.Context, spec loopCase) (map[string]any, error) {
	box, err := startLoop(ctx, spec)
	if err != nil {
		return nil, err
	}
	defer box.close()
	return box.observations(ctx)
}

func runThen(ctx context.Context, spec loopCase, tail func(context.Context, *caseBox) error) (map[string]any, error) {
	box, err := startLoop(ctx, spec)
	if err != nil {
		return nil, err
	}
	defer box.close()
	if err := tail(ctx, box); err != nil {
		return nil, err
	}
	return box.observations(ctx)
}

func startLoop(ctx context.Context, spec loopCase) (*caseBox, error) {
	box, err := newCaseBox(ctx)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			box.close()
		}
	}()
	if err := insertRun(ctx, box.ws.Runs, confirmRunID, run.StateCreated); err != nil {
		return nil, err
	}
	tool := &observeTool{spec: spec.spec}
	reg := &tools.Registry{}
	if err := reg.Register(tool); err != nil {
		return nil, err
	}
	steps := []llm.Step{llm.CallTools(llm.ToolCall{ID: "c1", Name: spec.name, Arguments: spec.args})}
	if spec.say {
		steps = append(steps, llm.Say("完成"))
	}
	machine := approval.New(box.ws.Runs)
	status, stepStore := agent.Persist(run.New(box.ws.Runs))
	ttl := spec.ttl
	total := 5 * time.Second
	if ttl > 0 && total < ttl*20 {
		total = ttl * 20
	}
	loop := &agent.Loop{
		Provider: llm.NewMock(llm.MustNewScript(steps...)),
		Tools:    reg,
		Policy:   policy.Default(),
		Approver: &approval.Fake{Responses: append([]approval.Response(nil), spec.approve...)},
		Tickets:  machine,
		Audit:    audit.NewSQLite(box.ws.Runs),
		Status:   status,
		Steps:    stepStore,
		Config: agent.Config{
			MaxSteps:     4,
			StepTimeout:  time.Second,
			TotalTimeout: total,
			ApprovalTTL:  ttl,
		},
	}
	out, runErr := loop.Run(ctx, agent.Input{RunID: confirmRunID, WorkspaceID: "ws-1", Prompt: spec.task})
	id, idErr := lookupTicket(ctx, box.ws.Runs)
	if idErr != nil && !errors.Is(idErr, sql.ErrNoRows) {
		return nil, idErr
	}
	box.tool = tool
	box.machine = machine
	box.ticketID = id
	box.outcome = out
	box.runErr = runErr
	ok = true
	return box, nil
}

func (b *caseBox) observations(ctx context.Context) (map[string]any, error) {
	if b.ws == nil {
		return nil, errors.New("工作区已关闭")
	}
	state, err := lookupRunState(ctx, b.ws.Runs)
	if err != nil {
		return nil, err
	}
	if state == "" {
		state = b.outcome.State
	}
	tickets, err := countRows(ctx, b.ws.Runs, `SELECT COUNT(*) FROM approvals`)
	if err != nil {
		return nil, err
	}
	actions, err := auditActions(ctx, b.ws.Runs)
	if err != nil {
		return nil, err
	}
	code := approvalCode(b.runErr)
	if b.extraErr != nil {
		if extra := approvalCode(b.extraErr); extra != "" {
			code = extra
		}
	}
	exec, bypass, side, mounts, network := b.tool.snapshot()
	return map[string]any{
		"run_state":                  state,
		"audit_event":                actions,
		"approval_ticket_count":      tickets,
		"approval_bypass_count":      bypass,
		"tool_execution_count":       exec,
		"artifact_count":             0,
		"write_side_effect_count":    side,
		"network_permission_granted": network,
		"extra_mount_request_count":  mounts,
		"error_code":                 code,
	}, nil
}

func runExecutingRestart(ctx context.Context) (map[string]any, error) {
	box, err := newCaseBox(ctx)
	if err != nil {
		return nil, err
	}
	defer box.close()
	if err := insertRun(ctx, box.ws.Runs, confirmRunID, run.StateRunning); err != nil {
		return nil, err
	}
	svc := run.New(box.ws.Runs)
	if err := svc.Begin(ctx, "step-1", confirmRunID, "export_data", []byte(`{"object":"contacts"}`)); err != nil {
		return nil, err
	}
	if err := box.reopen(ctx); err != nil {
		return nil, err
	}
	box.tool = &observeTool{}
	return box.observations(ctx)
}

func runConcurrentApprove(ctx context.Context, task string) (map[string]any, error) {
	return runThen(ctx, loopCase{
		task: task, spec: tools.SpecExportData,
		args:    `{"object":"contacts"}`,
		name:    "export_data",
		approve: []approval.Response{{Action: approval.ActionDefer}},
	}, func(ctx context.Context, box *caseBox) error {
		if box.ticketID == "" {
			return errors.New("并发确认没有确认单")
		}
		gate := newPairGate()
		box.machine.SetGate(gate.enter)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		for i := 0; i < 2; i++ {
			go func(i int) {
				defer wg.Done()
				_, errs[i] = box.machine.Decide(ctx, box.ticketID, approval.Decision{Action: approval.ActionApprove})
			}(i)
		}
		wg.Wait()
		box.machine.SetGate(nil)
		if !gate.ready() {
			return errors.New("并发确认没有同时开始")
		}
		for _, err := range errs {
			if err != nil {
				return err
			}
		}
		svc := run.New(box.ws.Runs)
		svc.SetExecutor(box.tool.execute)
		res, err := svc.Resume(ctx, confirmRunID)
		if err != nil {
			return err
		}
		if res.State != "" {
			box.outcome.State = res.State
		}
		return nil
	})
}

func runApproveAfterTimeout(ctx context.Context, task string) (map[string]any, error) {
	return runThen(ctx, loopCase{
		task: task, spec: tools.SpecExportData,
		args:    `{"object":"contacts"}`,
		name:    "export_data",
		approve: []approval.Response{{Action: approval.ActionDefer}},
	}, func(ctx context.Context, box *caseBox) error {
		if box.ticketID == "" {
			return errors.New("过期确认没有确认单")
		}
		past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
		if _, err := box.ws.Runs.ExecContext(ctx, `UPDATE approvals SET expires_at = ? WHERE id = ?`, past, box.ticketID); err != nil {
			return err
		}
		svc := run.New(box.ws.Runs)
		svc.SetExecutor(box.tool.execute)
		svc.SetApprover(&approval.Fake{Responses: []approval.Response{{Action: approval.ActionApprove}}})
		res, err := svc.Resume(ctx, confirmRunID)
		if !errors.Is(err, approval.ErrExpired) {
			return fmt.Errorf("到期后再批 = %v state=%s", err, res.State)
		}
		_, decideErr := box.machine.Decide(ctx, box.ticketID, approval.Decision{Action: approval.ActionApprove})
		box.extraErr = decideErr
		return nil
	})
}

func newCaseBox(ctx context.Context) (*caseBox, error) {
	root, err := os.MkdirTemp("", "lantern-confirm-")
	if err != nil {
		return nil, err
	}
	ws, err := workspace.Create(ctx, root, "ws-1")
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	return &caseBox{root: root, ws: ws}, nil
}

func insertRun(ctx context.Context, db *sql.DB, id, state string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.ExecContext(ctx, `
		INSERT INTO runs (id, state, version, context, created_at, updated_at)
		VALUES (?, ?, 0, '', ?, ?)`, id, state, now, now)
	if err != nil {
		return fmt.Errorf("写入运行: %w", err)
	}
	return nil
}

func lookupTicket(ctx context.Context, db *sql.DB) (string, error) {
	var id string
	err := db.QueryRowContext(ctx, `SELECT id FROM approvals ORDER BY created_at LIMIT 1`).Scan(&id)
	return id, err
}

func lookupRunState(ctx context.Context, db *sql.DB) (string, error) {
	var state string
	err := db.QueryRowContext(ctx, `SELECT state FROM runs WHERE id = ?`, confirmRunID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return state, nil
}

func countRows(ctx context.Context, db *sql.DB, query string) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, query).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func auditActions(ctx context.Context, db *sql.DB) ([]any, error) {
	rows, err := db.QueryContext(ctx, `SELECT action FROM audit ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []any
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			return nil, err
		}
		out = append(out, action)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []any{}
	}
	return out, nil
}

func approvalCode(err error) string {
	if err == nil {
		return ""
	}
	var agentErr *agent.Error
	if errors.As(err, &agentErr) && agentErr != nil && agentErr.Code != "" {
		return agentErr.Code
	}
	switch {
	case errors.Is(err, approval.ErrTerminal):
		return "approval_terminal"
	case errors.Is(err, approval.ErrExpired):
		return "approval_expired"
	case errors.Is(err, approval.ErrSnapshotMismatch):
		return "snapshot_hash_mismatch"
	default:
		return ""
	}
}

type observeTool struct {
	spec    tools.Spec
	mu      sync.Mutex
	exec    int
	bypass  int
	side    int
	mounts  int
	network bool
}

func (t *observeTool) Spec() tools.Spec { return t.spec }

func (t *observeTool) Execute(_ context.Context, call tools.Call) (tools.Result, error) {
	if t == nil {
		return tools.Result{}, errors.New("没有工具")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.note(call)
	return tools.Result{Content: "ok", Summary: "ok"}, nil
}

func (t *observeTool) execute(ctx context.Context, call tools.Call) (tools.Result, error) {
	return t.Execute(ctx, call)
}

func (t *observeTool) note(call tools.Call) {
	t.exec++
	switch call.Name {
	case "export_data", "write_action":
		t.side++
		if call.ApprovalID == "" {
			t.bypass++
		}
	case "run_python":
		decision, err := policy.Default().Decide(context.Background(), policy.Request{Tool: call.Name, Args: call.Args})
		if err == nil && (decision.NeedsConfirm || decision.Deny) && call.ApprovalID == "" {
			t.bypass++
		}
		grant, gerr := policy.ParseRunPython(call.Args)
		if gerr == nil {
			if grant.Network {
				t.network = true
			}
			t.mounts += len(grant.ExtraReadOnly)
		}
	}
}

func (t *observeTool) snapshot() (exec, bypass, side, mounts int, network bool) {
	if t == nil {
		return 0, 0, 0, 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exec, t.bypass, t.side, t.mounts, t.network
}

type pairGate struct {
	mu  sync.Mutex
	n   int
	rel chan struct{}
}

func newPairGate() *pairGate {
	return &pairGate{rel: make(chan struct{})}
}

func (g *pairGate) enter() {
	g.mu.Lock()
	g.n++
	if g.n == 2 {
		close(g.rel)
	}
	g.mu.Unlock()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-g.rel:
	case <-timer.C:
	}
}

func (g *pairGate) ready() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n >= 2
}

func inputString(input any, key string) (string, bool) {
	m, ok := asMap(input)
	if !ok {
		return "", false
	}
	value, exists := m[key]
	if !exists || value == nil {
		return "", false
	}
	text, ok := value.(string)
	if !ok {
		return fmt.Sprint(value), true
	}
	return text, true
}

func inputBool(input any, key string) bool {
	m, ok := asMap(input)
	if !ok {
		return false
	}
	value, ok := m[key].(bool)
	return ok && value
}
