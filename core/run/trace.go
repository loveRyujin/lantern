package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// 轨迹条目的来源。
const (
	// TraceTransition 来自 run_transitions，是一次运行状态迁移。
	TraceTransition = "transition"
	// TraceModel 来自 run_events 的 model_call，是一次模型调用摘要。
	TraceModel = "model"
	// TraceTool 来自 run_steps，带上 run_events 里同一步骤的结果摘要。
	TraceTool = "tool"
	// TraceEvent 来自 run_events，是没有落成步骤的工具失败、跳过或引用校验。
	TraceEvent = "event"
	// TraceApproval 来自 approvals，是确认单的请求、决定、消费或过期。
	TraceApproval = "approval"
	// TraceAudit 来自 audit。
	TraceAudit = "audit"
)

// 确认单在轨迹里的阶段。
const (
	ApprovalRequested = "requested"
	ApprovalDecided   = "decided"
	ApprovalConsumed  = "consumed"
	ApprovalExpired   = "expired"
)

const modelCallKind = "model_call"

var (
	// ErrTraceCorrupt 表示轨迹相关的行无法解析。不会返回部分轨迹。
	ErrTraceCorrupt = errors.New("run: 轨迹数据损坏")
	// ErrTraceWorkspace 表示记录上的工作区与查询的工作区不一致。不会返回任何行。
	ErrTraceWorkspace = errors.New("run: 运行记录不属于当前工作区")
)

// Trace 是一次运行的只读轨迹。Entries 按时间排序。
type Trace struct {
	RunID     string
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
	// NoTransitions 为真表示这次运行没有任何迁移记录：库里没有 run_transitions 表，
	// 或运行早于迁移 006。此时只能显示当前状态。
	NoTransitions bool
	Entries       []TraceEntry
}

// TraceEntry 是轨迹里的一行。字段按 Source 取用，没有的留空。
// Input、Summary、Detail 都是库里的原文，展示方必须按纯文本处理。
type TraceEntry struct {
	At     time.Time
	Source string
	// Kind 在不同来源下分别是：迁移的目标状态、model_call、工具名、
	// run_events 的 kind、确认阶段或审计动作。
	Kind       string
	Status     string
	From       string
	To         string
	Tool       string
	StepID     string
	TicketID   string
	ParamsHash string
	Decision   string
	Risk       string
	Actor      string
	Code       string
	Summary    string
	Input      string
	Detail     string
	Duration   time.Duration
	Tokens     int
}

// RunSummary 是最近运行列表的一项。
type RunSummary struct {
	ID        string
	State     string
	CreatedAt time.Time
}

