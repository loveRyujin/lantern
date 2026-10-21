package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
)

const (
	// DefaultModel 是 grok-4.6 在 xAI Chat Completions 上的模型 ID。
	DefaultModel = "grok-4.6"
	// DefaultBaseURL 是 grok API 的默认根地址，不含 /chat/completions。
	DefaultBaseURL = "https://api.x.ai/v1"
	// DefaultSecretName 是 SecretStore 里 grok API Key 的默认名称。
	DefaultSecretName = "grok_api_key"
	// EnvAPIKey 是官方环境变量名。
	EnvAPIKey = "XAI_API_KEY"
	// EnvAPIKeyAlias 是评测脚本里常见的别名，仅在 XAI_API_KEY 为空时使用。
	EnvAPIKeyAlias = "GROK_API_KEY"

	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"

	ToolChoiceAuto     = "auto"
	ToolChoiceNone     = "none"
	ToolChoiceRequired = "required"

	FinishStop      = "stop"
	FinishToolCalls = "tool_calls"
	FinishLength    = "length"

	ollamaUnreachableMessage = "Ollama 不可达：无法连接 localhost:11434"
)

// Provider 是一轮对话补全。Core、桌面端和评测 CLI 都依赖它。
type Provider interface {
	// ID 标识实现，例如 "grok:grok-4.6" 或 "mock"。
	ID() string
	// Chat 完成一轮补全。请求流式输出时，增量经 Request.OnDelta 送出，返回值仍是拼好的完整结果。
	// 调用方必须给 ctx 设置截止时间。默认 HTTP 客户端没有超时，避免把推理模型的长耗时误判成失败。
	Chat(ctx context.Context, req Request) (Response, error)
}

// SecretStore 按名称读取密钥。桌面端用 mygo.Secrets 做适配，评测可以用环境变量或 [MapSecrets]。
type SecretStore interface {
	Get(ctx context.Context, name string) (string, error)
}

// Role 是消息角色，取值与 Chat Completions 一致。
type Role string

// Message 是一条对话消息。工具参数 Arguments 是原始 JSON 文本，由调用方校验。
type Message struct {
	Role             Role       `json:"role"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	Name             string     `json:"name,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall 是模型请求执行的一个函数调用。
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool 是一次请求里声明的函数工具。Parameters 是 JSON Schema，根节点应为 object。
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Request 是一轮补全请求。
type Request struct {
	Messages    []Message
	Tools       []Tool
	ToolChoice  string
	Temperature *float64
	// MaxTokens 映射为 max_completion_tokens。0 表示不传，由模型自己停。
	MaxTokens int
	// Stream 为 true 时，grok 使用 SSE。Mock 会按脚本把增量交给 OnDelta。
	Stream bool
	// OnDelta 非空时也会走流式路径，即使 Stream 为 false。
	OnDelta func(Delta) error
}

// Delta 是一次流式增量。工具参数可能被拆成多段，由实现拼接成最终的 ToolCall。
type Delta struct {
	Content          string             `json:"content,omitempty"`
	ReasoningContent string             `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCallFragment `json:"tool_calls,omitempty"`
	FinishReason     string             `json:"finish_reason,omitempty"`
}

// ToolCallFragment 是 SSE 里的一段 tool_call。Index 用来把多段拼回同一次调用。
type ToolCallFragment struct {
	Index     int    `json:"index"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

func (d Delta) empty() bool {
	return d.Content == "" && d.ReasoningContent == "" && len(d.ToolCalls) == 0 && d.FinishReason == ""
}

// Response 是一轮补全的完整结果。
type Response struct {
	ID           string  `json:"id"`
	Model        string  `json:"model"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason,omitempty"`
	Usage        Usage   `json:"usage"`
}

// Usage 是本轮 token 用量。ReasoningTokens 来自 grok 的 completion_tokens_details。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
}

func (r Request) wantsStream() bool {
	return r.Stream || r.OnDelta != nil
}

func validateRequest(req Request) error {
	if len(req.Messages) == 0 {
		return invalidRequest("messages 不能为空")
	}
	for i, m := range req.Messages {
		if !knownRole(m.Role) {
			return invalidRequest("第 %d 条消息的 role 无效", i+1)
		}
		switch m.Role {
		case RoleSystem, RoleUser:
			if m.ToolCallID != "" || len(m.ToolCalls) > 0 {
				return invalidRequest("第 %d 条消息不能携带 tool_call", i+1)
			}
		case RoleAssistant:
			for _, tc := range m.ToolCalls {
				if tc.ID == "" || tc.Name == "" {
					return invalidRequest("第 %d 条消息的 tool_call 缺少 id 或 name", i+1)
				}
			}
		case RoleTool:
			if m.ToolCallID == "" {
				return invalidRequest("第 %d 条 tool 消息缺少 tool_call_id", i+1)
			}
			if len(m.ToolCalls) > 0 {
				return invalidRequest("第 %d 条 tool 消息不能再发起 tool_call", i+1)
			}
		}
	}
	seen := make(map[string]struct{}, len(req.Tools))
	for _, tool := range req.Tools {
		if tool.Name == "" {
			return invalidRequest("工具名不能为空")
		}
		if _, ok := seen[tool.Name]; ok {
			return invalidRequest("工具名重复: %s", tool.Name)
		}
		seen[tool.Name] = struct{}{}
		if len(bytes.TrimSpace(tool.Parameters)) > 0 && !json.Valid(tool.Parameters) {
			return invalidRequest("工具 %s 的 parameters 不是合法 JSON", tool.Name)
		}
	}
	if req.MaxTokens < 0 {
		return invalidRequest("max_tokens 不能为负")
	}
	if req.Temperature != nil && math.IsNaN(*req.Temperature) {
		return invalidRequest("temperature 无效")
	}
	return nil
}

func knownRole(role Role) bool {
	switch role {
	case RoleSystem, RoleUser, RoleAssistant, RoleTool:
		return true
	default:
		return false
	}
}

func classifyContext(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{Code: CodeTimeout, Retryable: true, Message: "请求超时", Err: err}
	case errors.Is(err, context.Canceled):
		return &Error{Code: CodeCanceled, Message: "请求已取消", Err: err}
	default:
		return &Error{Code: CodeCanceled, Message: "请求已取消", Err: err}
	}
}

// MapSecrets 是进程内的 SecretStore，测试和评测脚本用。
type MapSecrets map[string]string

func (m MapSecrets) Get(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", classifyContext(err)
	}
	if name == "" {
		return "", invalidRequest("密钥名为空")
	}
	v, ok := m[name]
	if !ok {
		return "", &Error{Code: CodeSecretNotFound, Message: "未找到密钥 " + name}
	}
	return v, nil
}

// EnvSecrets 从环境变量读取密钥，变量名就是 Get 的 name。
type EnvSecrets struct {
	Lookup func(string) (string, bool)
}

func (e EnvSecrets) Get(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", classifyContext(err)
	}
	if name == "" {
		return "", invalidRequest("密钥名为空")
	}
	lookup := e.Lookup
	if lookup == nil {
		lookup = envLookup
	}
	v, ok := lookup(name)
	if !ok || v == "" {
		return "", &Error{Code: CodeSecretNotFound, Message: "未找到密钥 " + name}
	}
	return v, nil
}

var (
	_ Provider    = (*Grok)(nil)
	_ Provider    = (*Mock)(nil)
	_ SecretStore = MapSecrets(nil)
	_ SecretStore = EnvSecrets{}
)
