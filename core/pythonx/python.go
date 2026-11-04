package pythonx

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/loveRyujin/lantern/core/analysisdata"
	"github.com/loveRyujin/lantern/core/policy"
	"github.com/loveRyujin/lantern/core/sandbox"
	"github.com/loveRyujin/lantern/core/tools"
)

type Manifest struct {
	ID    string               `json:"id"`
	Name  string               `json:"name"`
	Kind  sandbox.ArtifactKind `json:"kind"`
	Bytes int                  `json:"bytes"`
}
type observation struct {
	Stdout          string         `json:"stdout"`
	Stderr          string         `json:"stderr"`
	StdoutTruncated bool           `json:"stdout_truncated"`
	StderrTruncated bool           `json:"stderr_truncated"`
	Usage           sandbox.Usage  `json:"usage"`
	Limits          sandbox.Limits `json:"limits"`
	Artifacts       []Manifest     `json:"artifacts"`
}
type pythonTool struct{ session *Session }

func (t pythonTool) Spec() tools.Spec { return tools.SpecRunPython }
func (t pythonTool) Execute(ctx context.Context, call tools.Call) (tools.Result, error) {
	var args struct {
		Code             *string         `json:"code"`
		SQLCallID        *string         `json:"sql_call_id"`
		Limits           json.RawMessage `json:"limits"`
		ExtraReadOnly    json.RawMessage `json:"extra_read_only"`
		Network          json.RawMessage `json:"network"`
		RequiresApproval json.RawMessage `json:"requires_approval"`
		Approved         json.RawMessage `json:"approved"`
	}
	d := json.NewDecoder(bytes.NewReader(call.Args))
	d.DisallowUnknownFields()
	if err := d.Decode(&args); err != nil {
		return tools.Result{}, invalidArgs()
	}
	var extra any
	if d.Decode(&extra) != io.EOF || args.Code == nil || strings.TrimSpace(*args.Code) == "" || len(*args.Code) > 1048000 || !utf8.ValidString(*args.Code) || (args.SQLCallID != nil && !validID(*args.SQLCallID, 256)) {
		return tools.Result{}, invalidArgs()
	}
	// 这些字段只为通过解码。是否放行由 Policy 决定，这里不把模型标记当成批准。
	_ = []json.RawMessage{args.Limits, args.ExtraReadOnly, args.Network, args.RequiresApproval, args.Approved}
	limits, mounts, err := allowedSandbox(ctx, call)
	if err != nil {
		return tools.Result{}, err
	}
	s := t.session
	ctx, done, err := s.begin(ctx, call)
	if err != nil {
		return tools.Result{}, err
	}
	defer done()
	spec := sandbox.Spec{
		Code:          *args.Code,
		Runtime:       sandbox.Runtime{Python: s.runtime.Python, PyRoot: s.runtime.Base.Stdlib, Venv: s.runtime.Venv},
		Limits:        limits,
		ExtraReadOnly: mounts,
	}
	if args.SQLCallID != nil {
		s.mu.Lock()
		content, exists := s.sources[*args.SQLCallID]
		s.mu.Unlock()
		if !exists {
			return tools.Result{}, failure(CodeInput, "找不到同一运行中的成功 SQL 调用，请先执行 run_sql")
		}
		spec.Inputs, err = analysisdata.FromToolJSON(ctx, content)
		if err != nil {
			return tools.Result{}, bridge(err)
		}
		spec.Code = "import os as _lantern_os, runpy as _lantern_runpy\nload_table = _lantern_runpy.run_path(_lantern_os.environ['INPUT_DIR'] + '/lantern_table.py')['load_table']\n" + spec.Code
	}
	release, err := s.acquire(ctx, s.runtime)
	if err != nil {
		return tools.Result{}, bridge(err)
	}
	defer release()
	result, err := s.runner.Run(ctx, spec)
	if err != nil {
		return tools.Result{}, bridge(err)
	}
	if ctx.Err() != nil {
		return tools.Result{}, bridge(ctx.Err())
	}
	stdout, outCut := s.cleanOutput(result.Stdout)
	stderr, errCut := s.cleanOutput(result.Stderr)
	// Only bounded text, numeric usage and manifests enter the model observation;
	// retained image bytes never become base64 in the tool transcript.
	manifests, err := s.retain(ctx, result.Artifacts)
	if err != nil {
		return tools.Result{}, err
	}
	body, err := json.Marshal(observation{stdout, stderr, outCut, errCut, result.Usage, result.Limits, manifests})
	if err != nil {
		return tools.Result{}, bridge(err)
	}
	return tools.Result{Content: string(body), Summary: "Python 分析完成"}, nil
}
func invalidArgs() error {
	return &tools.Error{Code: tools.CodeArgumentsInvalid, Retryable: true, Message: "Python 参数不符合默认沙箱契约"}
}