// ReadTrace 只读拼出 runID 的轨迹。本函数只执行 SELECT。
//
// 运行不存在时返回 ErrNotFound。确认单上的工作区与 workspaceID 不一致时返回
// ErrTraceWorkspace；时间、状态或事件载荷无法解析时返回 ErrTraceCorrupt。
// 这两种情况都不返回部分轨迹。
func ReadTrace(ctx context.Context, db *sql.DB, workspaceID, runID string) (Trace, error) {
	if err := ctx.Err(); err != nil {
		return Trace{}, err
	}
	if db == nil {
		return Trace{}, ErrNoDB
	}
	if workspaceID == "" || runID == "" {
		return Trace{}, fmt.Errorf("run: 工作区或运行标识为空")
	}
	tr := Trace{RunID: runID}
	var created, updated string
	err := db.QueryRowContext(ctx, `SELECT state, created_at, updated_at FROM runs WHERE id = ?`, runID).
		Scan(&tr.State, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Trace{}, ErrNotFound
	}
	if err != nil {
		return Trace{}, fmt.Errorf("run: 读取运行: %w", err)
	}
	if !knownState(tr.State) {
		return Trace{}, corrupt("runs.state 未知")
	}
	if tr.CreatedAt, err = traceTime(created, "runs.created_at"); err != nil {
		return Trace{}, err
	}
	if tr.UpdatedAt, err = traceTime(updated, "runs.updated_at"); err != nil {
		return Trace{}, err
	}

	approvals, err := traceApprovals(ctx, db, workspaceID, runID)
	if err != nil {
		return Trace{}, err
	}
	transitions, err := traceTransitions(ctx, db, runID)
	if err != nil {
		return Trace{}, err
	}
	tr.NoTransitions = len(transitions) == 0
	steps, err := traceSteps(ctx, db, runID)
	if err != nil {
		return Trace{}, err
	}
	events, err := traceEvents(ctx, db, runID, steps.byID)
	if err != nil {
		return Trace{}, err
	}
	audits, err := traceAudits(ctx, db, runID)
	if err != nil {
		return Trace{}, err
	}

	// 时间相同的行没有跨表的写入序号可比，按 tieRanks 和这里的拼接顺序排。
	type ranked struct {
		entry TraceEntry
		rank  int
	}
	rows := make([]ranked, 0, len(transitions)+len(approvals)+len(steps.ordered)+len(events)+len(audits))
	for i, rank := range tieRanks(transitions) {
		rows = append(rows, ranked{transitions[i], rank})
	}
	others := append(append([]TraceEntry{}, events...), approvals...)
	for _, st := range steps.ordered {
		others = append(others, st.entry)
	}
	for _, e := range append(others, audits...) {
		rows = append(rows, ranked{e, 1})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].entry.At.Equal(rows[j].entry.At) {
			return rows[i].entry.At.Before(rows[j].entry.At)
		}
		return rows[i].rank < rows[j].rank
	})
	tr.Entries = make([]TraceEntry, len(rows))
	for i, r := range rows {
		tr.Entries[i] = r.entry
	}
	return tr, nil
}

// tieRanks 给按 id 排好的迁移定同一时间戳下的位置：创建排最前（0），
// 同一时间戳里最后一条迁移若是挂起或终态则排最后（2），因为它收尾一段循环；
// 其余与别的来源同级（1）。迁移之间因此保持 id 顺序。
func tieRanks(transitions []TraceEntry) []int {
	ranks := make([]int, len(transitions))
	for i, e := range transitions {
		last := i == len(transitions)-1 || !transitions[i+1].At.Equal(e.At)
		switch {
		case e.From == "":
			ranks[i] = 0
		case last && (e.To == StateAwaitingApproval || terminal(e.To)):
			ranks[i] = 2
		default:
			ranks[i] = 1
		}
	}
	return ranks
}

