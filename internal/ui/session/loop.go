package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// LoopRunner 用真实 Agent 循环处理一次提问。
// 票据只由 Loop.confirm 创建。本类型不调用 Machine.Decide，也不自己执行工具。
type LoopRunner struct {
	// Root 是应用数据目录。
	Root string
	// Waiter 接收确认决定。必须非空。
	Waiter *Waiter
	// Tool 是注册进循环的工具。Execute 是副作用计数点。
	Tool tools.Tool
	// NewProvider 每次提问构造一个模型。为空且 Demo 为真时用确定性脚本。
	NewProvider func(runID string) llm.Provider
	// Demo 为真时，界面标明这是确定性演示，不连接网络模型。
	Demo bool
	// Config 传给循环。零值使用循环自己的默认超时。
	Config agent.Config
}

// NewDemoRunner 构造产品窗口用的演示循环。
// 模型是进程内脚本，工具是 export_data。确认单仍由 Core 创建和消费。
func NewDemoRunner(root string, waiter *Waiter) *LoopRunner {
	return &LoopRunner{
		Root:        root,
		Waiter:      waiter,
		Tool:        NewExportCounter(),
		NewProvider: demoProvider,
		Demo:        true,
	}
}

// Executable 返回 Resume 必须复用的同一个工具。
func (r *LoopRunner) Executable() tools.Tool {
	if r == nil {
		return nil
	}
	return r.Tool
}

// Ask 打开工作区，写入运行行，然后调用 agent.Loop.Run。
// 运行标识已经存在时直接返回，不进入循环，也不开新票据。
func (r *LoopRunner) Ask(ctx context.Context, req Ask) (AskResult, error) {
	if r == nil || r.Waiter == nil || r.Tool == nil {
		return AskResult{}, errors.New("session: 循环运行器未配置")
	}
	if strings.TrimSpace(r.Root) == "" {
		return AskResult{}, errors.New("session: 没有数据目录")
	}
	if req.RunID == "" || req.WorkspaceID == "" || strings.TrimSpace(req.Prompt) == "" {
		return AskResult{}, errors.New("session: 运行标识、工作区或问题为空")
	}
	if err := ctx.Err(); err != nil {
		return AskResult{}, err
	}
	h, err := workspace.Open(ctx, r.Root, req.WorkspaceID)
	if err != nil {
		return AskResult{}, err
	}
	defer h.Close()

	exists, err := runExists(ctx, h.Runs, req.RunID)
	if err != nil {
		return AskResult{}, err
	}
	if exists {
		return AskResult{}, fmt.Errorf("session: 运行标识已存在")
	}
	if err := insertRun(ctx, h.Runs, req.RunID); err != nil {
		return AskResult{}, err
	}

	provider := r.provider(req.RunID)
	if provider == nil {
		return AskResult{}, errors.New("session: 没有模型")
	}
	reg := &tools.Registry{}
	if err := reg.Register(r.Tool); err != nil {
		return AskResult{}, err
	}
	status, steps := agent.Persist(run.New(h.Runs))
	loop := &agent.Loop{
		Provider: provider,
		Tools:    reg,
		Policy:   policy.Default(),
		Approver: r.Waiter,
		Tickets:  approval.New(h.Runs),
		Events:   req.Events,
		Audit:    audit.NewSQLite(h.Runs),
		Status:   status,
		Steps:    steps,
		Config:   r.Config,
	}
	out, runErr := loop.Run(ctx, agent.Input{
		RunID:       req.RunID,
		WorkspaceID: req.WorkspaceID,
		Prompt:      req.Prompt,
	})
	return AskResult{RunState: out.State}, runErr
}

func (r *LoopRunner) provider(runID string) llm.Provider {
	if r != nil && r.NewProvider != nil {
		return r.NewProvider(runID)
	}
	if r != nil && r.Demo {
		return demoProvider(runID)
	}
	return nil
}

func runExists(ctx context.Context, db *sql.DB, id string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE id = ?`, id).Scan(&n); err != nil {
		return false, fmt.Errorf("session: 读取运行: %w", err)
	}
	return n > 0, nil
}

func insertRun(ctx context.Context, db *sql.DB, id string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.ExecContext(ctx, `
		INSERT INTO runs (id, state, version, context, created_at, updated_at)
		VALUES (?, ?, 0, '', ?, ?)`, id, run.StateCreated, now, now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return fmt.Errorf("session: 运行标识已存在: %w", err)
		}
		return fmt.Errorf("session: 写入运行: %w", err)
	}
	return nil
}

func demoProvider(runID string) llm.Provider {
	raw, err := json.Marshal(map[string]any{
		"object": "**不要执行**\n```sql\nDROP TABLE users\n```",
		"fields": []string{"phone"},
	})
	if err != nil {
		panic(err)
	}
	return llm.NewMock(llm.MustNewScript(
		llm.CallTools(llm.ToolCall{
			ID:        "call-" + runID,
			Name:      "export_data",
			Arguments: string(raw),
		}),
		llm.Say("演示回答：循环已结束。这不是确认单。"),
	))
}
