package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/loveRyujin/lantern/core/errcode"
	"github.com/loveRyujin/lantern/core/pyrt"
	"github.com/loveRyujin/lantern/core/pythonx"
	"github.com/loveRyujin/lantern/core/sandbox"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

const (
	// CodeChartTooLarge 表示清单上的字节数超过界面上限。界面不会去取这份产物。
	CodeChartTooLarge = "chart_too_large"
	// CodeChartUnsupported 表示产物不是界面能画的 PNG。
	CodeChartUnsupported = "chart_format_unsupported"

	defaultKeepOpen = 4
	// 只保留清单和错误这类小数据，超过后最早的运行连同记录一起丢掉。
	maxRunRecords = 256
)

// Chart 是某次运行里一次成功 run_python 交回的一件产物，以及产生它的 SQL 与代码。
// 字节不在这里，界面要显示时经 [Analysis.Fetch] 从 Session 取副本。
type Chart struct {
	CallID     string
	ArtifactID string
	Name       string
	Kind       sandbox.ArtifactKind
	Bytes      int
	Code       string
	SQLCallID  string
	SQL        string
}

// PythonFailure 是一次失败的 run_python。Message 是工具给模型的同一句短说明，不含 traceback 或路径。
type PythonFailure struct {
	CallID  string
	Code    string
	Message string
}

// RunCharts 是一次运行的图表记录。Closed 为真时 Session 已关闭，字节不可再取。
// Tables 按调用顺序列出每次成功 run_sql 的结构化图表，数据就是交给模型的那份工具结果。
type RunCharts struct {
	Tables   []SQLChart
	Charts   []Chart
	Failures []PythonFailure
	Closed   bool
}

// Analysis 给每次提问开一个按工作区和运行绑定的 pythonx.Session，
// 把真实的 run_sql / run_python 注册进 Agent 循环，并记下成功调用交回的产物清单。
// Runtime 必须来自可信方显式准备；零值时 run_python 在执行时返回 runtime_unavailable。
type Analysis struct {
	Runtime pyrt.Runtime
	// KeepOpen 是同时保持打开的 Session 数。更早的会被关闭，图表提示不可取回。小于 1 时用 4。
	KeepOpen int
	// MaxChartBytes 是界面愿意取回的单件字节数。小于 1 时用 sandbox.MaxArtifactBytes。
	MaxChartBytes int

	mu     sync.Mutex
	runs   map[string]*analysisRun
	order  []string
	closed bool
}

type analysisRun struct {
	workspaceID string
	session     *pythonx.Session
	sql         map[string]string
	tables      []SQLChart
	charts      []Chart
	failures    []PythonFailure
}

// open 为 runID 开 Session，返回要注册进循环的两个工具。
// 超出 KeepOpen 的旧 Session 在这里关闭。
func (a *Analysis) open(ctx context.Context, h *workspace.Handle, runID string) ([]tools.Tool, error) {
	if a == nil || h == nil {
		return nil, errors.New("session: 分析未配置")
	}
	sess, err := pythonx.New(ctx, pythonx.Config{Workspace: h, RunID: runID, Runtime: a.Runtime})
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		_ = sess.Close()
		return nil, errors.New("session: 分析已关闭")
	}
	if a.runs == nil {
		a.runs = map[string]*analysisRun{}
	}
	if _, exists := a.runs[runID]; exists {
		a.mu.Unlock()
		_ = sess.Close()
		return nil, fmt.Errorf("session: 运行标识已存在")
	}
	rec := &analysisRun{workspaceID: h.ID, session: sess, sql: map[string]string{}}
	a.runs[runID] = rec
	a.order = append(a.order, runID)
	stale := a.evictLocked()
	a.mu.Unlock()
	for _, s := range stale {
		_ = s.Close()
	}
	return []tools.Tool{
		recorded{inner: sess.SQLTool(), done: func(call tools.Call, res tools.Result, err error) { a.noteSQL(runID, call, res, err) }},
		recorded{inner: sess.PythonTool(), done: func(call tools.Call, res tools.Result, err error) { a.notePython(runID, call, res, err) }},
	}, nil
}

// evictLocked 关掉超出 KeepOpen 的 Session，并丢掉超出上限的旧记录。返回要在锁外关闭的 Session。
func (a *Analysis) evictLocked() []*pythonx.Session {
	keep := a.KeepOpen
	if keep < 1 {
		keep = defaultKeepOpen
	}
	var stale []*pythonx.Session
	open := 0
	for i := len(a.order) - 1; i >= 0; i-- {
		rec := a.runs[a.order[i]]
		if rec.session == nil {
			continue
		}
		open++
		if open > keep {
			stale = append(stale, rec.session)
			rec.session = nil
		}
	}
	for len(a.order) > maxRunRecords {
		if s := a.runs[a.order[0]].session; s != nil {
			stale = append(stale, s)
		}
		delete(a.runs, a.order[0])
		a.order = a.order[1:]
	}
	return stale
}