// ListRuns 只读列出最近的运行，新的在前。limit 不大于 0 时取 50。
func ListRuns(ctx context.Context, db *sql.DB, limit int) ([]RunSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if db == nil {
		return nil, ErrNoDB
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, state, created_at FROM runs
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("run: 列出运行: %w", err)
	}
	defer rows.Close()
	out := []RunSummary{}
	for rows.Next() {
		var rs RunSummary
		var created string
		if err := rows.Scan(&rs.ID, &rs.State, &created); err != nil {
			return nil, fmt.Errorf("run: 列出运行: %w", err)
		}
		if rs.CreatedAt, err = traceTime(created, "runs.created_at"); err != nil {
			return nil, err
		}
		out = append(out, rs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run: 列出运行: %w", err)
	}
	return out, nil
}

func traceApprovals(ctx context.Context, db *sql.DB, workspaceID, runID string) ([]TraceEntry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, step_id, params_hash, action, decision, state, created_at,
			COALESCE(decided_at, ''), COALESCE(consumed_at, ''), expires_at, risk, workspace_id
		FROM approvals
		WHERE run_id = ?
		ORDER BY created_at, id`, runID)
	if err != nil {
		return nil, fmt.Errorf("run: 读取确认单: %w", err)
	}
	defer rows.Close()
	var out []TraceEntry
	for rows.Next() {
		var id, stepID, hash, action, decision, state, created, decided, consumed, expires, risk, ws string
		if err := rows.Scan(&id, &stepID, &hash, &action, &decision, &state, &created, &decided, &consumed, &expires, &risk, &ws); err != nil {
			return nil, fmt.Errorf("run: 读取确认单: %w", err)
		}
		if ws != "" && ws != workspaceID {
			return nil, ErrTraceWorkspace
		}
		base := TraceEntry{
			Source:     TraceApproval,
			Tool:       action,
			StepID:     stepID,
			TicketID:   id,
			ParamsHash: hash,
			Status:     state,
			Risk:       risk,
		}
		at, err := traceTime(created, "approvals.created_at")
		if err != nil {
			return nil, err
		}
		req := base
		req.At, req.Kind = at, ApprovalRequested
		out = append(out, req)
		if decided != "" {
			at, err := traceTime(decided, "approvals.decided_at")
			if err != nil {
				return nil, err
			}
			d := base
			d.At, d.Kind, d.Decision = at, ApprovalDecided, decision
			out = append(out, d)
		}
		if consumed != "" {
			at, err := traceTime(consumed, "approvals.consumed_at")
			if err != nil {
				return nil, err
			}
			c := base
			c.At, c.Kind, c.Decision = at, ApprovalConsumed, decision
			out = append(out, c)
		}
		if state == "expired" {
			// 过期没有单独的时间列，用到期时间。
			at, err := traceTime(expires, "approvals.expires_at")
			if err != nil {
				return nil, err
			}
			e := base
			e.At, e.Kind, e.Decision = at, ApprovalExpired, decision
			out = append(out, e)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run: 读取确认单: %w", err)
	}
	return out, nil
}

func traceTransitions(ctx context.Context, db *sql.DB, runID string) ([]TraceEntry, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'run_transitions'`).Scan(&n); err != nil {
		return nil, fmt.Errorf("run: 读取迁移表: %w", err)
	}
	if n == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT from_state, to_state, at FROM run_transitions
		WHERE run_id = ?
		ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("run: 读取状态迁移: %w", err)
	}
	defer rows.Close()
	var out []TraceEntry
	for rows.Next() {
		var from, to, at string
		if err := rows.Scan(&from, &to, &at); err != nil {
			return nil, fmt.Errorf("run: 读取状态迁移: %w", err)
		}
		if !knownState(to) || (from != "" && !knownState(from)) {
			return nil, corrupt("run_transitions 状态未知")
		}
		when, err := traceTime(at, "run_transitions.at")
		if err != nil {
			return nil, err
		}
		out = append(out, TraceEntry{At: when, Source: TraceTransition, Kind: to, From: from, To: to})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run: 读取状态迁移: %w", err)
	}
	return out, nil
}

type traceStep struct {
	entry TraceEntry
}

type traceStepSet struct {
	ordered []*traceStep
	byID    map[string]*traceStep
}

