package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
)

// 故障类型由 Mock 注入，评测用同一套错误码断言。
const (
	FaultTimeout           = "timeout"
	FaultHTTP5xx           = "http_5xx"
	FaultInvalidJSON       = "invalid_json"
	FaultOllamaUnreachable = "ollama_unreachable"
	FaultStreamTruncated   = "stream_truncated"
)

const maxScriptBytes = 8 << 20

// When 描述这一步期望的请求。为空的字段不参与匹配。
// 不匹配的步骤仍会被消费，脚本不会停在这一步重试。
type When struct {
	Contains string
	Tool     string
	Role     Role
}

// Fault 是一步注入的故障。Delay 只在返回故障前等待，ctx 先结束则返回 ctx 的错误。
// 故障可以和 Deltas 一起写：流式请求先把这些增量回调出去，再返回故障。
type Fault struct {
	Kind    string
	Status  int
	Message string
	Delay   time.Duration
}

// Step 是脚本中的一步。Fault 不能和最终回复字段一起写，但可以附带 Deltas。
// Content 非空时作为最终正文；否则把 Deltas 的正文拼起来。
// 两者都有时，最终正文用 Content，流式回调仍逐个发送 Deltas。
type Step struct {
	When             *When
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	Deltas           []Delta
	FinishReason     string
	Usage            Usage
	Fault            *Fault
}

// Script 按调用顺序消费步骤。可以并发调用，游标由互斥锁保护。
type Script struct {
	mu     sync.Mutex
	steps  []Step
	cursor int
}

// NewScript 校验并复制步骤。调用方之后修改入参不会影响脚本。
func NewScript(steps ...Step) (*Script, error) {
	cloned := make([]Step, len(steps))
	for i, step := range steps {
		if err := validateStep(i, step); err != nil {
			return nil, err
		}
		cloned[i] = cloneStep(step)
	}
	return &Script{steps: cloned}, nil
}

// MustNewScript 在步骤非法时 panic，适合写死在测试和评测里的脚本。
func MustNewScript(steps ...Step) *Script {
	s, err := NewScript(steps...)
	if err != nil {
		panic(err)
	}
	return s
}

// ParseScript 解析 JSON 脚本。未知字段会失败，避免拼写错误被静默忽略。
func ParseScript(data []byte) (*Script, error) {
	data = bytes.TrimSpace(data)
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if len(data) > maxScriptBytes {
		return nil, invalidScript("脚本过大")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc struct {
		Steps json.RawMessage `json:"steps"`
	}
	if err := dec.Decode(&doc); err != nil {
		return nil, scriptJSONError(err)
	}
	var extra any
	if err := dec.Decode(&extra); !errorsIsEOF(err) {
		if err == nil {
			return nil, invalidScript("脚本末尾有多余内容")
		}
		return nil, scriptJSONError(err)
	}
	if len(bytes.TrimSpace(doc.Steps)) == 0 || bytes.Equal(bytes.TrimSpace(doc.Steps), []byte("null")) {
		return nil, invalidScript("脚本缺少 steps")
	}
	stepDec := json.NewDecoder(bytes.NewReader(doc.Steps))
	stepDec.DisallowUnknownFields()
	var raws []json.RawMessage
	if err := stepDec.Decode(&raws); err != nil {
		return nil, scriptJSONError(err)
	}
	steps := make([]Step, len(raws))
	for i, raw := range raws {
		step, err := parseStep(i, raw)
		if err != nil {
			return nil, err
		}
		steps[i] = step
	}
	return NewScript(steps...)
}

// LoadScript 从 r 读取 JSON 脚本，最大 8 MiB。
func LoadScript(r io.Reader) (*Script, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxScriptBytes+1))
	if err != nil {
		return nil, &Error{Code: CodeInvalidScript, Message: "读取脚本失败", Err: err}
	}
	return ParseScript(data)
}

// MarshalJSON 写出完整脚本，与游标位置无关。delay 精确到毫秒。
func (s *Script) MarshalJSON() ([]byte, error) {
	if s == nil {
		return []byte("null"), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dto := scriptDTO{Steps: make([]stepDTO, len(s.steps))}
	for i, step := range s.steps {
		dto.Steps[i] = step.toDTO()
	}
	return json.Marshal(dto)
}

// Reset 把游标拨回第一步。
func (s *Script) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cursor = 0
	s.mu.Unlock()
}

// Clone 复制步骤并使用新的游标，原脚本的进度保持不变。
func (s *Script) Clone() *Script {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return &Script{steps: cloneSteps(s.steps)}
}