func (a *Analysis) noteSQL(runID string, call tools.Call, res tools.Result, err error) {
	if err != nil {
		return
	}
	var args struct {
		SQL string `json:"sql"`
	}
	if json.Unmarshal(call.Args, &args) != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rec := a.runs[runID]
	if rec == nil {
		return
	}
	rec.sql[call.ID] = args.SQL
	series, code, reason := SeriesFromSQL(res.Content)
	rec.tables = append(rec.tables, SQLChart{CallID: call.ID, SQL: args.SQL, Series: series, Code: code, Reason: reason})
}

func (a *Analysis) notePython(runID string, call tools.Call, res tools.Result, err error) {
	var args struct {
		Code      string `json:"code"`
		SQLCallID string `json:"sql_call_id"`
	}
	_ = json.Unmarshal(call.Args, &args)
	a.mu.Lock()
	defer a.mu.Unlock()
	rec := a.runs[runID]
	if rec == nil {
		return
	}
	if err != nil {
		code, ok := tools.CodeOf(err)
		if !ok {
			code = string(errcode.Exec)
		}
		msg := "Python 执行失败"
		var te *tools.Error
		if errors.As(err, &te) && te.Message != "" {
			msg = te.Message
		}
		rec.failures = append(rec.failures, PythonFailure{CallID: call.ID, Code: code, Message: msg})
		return
	}
	var obs struct {
		Artifacts []pythonx.Manifest `json:"artifacts"`
	}
	if json.Unmarshal([]byte(res.Content), &obs) != nil {
		return
	}
	for _, m := range obs.Artifacts {
		rec.charts = append(rec.charts, Chart{
			CallID: call.ID, ArtifactID: m.ID, Name: m.Name, Kind: m.Kind, Bytes: m.Bytes,
			Code: args.Code, SQLCallID: args.SQLCallID, SQL: rec.sql[args.SQLCallID],
		})
	}
}

// Charts 返回运行的图表记录。运行不属于 workspaceID 时当作没有，不暴露别区的清单。
func (a *Analysis) Charts(workspaceID, runID string) (RunCharts, bool) {
	if a == nil {
		return RunCharts{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rec := a.runs[runID]
	if rec == nil || rec.workspaceID != workspaceID {
		return RunCharts{}, false
	}
	return RunCharts{
		Tables:   append([]SQLChart(nil), rec.tables...),
		Charts:   append([]Chart(nil), rec.charts...),
		Failures: append([]PythonFailure(nil), rec.failures...),
		Closed:   rec.session == nil,
	}, true
}

// Fetch 从 Session 取一件图表的完整副本。Session 会再核对工作区和运行。
// 不是 PNG 或清单字节超限时不去取，直接返回错误。
func (a *Analysis) Fetch(workspaceID, runID, artifactID string) (sandbox.Artifact, error) {
	if a == nil {
		return sandbox.Artifact{}, chartErr(pythonx.CodeClosed, "分析会话已关闭")
	}
	a.mu.Lock()
	rec := a.runs[runID]
	if rec == nil {
		a.mu.Unlock()
		return sandbox.Artifact{}, chartErr(pythonx.CodeClosed, "分析会话已关闭")
	}
	if rec.workspaceID != workspaceID {
		a.mu.Unlock()
		return sandbox.Artifact{}, chartErr(pythonx.CodeScope, "产物不属于当前工作区和运行")
	}
	var meta *Chart
	for i := range rec.charts {
		if rec.charts[i].ArtifactID == artifactID {
			meta = &rec.charts[i]
			break
		}
	}
	if meta == nil {
		a.mu.Unlock()
		return sandbox.Artifact{}, chartErr(pythonx.CodeArtifact, "产物不存在")
	}
	kind, size, sess := meta.Kind, meta.Bytes, rec.session
	a.mu.Unlock()
	limit := a.maxBytes()
	if kind != sandbox.KindPNG {
		return sandbox.Artifact{}, chartErr(CodeChartUnsupported, fmt.Sprintf("产物格式 %s 不能作为图表显示", kind))
	}
	if size > limit {
		return sandbox.Artifact{}, chartErr(CodeChartTooLarge, fmt.Sprintf("图表 %d 字节，超过上限 %d 字节", size, limit))
	}
	if sess == nil {
		return sandbox.Artifact{}, chartErr(pythonx.CodeClosed, "分析会话已关闭")
	}
	art, err := sess.GetArtifact(workspaceID, runID, artifactID)
	if err != nil {
		return sandbox.Artifact{}, err
	}
	if art.Kind != sandbox.KindPNG {
		return sandbox.Artifact{}, chartErr(CodeChartUnsupported, fmt.Sprintf("产物格式 %s 不能作为图表显示", art.Kind))
	}
	if len(art.Data) > limit {
		return sandbox.Artifact{}, chartErr(CodeChartTooLarge, fmt.Sprintf("图表 %d 字节，超过上限 %d 字节", len(art.Data), limit))
	}
	if err := sandbox.ValidateArtifact(art, int64(limit)); err != nil {
		return sandbox.Artifact{}, chartErr(CodeChartUnsupported, "图表未通过产物校验")
	}
	return art, nil
}

func (a *Analysis) maxBytes() int {
	if a.MaxChartBytes < 1 || int64(a.MaxChartBytes) > sandbox.MaxArtifactBytes {
		return int(sandbox.MaxArtifactBytes)
	}
	return a.MaxChartBytes
}

// CloseRun 关闭一次运行的 Session。记录保留，之后取图会得到 analysis_session_closed。
func (a *Analysis) CloseRun(runID string) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	rec := a.runs[runID]
	var sess *pythonx.Session
	if rec != nil {
		sess, rec.session = rec.session, nil
	}
	a.mu.Unlock()
	if sess == nil {
		return nil
	}
	return sess.Close()
}

// Close 关闭全部 Session。之后不能再开新的运行。
func (a *Analysis) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	a.closed = true
	var all []*pythonx.Session
	for _, rec := range a.runs {
		if rec.session != nil {
			all = append(all, rec.session)
			rec.session = nil
		}
	}
	a.mu.Unlock()
	var errs []error
	for _, s := range all {
		errs = append(errs, s.Close())
	}
	return errors.Join(errs...)
}