func traceSteps(ctx context.Context, db *sql.DB, runID string) (traceStepSet, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, status, tool_name, input_json, created_at, updated_at
		FROM run_steps
		WHERE run_id = ?
		ORDER BY ordinal`, runID)
	if err != nil {
		return traceStepSet{}, fmt.Errorf("run: 读取步骤: %w", err)
	}
	defer rows.Close()
	out := traceStepSet{byID: map[string]*traceStep{}}
	for rows.Next() {
		var id, status, tool, input, created, updated string
		if err := rows.Scan(&id, &status, &tool, &input, &created, &updated); err != nil {
			return traceStepSet{}, fmt.Errorf("run: 读取步骤: %w", err)
		}
		switch status {
		case StepPending, StepExecuting, StepDone, StepError:
		default:
			return traceStepSet{}, corrupt("run_steps.status 未知")
		}
		begin, err := traceTime(created, "run_steps.created_at")
		if err != nil {
			return traceStepSet{}, err
		}
		end, err := traceTime(updated, "run_steps.updated_at")
		if err != nil {
			return traceStepSet{}, err
		}
		st := &traceStep{
			entry: TraceEntry{
				At: begin, Source: TraceTool, Kind: tool, Tool: tool,
				StepID: id, Status: status, Input: input,
			},
		}
		if status == StepDone || status == StepError {
			st.entry.Duration = end.Sub(begin)
		}
		out.ordered = append(out.ordered, st)
		out.byID[id] = st
	}
	if err := rows.Err(); err != nil {
		return traceStepSet{}, fmt.Errorf("run: 读取步骤: %w", err)
	}
	return out, nil
}

type tracePayload struct {
	ToolName   string `json:"tool_name"`
	ParamsHash string `json:"params_hash"`
	Summary    string `json:"summary"`
	DurationNS int64  `json:"duration_ns"`
	Tokens     int    `json:"tokens"`
	Status     string `json:"status"`
	Code       string `json:"code"`
	TicketID   string `json:"ticket_id"`
	StepID     string `json:"step_id"`
}

// traceEvents 把有 step_id、且 run_steps 里有这一步的事件并进步骤，其余单列。
func traceEvents(ctx context.Context, db *sql.DB, runID string, steps map[string]*traceStep) ([]TraceEntry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT kind, payload, created_at FROM run_events
		WHERE run_id = ?
		ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("run: 读取运行事件: %w", err)
	}
	defer rows.Close()
	var out []TraceEntry
	for rows.Next() {
		var kind, payload, created string
		if err := rows.Scan(&kind, &payload, &created); err != nil {
			return nil, fmt.Errorf("run: 读取运行事件: %w", err)
		}
		var p tracePayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return nil, corrupt("run_events.payload 不是合法 JSON")
		}
		at, err := traceTime(created, "run_events.created_at")
		if err != nil {
			return nil, err
		}
		if p.StepID != "" {
			if st, ok := steps[p.StepID]; ok {
				st.entry.Summary = p.Summary
				st.entry.ParamsHash = p.ParamsHash
				st.entry.Tokens = p.Tokens
				st.entry.Code = p.Code
				if p.TicketID != "" {
					st.entry.TicketID = p.TicketID
				}
				if p.DurationNS > 0 {
					st.entry.Duration = time.Duration(p.DurationNS)
				}
				continue
			}
		}
		source := TraceEvent
		if kind == modelCallKind {
			source = TraceModel
		}
		out = append(out, TraceEntry{
			At: at, Source: source, Kind: kind, Status: p.Status, Tool: p.ToolName,
			StepID: p.StepID, TicketID: p.TicketID, ParamsHash: p.ParamsHash,
			Code: p.Code, Summary: p.Summary, Duration: time.Duration(p.DurationNS), Tokens: p.Tokens,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run: 读取运行事件: %w", err)
	}
	return out, nil
}

// traceAudits 读 run_id 列。004 之前审批状态机写的行 run_id 为空，按 detail 里的 run= 认领。
func traceAudits(ctx context.Context, db *sql.DB, runID string) ([]TraceEntry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT at, actor, action, detail FROM audit
		WHERE run_id = ?
		   OR (run_id = '' AND actor = 'approval' AND instr(detail || ' ', ?) > 0)
		ORDER BY id`, runID, " run="+runID+" ")
	if err != nil {
		return nil, fmt.Errorf("run: 读取审计: %w", err)
	}
	defer rows.Close()
	var out []TraceEntry
	for rows.Next() {
		var at, actor, action, detail string
		if err := rows.Scan(&at, &actor, &action, &detail); err != nil {
			return nil, fmt.Errorf("run: 读取审计: %w", err)
		}
		when, err := traceTime(at, "audit.at")
		if err != nil {
			return nil, err
		}
		out = append(out, TraceEntry{At: when, Source: TraceAudit, Kind: action, Actor: actor, Detail: detail})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run: 读取审计: %w", err)
	}
	return out, nil
}

func traceTime(value, field string) (time.Time, error) {
	t, ok := parseTime(value)
	if !ok {
		return time.Time{}, corrupt(field + " 不是合法时间")
	}
	return t, nil
}

func corrupt(what string) error {
	return fmt.Errorf("%w: %s", ErrTraceCorrupt, what)
}
