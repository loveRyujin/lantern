package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Spec 是注册时冻结的工具说明。是否需要确认不在这里，由 Policy 决定。
type Spec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Timeout     time.Duration
	// MaxAttempts 是执行失败时的尝试次数，含第一次。小于 1 时按 1 次。
	// 这是工具自己的重试，不计入运行级的连续参数失败。
	MaxAttempts int
	Backoff     time.Duration
}

// Call 是已经通过校验的一次调用。Args 是模型给出的原始 JSON。
type Call struct {
	ID          string
	Name        string
	Args        json.RawMessage
	Hash        string
	RunID       string
	WorkspaceID string
	// ApprovalID、StepID、ArgsHash 是执行前消费确认单用的当前步骤。
	// 导出工具把它们交给 ConfirmExport，不能从确认单记录里反推。
	ApprovalID string
	StepID     string
	ArgsHash   string
}

// Result 是工具的观察结果。Summary 会进入 run_events，不要把参数原文放进去。
type Result struct {
	Content string
	Summary string
}

// Tool 是一个可注册的工具。Execute 只会在参数校验、策略和确认都通过之后被调用。
// 传入的 Args 已经通过 JSON 和 Schema 校验。工具自己的业务校验（例如 SQL 只读）放在 Execute 里。
type Tool interface {
	Spec() Spec
	Execute(ctx context.Context, call Call) (Result, error)
}

// CallInput 是模型输出的一次工具调用，还没校验。
type CallInput struct {
	ID           string
	Name         string
	Arguments    string
	FinishReason string
	// Truncated 是 finish_reason 之外的截断信号，例如流在结束前断开。
	Truncated bool
}

// Prepared 是校验通过、还没执行的调用。
type Prepared struct {
	ID   string
	Name string
	Args []byte
	Hash string
	Tool Tool
	Spec Spec
}

type entry struct {
	tool   Tool
	spec   Spec
	schema *jsonschema.Schema
}

// Registry 按名字保存工具。同名再次注册返回错误，不覆盖。
type Registry struct {
	mu    sync.Mutex
	tools map[string]*entry
}

// Register 编译 Schema 并放入注册表。名字必须完全一致，前后空白和大小写都算另一个名字。
func (r *Registry) Register(tool Tool) error {
	if r == nil {
		return fmt.Errorf("tools: 注册表是 nil")
	}
	if tool == nil {
		return fmt.Errorf("tools: 工具是 nil")
	}
	spec := tool.Spec()
	if spec.Name == "" || spec.Name != strings.TrimSpace(spec.Name) {
		return fmt.Errorf("tools: 工具名不合法")
	}
	schema, err := compileSchema(spec.Name, spec.Parameters)
	if err != nil {
		return fmt.Errorf("tools: 工具 %s 的 Schema 无法编译: %w", spec.Name, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tools == nil {
		r.tools = map[string]*entry{}
	}
	if _, exists := r.tools[spec.Name]; exists {
		return fmt.Errorf("tools: 工具 %q 已注册", spec.Name)
	}
	copied := spec
	copied.Parameters = append(json.RawMessage(nil), spec.Parameters...)
	r.tools[spec.Name] = &entry{tool: tool, spec: copied, schema: schema}
	return nil
}

// Specs 按名字排序返回已注册工具的说明，给模型请求声明工具用。
func (r *Registry) Specs() []Spec {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Spec, 0, len(r.tools))
	for _, item := range r.tools {
		out = append(out, item.spec)
	}
	slices.SortFunc(out, func(a, b Spec) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Prepare 按固定顺序检查一次调用：截断 → JSON → 对象 → Schema。
// 截断时不解析，即使参数文本碰巧是合法 JSON。
// 未知工具名在 JSON 合法之后返回 tool_not_found。它计入连续失败，但不执行。
func (r *Registry) Prepare(in CallInput) (Prepared, error) {
	if in.Truncated || TruncatedFinish(in.FinishReason) {
		return Prepared{}, incomplete("参数因输出截断而不完整")
	}
	if len(in.Arguments) > MaxArgumentBytes {
		return Prepared{}, invalidArgs("参数超过大小上限")
	}
	value, err := parseJSON([]byte(in.Arguments))
	if err != nil {
		return Prepared{}, err
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return Prepared{}, invalidArgs("参数必须是 JSON 对象")
	}
	if !jsonNumbersOK(obj) {
		return Prepared{}, invalidArgs("参数里的数字无法表示")
	}
	if r == nil {
		return Prepared{}, &Error{Code: CodeToolNotFound, Retryable: true, Message: "没有注册工具 " + in.Name}
	}
	r.mu.Lock()
	item := r.tools[in.Name]
	r.mu.Unlock()
	if item == nil {
		return Prepared{}, &Error{Code: CodeToolNotFound, Retryable: true, Message: "没有注册工具 " + in.Name}
	}
	if err := validateSchema(item.schema, obj); err != nil {
		return Prepared{}, err
	}
	raw := []byte(in.Arguments)
	sum, err := approval.ParamsHash(raw)
	if err != nil {
		return Prepared{}, invalidArgs("参数不是合法 JSON")
	}
	return Prepared{
		ID:   in.ID,
		Name: in.Name,
		Args: append([]byte(nil), raw...),
		Hash: sum,
		Tool: item.tool,
		Spec: item.spec,
	}, nil
}

// PrepareRaw 重新校验一份参数，给批准时带上的替换参数用。
// 不看 finish_reason。Schema 或 JSON 失败时不得执行。
func (r *Registry) PrepareRaw(name string, args []byte) (Prepared, error) {
	return r.Prepare(CallInput{Name: name, Arguments: string(args)})
}