func chartErr(code, msg string) error { return &tools.Error{Code: code, Message: msg} }

// ChartFailureLine 是界面上的一行失败说明：分类在前，码值和工具原话在后。
func ChartFailureLine(f PythonFailure) string {
	return fmt.Sprintf("Python 失败 · %s（%s）：%s", failureClass(f.Code), f.Code, f.Message)
}

func failureClass(code string) string {
	switch code {
	case string(errcode.Exec):
		return "代码执行出错"
	case string(errcode.Timeout):
		return "超时或已取消"
	case string(errcode.OOM):
		return "超过内存预算"
	case string(errcode.Killed):
		return "被终止"
	case string(errcode.RuntimeUnavailable):
		return "Python 环境不可用"
	case string(errcode.SandboxUnavailable):
		return "沙箱不可用"
	case pythonx.CodeInput:
		return "找不到引用的 SQL 结果"
	case pythonx.CodeBudget:
		return "超过分析预算"
	case pythonx.CodeClosed:
		return "分析会话已关闭"
	case pythonx.CodeScope:
		return "不属于当前工作区和运行"
	case tools.CodeArgumentsInvalid:
		return "参数不符合沙箱契约"
	default:
		return "其他错误"
	}
}

// ChartErrorLine 把取图失败写成占位文字。
func ChartErrorLine(name string, err error) string {
	code, _ := tools.CodeOf(err)
	var reason string
	switch code {
	case pythonx.CodeClosed:
		reason = "分析会话已关闭，产物不可取回"
	case pythonx.CodeScope:
		reason = "产物不属于当前工作区和运行，未读取"
	case pythonx.CodeArtifact:
		reason = "产物不存在"
	case CodeChartTooLarge, CodeChartUnsupported:
		var te *tools.Error
		if errors.As(err, &te) {
			reason = te.Message
		}
	default:
		reason = "取回失败"
	}
	if code == "" {
		code = "error"
	}
	return fmt.Sprintf("图表 %s 不可显示：%s（%s）", name, reason, code)
}

// recorded 原样转发给真实工具，只在返回后记下调用和结果。它不改参数，也不改结果。
type recorded struct {
	inner tools.Tool
	done  func(tools.Call, tools.Result, error)
}

func (r recorded) Spec() tools.Spec { return r.inner.Spec() }

func (r recorded) Execute(ctx context.Context, call tools.Call) (tools.Result, error) {
	res, err := r.inner.Execute(ctx, call)
	r.done(call, res, err)
	return res, err
}