// Say 构造一条按片段流式输出的文本回复。
func Say(parts ...string) Step {
	step := Step{FinishReason: FinishStop, Deltas: make([]Delta, 0, len(parts))}
	var b strings.Builder
	for _, part := range parts {
		step.Deltas = append(step.Deltas, Delta{Content: part})
		b.WriteString(part)
	}
	step.Content = b.String()
	return step
}

// CallTools 构造一条只包含 tool_calls 的回复。
func CallTools(calls ...ToolCall) Step {
	return Step{ToolCalls: slices.Clone(calls), FinishReason: FinishToolCalls}
}

// Inject 构造一种立即返回的故障。需要状态码或延迟时直接写 [Fault]。
func Inject(kind string) Step {
	return Step{Fault: &Fault{Kind: kind}}
}

func (s *Script) next(req Request) (Step, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursor >= len(s.steps) {
		return Step{}, 0, &Error{
			Code:    CodeScriptExhausted,
			Message: fmt.Sprintf("脚本已用尽（共 %d 步）", len(s.steps)),
		}
	}
	idx := s.cursor
	s.cursor++
	step := s.steps[idx]
	if !step.matches(req) {
		return Step{}, 0, &Error{Code: CodeScriptMismatch, Message: mismatchMessage(idx+1, step.When)}
	}
	return cloneStep(step), idx + 1, nil
}

