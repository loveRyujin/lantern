// Package pythonx supplies production run_sql/run_python tools scoped to one
// workspace and Agent run. Trusted setup prepares Python separately; tool calls
// never install packages or expand sandbox permissions.
package pythonx

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/loveRyujin/lantern/core/analysisdata"
	"github.com/loveRyujin/lantern/core/errcode"
	"github.com/loveRyujin/lantern/core/pyrt"
	"github.com/loveRyujin/lantern/core/sandbox"
	"github.com/loveRyujin/lantern/core/sqlx"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

const (
	MaxSQLBytes = 8 << 20
	MaxSQLCalls = 32
	// MaxArtifactBytes bounds retained payload, not total Session heap/RSS.
	MaxArtifactBytes = 64 << 20
	CodeScope        = "analysis_scope_mismatch"
	CodeClosed       = "analysis_session_closed"
	CodeInput        = "sql_input_unavailable"
	CodeBudget       = "analysis_budget_exceeded"
	CodeArtifact     = "artifact_not_found"
)

type Config struct {
	Workspace *workspace.Handle
	RunID     string
	// Runtime must come from explicit trusted pyrt preparation. Missing/stale
	// environments fail at Execute; SQL remains usable without Python.
	Runtime pyrt.Runtime
}

type Session struct {
	mu                      sync.Mutex
	wg                      sync.WaitGroup
	workspaceID, runID, dir string
	runtime                 pyrt.Runtime
	db                      *sql.DB
	sql                     tools.Tool
	runner                  sandbox.Sandbox
	acquire                 func(context.Context, pyrt.Runtime) (func(), error)
	life                    context.Context
	cancel                  context.CancelFunc
	slots                   chan struct{}
	closed                  bool
	closeDone               chan struct{}
	closeErr                error
	sources                 map[string]string
	reserved                map[string]bool
	sourceBytes             int
	artifacts               map[string]sandbox.Artifact
	artifactBytes           int
}

func New(ctx context.Context, cfg Config) (*Session, error) {
	if ctx == nil || cfg.Workspace == nil || cfg.Workspace.Analytics == nil || !validID(cfg.Workspace.ID, 128) || !validID(cfg.RunID, 256) {
		return nil, errors.New("pythonx: workspace and run are required")
	}
	ws := cfg.Workspace
	if !filepath.IsAbs(ws.Dir) || filepath.Clean(ws.Dir) != ws.Dir || filepath.Join(ws.Root, "workspaces", ws.ID) != ws.Dir {
		return nil, errors.New("pythonx: invalid workspace directory")
	}
	if err := ws.Analytics.PingContext(ctx); err != nil {
		return nil, err
	}
	path := filepath.Join(ws.Dir, "analytics.db")
	db, err := sqlx.Open(ctx, path, path)
	if err != nil {
		return nil, err
	}
	tool, err := sqlx.NewTool(db, path, path, nil)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	life, cancel := context.WithCancel(context.Background())
	return &Session{workspaceID: ws.ID, runID: cfg.RunID, dir: ws.Dir, runtime: cfg.Runtime, db: db, sql: tool,
		runner: sandbox.New(sandbox.Config{TempRoot: filepath.Join(ws.Dir, "tmp")}), acquire: pyrt.AcquirePrepared,
		life: life, cancel: cancel, slots: make(chan struct{}, 2), closeDone: make(chan struct{}),
		sources: map[string]string{}, reserved: map[string]bool{}, artifacts: map[string]sandbox.Artifact{}}, nil
}

func validID(value string, max int) bool {
	return value != "" && len(value) <= max && utf8.ValidString(value) && strings.IndexByte(value, 0) < 0
}
func failure(code, message string) error { return &tools.Error{Code: code, Message: message} }

// SQLTool records successful SQL observations; PythonTool consumes explicit IDs.
func (s *Session) SQLTool() tools.Tool    { return sqlTool{s} }
func (s *Session) PythonTool() tools.Tool { return pythonTool{s} }