// allowedSandbox 使用和 Agent 相同的 Policy。
// 未确认的超权限、网络和无法识别的字段在进入沙箱前失败。
// 已经带确认单号的 NeedsConfirm 才把资源和额外只读目录写进 Spec；网络永远不写。
func allowedSandbox(ctx context.Context, call tools.Call) (sandbox.Limits, []string, error) {
	if ctx == nil {
		return sandbox.Limits{}, nil, nil
	}
	snapshot := append(json.RawMessage(nil), call.Args...)
	decision, err := policy.Default().Decide(ctx, policy.Request{Tool: policy.ToolRunPython, Args: snapshot})
	if err != nil {
		if ctx.Err() != nil {
			return sandbox.Limits{}, nil, bridge(ctx.Err())
		}
		return sandbox.Limits{}, nil, invalidArgs()
	}
	if decision.Deny || (decision.NeedsConfirm && strings.TrimSpace(call.ApprovalID) == "") {
		return sandbox.Limits{}, nil, invalidArgs()
	}
	if !decision.NeedsConfirm {
		return sandbox.Limits{}, nil, nil
	}
	grant, err := policy.ParseRunPython(snapshot)
	if err != nil || grant.Network {
		return sandbox.Limits{}, nil, invalidArgs()
	}
	cpu, memory, nproc, wall, err := grant.Limits()
	if err != nil {
		return sandbox.Limits{}, nil, invalidArgs()
	}
	return sandbox.Limits{CPU: cpu, Memory: memory, NProc: nproc, Wall: wall}, append([]string(nil), grant.ExtraReadOnly...), nil
}

func (s *Session) cleanOutput(value string) (string, bool) {
	for _, path := range []string{s.dir, s.runtime.Venv, s.runtime.Base.Stdlib, s.runtime.Base.Path, s.runtime.Python, s.runtime.Base.Prefix} {
		if path != "" && path != "/" {
			value = strings.ReplaceAll(value, path, "[runtime]")
		}
	}
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= 8192 {
		return value, false
	}
	cut := 8192
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut], true
}

func (s *Session) retain(ctx context.Context, artifacts []sandbox.Artifact) ([]Manifest, error) {
	if len(artifacts) > 16 {
		return nil, failure(CodeBudget, "产物批次超过数量预算")
	}
	manifests := make([]Manifest, 0, len(artifacts))
	pending := make(map[string]sandbox.Artifact, len(artifacts))
	total := 0
	for _, artifact := range artifacts {
		if err := sandbox.ValidateArtifact(artifact, 0); err != nil {
			return nil, bridge(err)
		}
		total += len(artifact.Data)
		if total > 20<<20 {
			return nil, failure(CodeBudget, "产物批次超过字节预算")
		}
		var token [20]byte
		if _, err := rand.Read(token[:]); err != nil {
			return nil, bridge(err)
		}
		id := hex.EncodeToString(token[:])
		if _, exists := pending[id]; exists {
			return nil, bridge(errors.New("artifact identifier collision within batch"))
		}
		artifact.Data = append([]byte(nil), artifact.Data...)
		pending[id] = artifact
		manifests = append(manifests, Manifest{id, artifact.Name, artifact.Kind, len(artifact.Data)})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, failure(CodeClosed, "分析会话已关闭")
	}
	if ctx.Err() != nil {
		return nil, bridge(ctx.Err())
	}
	if s.artifactBytes+total > MaxArtifactBytes {
		return nil, failure(CodeBudget, "产物保留预算已满，请取回图表后关闭当前会话")
	}
	for id := range pending {
		if _, exists := s.artifacts[id]; exists {
			return nil, bridge(errors.New("artifact identifier collision"))
		}
	}
	for id, artifact := range pending {
		s.artifacts[id] = artifact
	}
	s.artifactBytes += total
	return manifests, nil
}

// GetArtifact returns a complete independent copy after Agent.Run and before
// Close. IDs are opaque; both workspace and run ownership are checked again.
func (s *Session) GetArtifact(workspaceID, runID, id string) (sandbox.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sandbox.Artifact{}, failure(CodeClosed, "分析会话已关闭")
	}
	if workspaceID != s.workspaceID || runID != s.runID {
		return sandbox.Artifact{}, failure(CodeScope, "产物不属于当前工作区和运行")
	}
	artifact, exists := s.artifacts[id]
	if !exists {
		return sandbox.Artifact{}, failure(CodeArtifact, "产物不存在")
	}
	artifact.Data = append([]byte(nil), artifact.Data...)
	return artifact, nil
}