func (s Step) matches(req Request) bool {
	if s.When == nil {
		return true
	}
	if len(req.Messages) == 0 {
		return false
	}
	last := req.Messages[len(req.Messages)-1]
	if s.When.Role != "" && last.Role != s.When.Role {
		return false
	}
	if s.When.Contains != "" && !strings.Contains(last.Content, s.When.Contains) {
		return false
	}
	if s.When.Tool != "" {
		found := false
		for _, tool := range req.Tools {
			if tool.Name == s.When.Tool {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func mismatchMessage(n int, when *When) string {
	if when == nil {
		return fmt.Sprintf("脚本第 %d 步不匹配", n)
	}
	var parts []string
	if when.Role != "" {
		parts = append(parts, fmt.Sprintf("最后一条消息角色为 %s", when.Role))
	}
	if when.Contains != "" {
		parts = append(parts, fmt.Sprintf("最后一条消息包含 %q", when.Contains))
	}
	if when.Tool != "" {
		parts = append(parts, fmt.Sprintf("请求声明工具 %q", when.Tool))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("脚本第 %d 步不匹配", n)
	}
	return fmt.Sprintf("脚本第 %d 步不匹配：需要%s", n, strings.Join(parts, "，"))
}

func (f *Fault) asError() error {
	if f == nil {
		return invalidScript("故障为空")
	}
	switch f.Kind {
	case FaultTimeout:
		msg := f.Message
		if msg == "" {
			msg = "注入超时"
		}
		return &Error{Code: CodeTimeout, Retryable: true, Message: msg}
	case FaultHTTP5xx:
		status := f.Status
		if status == 0 {
			status = 500
		}
		return httpStatusError(status, f.Message)
	case FaultInvalidJSON:
		msg := f.Message
		if msg == "" {
			msg = "响应不是合法 JSON"
		}
		return &Error{Code: CodeInvalidJSON, Message: msg}
	case FaultOllamaUnreachable:
		msg := f.Message
		if msg == "" {
			msg = ollamaUnreachableMessage
		}
		return &Error{Code: CodeOllamaUnreachable, Message: msg}
	case FaultStreamTruncated:
		return streamTruncated(f.Message)
	default:
		return invalidScript("未知故障类型 %s", f.Kind)
	}
}

func validateStep(i int, step Step) error {
	n := i + 1
	if step.When != nil && step.When.Role != "" && !knownRole(step.When.Role) {
		return invalidScript("第 %d 步的 when.role 无效", n)
	}
	if step.Fault != nil {
		if step.Content != "" || step.ReasoningContent != "" || len(step.ToolCalls) > 0 || step.FinishReason != "" || step.Usage != (Usage{}) {
			return invalidScript("第 %d 步同时写了故障和正常回复", n)
		}
		if err := validateFault(n, step.Fault); err != nil {
			return err
		}
		for _, d := range step.Deltas {
			for _, frag := range d.ToolCalls {
				if frag.Index < 0 {
					return invalidScript("第 %d 步的 tool_call 下标不能为负", n)
				}
			}
		}
		return nil
	}
	if step.Content == "" && step.ReasoningContent == "" && len(step.ToolCalls) == 0 && len(step.Deltas) == 0 && step.FinishReason == "" {
		return invalidScript("第 %d 步没有回复", n)
	}
	for _, tc := range step.ToolCalls {
		if tc.ID == "" || tc.Name == "" {
			return invalidScript("第 %d 步的 tool_call 缺少 id 或 name", n)
		}
	}
	for _, d := range step.Deltas {
		for _, frag := range d.ToolCalls {
			if frag.Index < 0 {
				return invalidScript("第 %d 步的 tool_call 下标不能为负", n)
			}
		}
	}
	return nil
}

func validateFault(n int, f *Fault) error {
	if f.Delay < 0 {
		return invalidScript("第 %d 步的 delay 不能为负", n)
	}
	switch f.Kind {
	case FaultTimeout, FaultInvalidJSON, FaultOllamaUnreachable, FaultStreamTruncated:
		return nil
	case FaultHTTP5xx:
		if f.Status != 0 && (f.Status < 500 || f.Status > 599) {
			return invalidScript("第 %d 步的 http_5xx 状态码必须在 500 到 599", n)
		}
		return nil
	default:
		return invalidScript("第 %d 步的故障类型无效: %s", n, f.Kind)
	}
}

func cloneSteps(in []Step) []Step {
	if in == nil {
		return nil
	}
	out := make([]Step, len(in))
	for i := range in {
		out[i] = cloneStep(in[i])
	}
	return out
}

func cloneStep(step Step) Step {
	out := step
	out.ToolCalls = cloneToolCalls(step.ToolCalls)
	out.Deltas = cloneDeltas(step.Deltas)
	if step.When != nil {
		w := *step.When
		out.When = &w
	}
	if step.Fault != nil {
		f := *step.Fault
		out.Fault = &f
	}
	return out
}

func cloneToolCalls(in []ToolCall) []ToolCall {
	if in == nil {
		return nil
	}
	return slices.Clone(in)
}

func cloneDeltas(in []Delta) []Delta {
	if in == nil {
		return nil
	}
	out := slices.Clone(in)
	for i := range out {
		if in[i].ToolCalls != nil {
			out[i].ToolCalls = slices.Clone(in[i].ToolCalls)
		}
	}
	return out
}

type scriptDTO struct {
	Steps []stepDTO `json:"steps"`
}

type stepDTO struct {
	When             *whenDTO   `json:"when,omitempty"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	Deltas           []Delta    `json:"deltas,omitempty"`
	FinishReason     string     `json:"finish_reason,omitempty"`
	Usage            *Usage     `json:"usage,omitempty"`
	Fault            *faultDTO  `json:"fault,omitempty"`
}

type whenDTO struct {
	Contains string `json:"contains,omitempty"`
	Tool     string `json:"tool,omitempty"`
	Role     string `json:"role,omitempty"`
}

type faultDTO struct {
	Kind    string `json:"kind"`
	Status  int    `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
	DelayMS int    `json:"delay_ms,omitempty"`
}

func (s Step) toDTO() stepDTO {
	dto := stepDTO{
		Content:          s.Content,
		ReasoningContent: s.ReasoningContent,
		ToolCalls:        cloneToolCalls(s.ToolCalls),
		Deltas:           cloneDeltas(s.Deltas),
		FinishReason:     s.FinishReason,
	}
	if s.Usage != (Usage{}) {
		u := s.Usage
		dto.Usage = &u
	}
	if s.When != nil {
		dto.When = &whenDTO{Contains: s.When.Contains, Tool: s.When.Tool, Role: string(s.When.Role)}
	}
	if s.Fault != nil {
		dto.Fault = &faultDTO{
			Kind:    s.Fault.Kind,
			Status:  s.Fault.Status,
			Message: s.Fault.Message,
			DelayMS: int(s.Fault.Delay / time.Millisecond),
		}
	}
	return dto
}

func (d stepDTO) step() Step {
	step := Step{
		Content:          d.Content,
		ReasoningContent: d.ReasoningContent,
		ToolCalls:        d.ToolCalls,
		Deltas:           d.Deltas,
		FinishReason:     d.FinishReason,
	}
	if d.Usage != nil {
		step.Usage = *d.Usage
	}
	if d.When != nil {
		step.When = &When{Contains: d.When.Contains, Tool: d.When.Tool, Role: Role(d.When.Role)}
	}
	if d.Fault != nil {
		step.Fault = &Fault{
			Kind:    d.Fault.Kind,
			Status:  d.Fault.Status,
			Message: d.Fault.Message,
			Delay:   time.Duration(d.Fault.DelayMS) * time.Millisecond,
		}
	}
	return step
}

func parseStep(i int, raw json.RawMessage) (Step, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return Step{}, invalidScript("第 %d 步是空的", i+1)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var dto stepDTO
	if err := dec.Decode(&dto); err != nil {
		return Step{}, scriptJSONError(err)
	}
	return dto.step(), nil
}

func scriptJSONError(err error) *Error {
	return &Error{Code: CodeInvalidScript, Message: "脚本无法解析: " + err.Error(), Err: err}
}

func errorsIsEOF(err error) bool {
	return err == io.EOF
}
