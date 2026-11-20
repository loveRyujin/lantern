package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/loveRyujin/lantern/core/run"
)

// maxTraceInputRunes 是轨迹里参数原文的展示上限。
const maxTraceInputRunes = 2000

// TraceLine 是轨迹的一行。所有字段都已转成纯文本，可以直接画或打印。
type TraceLine struct {
	Time string
	Head string
	Body []string
}

// ReadTrace 只读打开工作区的 runs.db 读一次运行的轨迹。
// 不调用 workspace.Open，因此不会跑 Recover，也不会追加审计。
func ReadTrace(ctx context.Context, root, workspaceID, runID string) (run.Trace, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return run.Trace{}, errors.New("session: 运行标识为空")
	}
	db, err := openRunsRO(ctx, root, workspaceID)
	if err != nil {
		return run.Trace{}, err
	}
	if db == nil {
		return run.Trace{}, run.ErrNotFound
	}
	defer db.Close()
	return run.ReadTrace(ctx, db, workspaceID, runID)
}

// ListRuns 只读列出工作区里最近的运行，新的在前。
func ListRuns(ctx context.Context, root, workspaceID string, limit int) ([]run.RunSummary, error) {
	db, err := openRunsRO(ctx, root, workspaceID)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return []run.RunSummary{}, nil
	}
	defer db.Close()
	return run.ListRuns(ctx, db, limit)
}

// openRunsRO 在库文件不存在时返回 nil, nil。
func openRunsRO(ctx context.Context, root, workspaceID string) (*sql.DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := runsDBPath(root, workspaceID)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return sql.Open("sqlite", readOnlyDSN(path))
}

// TraceError 把查询错误换成界面文案。不带库里的任何内容。
func TraceError(workspaceID, runID string, err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, run.ErrNotFound):
		return fmt.Sprintf("工作区 %s 里没有运行 %s。", PlainText(workspaceID), PlainText(runID))
	case errors.Is(err, run.ErrTraceWorkspace):
		return "这次运行的记录不属于当前工作区，已拒绝显示。"
	case errors.Is(err, run.ErrTraceCorrupt):
		return "轨迹数据损坏，无法显示：" + PlainText(strings.TrimPrefix(err.Error(), run.ErrTraceCorrupt.Error()+": "))
	default:
		return "读取轨迹失败：" + PlainText(err.Error())
	}
}

// TraceHeader 是轨迹顶部的运行摘要。
func TraceHeader(tr run.Trace) []string {
	out := []string{
		"运行 " + PlainText(tr.RunID) + " · 状态 " + stateLabel(tr.State),
		"创建 " + clock(tr.CreatedAt) + " · 更新 " + clock(tr.UpdatedAt),
	}
	switch tr.State {
	case run.StateAwaitingApproval:
		out = append(out, "等待确认：运行仍挂起，可在待确认列表里重开。")
	case run.StateRunning:
		out = append(out, "运行中：步骤可能还在增加。")
	}
	if tr.NoTransitions {
		out = append(out, "这次运行没有状态迁移记录（早于迁移 006），只能显示当前状态。")
	}
	return out
}

// TraceLines 按时间顺序把轨迹转成纯文本行。
func TraceLines(tr run.Trace) []TraceLine {
	out := make([]TraceLine, 0, len(tr.Entries))
	for _, e := range tr.Entries {
		out = append(out, traceLine(e))
	}
	return out
}

// FormatTrace 给命令行用。每个值都按纯文本转义，换行也不会伪造出新的一行。
func FormatTrace(workspaceID string, tr run.Trace) string {
	var b strings.Builder
	b.WriteString("工作区 " + PlainText(workspaceID) + "\n")
	for _, line := range TraceHeader(tr) {
		b.WriteString(line + "\n")
	}
	for _, line := range TraceLines(tr) {
		b.WriteString(line.Time + "  " + line.Head + "\n")
		for _, body := range line.Body {
			b.WriteString("    " + body + "\n")
		}
	}
	return b.String()
}