// begin atomically joins the lifetime before waiting for the concurrency slot.
// Close cancels and waits for both queued and active calls before releasing data.
func (s *Session) begin(ctx context.Context, call tools.Call) (context.Context, func(), error) {
	if ctx == nil {
		return nil, nil, failure(CodeClosed, "执行上下文不可用")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, failure(CodeClosed, "分析会话已关闭")
	}
	if call.WorkspaceID != s.workspaceID || call.RunID != s.runID {
		s.mu.Unlock()
		return nil, nil, failure(CodeScope, "调用不属于当前工作区和运行")
	}
	s.wg.Add(1)
	s.mu.Unlock()
	active, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.life, cancel)
	finish := func() { stop(); cancel(); s.wg.Done() }
	select {
	case s.slots <- struct{}{}:
		return active, func() { <-s.slots; finish() }, nil
	case <-active.Done():
		finish()
		return nil, nil, bridge(active.Err())
	}
}

// Close cancels in-flight tools, waits for their cleanup/lease release, then
// releases all retained bytes. Agent.Run does not automatically close Session.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		return s.closeErr
	}
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	s.wg.Wait()
	err := s.db.Close()
	s.mu.Lock()
	s.sources = nil
	s.reserved = nil
	s.artifacts = nil
	s.sourceBytes = 0
	s.artifactBytes = 0
	s.closeErr = err
	close(s.closeDone)
	s.mu.Unlock()
	return err
}

type sqlTool struct{ session *Session }

func (t sqlTool) Spec() tools.Spec { return tools.SpecRunSQL }
func (t sqlTool) Execute(ctx context.Context, call tools.Call) (tools.Result, error) {
	s := t.session
	ctx, done, err := s.begin(ctx, call)
	if err != nil {
		return tools.Result{}, err
	}
	defer done()
	if !validID(call.ID, 256) {
		return tools.Result{}, failure(CodeInput, "SQL 调用 ID 不合法")
	}
	s.mu.Lock()
	_, exists := s.sources[call.ID]
	if exists || s.reserved[call.ID] {
		s.mu.Unlock()
		return tools.Result{}, failure(CodeInput, "SQL 调用 ID 已存在，不会覆盖旧结果")
	}
	if len(s.sources)+len(s.reserved) >= MaxSQLCalls || s.sourceBytes+(len(s.reserved)+1)*analysisdata.MaxSourceBytes > MaxSQLBytes {
		s.mu.Unlock()
		return tools.Result{}, failure(CodeBudget, "SQL 输入缓存已满，请开始新的分析运行")
	}
	s.reserved[call.ID] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.reserved, call.ID); s.mu.Unlock() }()
	result, err := s.sql.Execute(ctx, call)
	if err != nil {
		return tools.Result{}, err
	}
	// Validate the exact production observation before making it referenceable.
	if _, err = analysisdata.FromToolJSON(ctx, result.Content); err != nil {
		return tools.Result{}, bridge(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return tools.Result{}, failure(CodeClosed, "分析会话已关闭")
	}
	if ctx.Err() != nil {
		return tools.Result{}, bridge(ctx.Err())
	}
	delete(s.reserved, call.ID)
	s.sources[call.ID] = result.Content
	s.sourceBytes += len(result.Content)
	return result, nil
}

func bridge(err error) error {
	code := errcode.Exec
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		code = errcode.Timeout
	} else if coded, ok := errcode.CodeOf(err); ok {
		code = coded
	} else if errors.Is(err, sandbox.ErrUnsupported) {
		code = errcode.SandboxUnavailable
	}
	message := "Python 执行失败，请检查代码和输入"
	switch code {
	case errcode.Timeout:
		message = "分析已取消或超过执行时限"
	case errcode.OOM:
		message = "分析超过内存预算，请减少数据或中间结果"
	case errcode.RuntimeUnavailable:
		message = "Python 环境不可用，请先显式准备运行环境"
	case errcode.SandboxUnavailable:
		message = "当前平台无法提供所需沙箱隔离"
	}
	return &tools.Error{Code: string(code), Message: message, Err: err}
}