func traceLine(e run.TraceEntry) TraceLine {
	line := TraceLine{Time: clock(e.At)}
	add := func(label, value string) {
		if value != "" {
			line.Body = append(line.Body, label+" "+PlainText(value))
		}
	}
	switch e.Source {
	case run.TraceTransition:
		if e.From == "" {
			line.Head = "状态 创建为 " + stateLabel(e.To)
		} else {
			line.Head = "状态 " + stateLabel(e.From) + " → " + stateLabel(e.To)
		}
	case run.TraceModel:
		line.Head = joinHead("模型调用", resultLabel(e.Status), PlainText(e.Summary), duration(e.Duration), tokens(e.Tokens))
		add("错误码", e.Code)
	case run.TraceTool:
		line.Head = joinHead("工具 "+PlainText(e.Tool), stepLabel(e.Status), duration(e.Duration), tokens(e.Tokens))
		add("步骤", e.StepID)
		line.Body = append(line.Body, "参数 "+PlainText(clipRunes(e.Input, maxTraceInputRunes)))
		add("参数哈希", e.ParamsHash)
		add("结果", e.Summary)
		add("错误码", e.Code)
		add("确认单", e.TicketID)
		if e.Status == run.StepExecuting {
			line.Body = append(line.Body, "没有结束记录：执行中断，重启后不会重跑。")
		}
	case run.TraceEvent:
		line.Head = joinHead("工具 "+PlainText(e.Tool), resultLabel(e.Status), PlainText(e.Kind))
		add("摘要", e.Summary)
		add("参数哈希", e.ParamsHash)
		add("确认单", e.TicketID)
	case run.TraceApproval:
		line.Head = approvalHead(e)
		add("确认单", e.TicketID)
		add("冻结参数哈希", e.ParamsHash)
		if e.Kind == run.ApprovalRequested {
			line.Body = append(line.Body, RiskLine(PlainText(e.Risk)))
			if e.Status == "pending" {
				line.Body = append(line.Body, "当前 等待决定")
			}
		}
	case run.TraceAudit:
		line.Head = joinHead("审计 "+PlainText(e.Kind), PlainText(e.Actor))
		add("详情", e.Detail)
	default:
		line.Head = PlainText(e.Source + " " + e.Kind)
	}
	return line
}

func approvalHead(e run.TraceEntry) string {
	tool := PlainText(e.Tool)
	switch e.Kind {
	case run.ApprovalRequested:
		return joinHead("确认请求", tool)
	case run.ApprovalDecided:
		return joinHead("确认决定", decisionLabel(e.Decision), tool)
	case run.ApprovalConsumed:
		return joinHead("确认消费", "执行前核销", tool)
	case run.ApprovalExpired:
		return joinHead("确认过期", tool)
	default:
		return joinHead("确认", PlainText(e.Kind), tool)
	}
}

// PlainText 把控制字符和双向覆盖字符写成可见的转义，换行也一样。
// 结果只当文字画，不进 Markdown，也不会在终端里变成新的一行或颜色。
func PlainText(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case unicode.IsControl(r) || bidiControl(r):
			q := strconv.QuoteRuneToASCII(r)
			b.WriteString(q[1 : len(q)-1])
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func bidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x200E || r == 0x200F || r == 0x061C
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…（已截断）"
}

func joinHead(parts ...string) string {
	kept := parts[:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " · ")
}

func clock(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05.000")
}

func duration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if d < time.Millisecond {
		return "耗时 <1ms"
	}
	return "耗时 " + d.Round(time.Millisecond).String()
}

func tokens(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n) + " tokens"
}

func stateLabel(state string) string {
	switch state {
	case run.StateCreated:
		return "已创建"
	case run.StateRunning:
		return "运行中"
	case run.StateAwaitingApproval:
		return "等待确认"
	case run.StateRejected:
		return "已拒绝"
	case run.StateExpired:
		return "已过期"
	case run.StateCompleted:
		return "已完成"
	case run.StateFailed:
		return "失败"
	case run.StateCancelled:
		return "已取消"
	default:
		return PlainText(state)
	}
}

func stepLabel(status string) string {
	switch status {
	case run.StepPending:
		return "未开始"
	case run.StepExecuting:
		return "执行中"
	case run.StepDone:
		return "完成"
	case run.StepError:
		return "出错"
	default:
		return PlainText(status)
	}
}

func resultLabel(status string) string {
	switch status {
	case "", "ok":
		return "成功"
	case "failed":
		return "失败"
	case "skipped":
		return "跳过"
	case "rejected":
		return "打回"
	default:
		return PlainText(status)
	}
}

func decisionLabel(d string) string {
	switch d {
	case "approve":
		return "批准"
	case "reject":
		return "拒绝"
	case "defer":
		return "暂缓"
	case "":
		return "无决定"
	default:
		return PlainText(d)
	}
}
