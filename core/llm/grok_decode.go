package llm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

func (g *Grok) decode(resp *http.Response, stream bool, on func(Delta) error) (Response, error) {
	if resp.StatusCode != http.StatusOK {
		return Response{}, readStatusError(resp)
	}
	if !stream {
		out, err := decodeJSON(io.LimitReader(resp.Body, maxJSONBody+1))
		if err != nil {
			return Response{}, err
		}
		if err := rejectIncompleteTools(out); err != nil {
			return Response{}, err
		}
		return g.finish(out), nil
	}
	limited := &byteLimit{r: resp.Body, lim: maxStreamBody}
	br := bufio.NewReaderSize(limited, 64*1024)
	peek, _ := br.Peek(128)
	if !looksLikeSSE(resp.Header.Get("Content-Type"), peek) {
		out, err := decodeJSON(br)
		if err != nil {
			return Response{}, err
		}
		if err := rejectIncompleteTools(out); err != nil {
			return Response{}, err
		}
		out = g.finish(out)
		if err := emitFinal(out, on); err != nil {
			return Response{}, err
		}
		return out, nil
	}
	out, err := decodeSSE(br, on)
	if err != nil {
		return Response{}, err
	}
	return g.finish(out), nil
}

func (g *Grok) finish(resp Response) Response {
	if resp.Message.Role == "" {
		resp.Message.Role = RoleAssistant
	}
	if resp.Model == "" {
		resp.Model = g.modelName()
	}
	return resp
}

type byteLimit struct {
	r   io.Reader
	n   int64
	lim int64
}

func (b *byteLimit) Read(p []byte) (int, error) {
	if b.n >= b.lim {
		return 0, errBodyTooLarge
	}
	if int64(len(p)) > b.lim-b.n {
		p = p[:b.lim-b.n]
	}
	n, err := b.r.Read(p)
	b.n += int64(n)
	return n, err
}

func looksLikeSSE(contentType string, peek []byte) bool {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return true
	}
	trim := bytes.TrimSpace(peek)
	if len(trim) == 0 {
		return len(peek) == 0
	}
	return trim[0] == ':' || bytes.HasPrefix(trim, []byte("data:")) || bytes.HasPrefix(trim, []byte("event:"))
}

func readStatusError(resp *http.Response) *Error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return httpStatusError(resp.StatusCode, "")
	}
	return httpStatusError(resp.StatusCode, clip(parseAPIErrorMessage(body)))
}

func parseAPIErrorMessage(body []byte) string {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return ""
	}
	var top struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return string(body)
	}
	trimmed := bytes.TrimSpace(top.Error)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if trimmed[0] == '"' {
			var s string
			if json.Unmarshal(trimmed, &s) == nil && s != "" {
				return s
			}
		}
		var obj struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(trimmed, &obj) == nil && obj.Message != "" {
			return obj.Message
		}
	}
	if top.Message != "" {
		return top.Message
	}
	return string(body)
}

func errorFromRaw(raw json.RawMessage) *Error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return &Error{Code: CodeHTTP, Message: "上游返回错误"}
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return &Error{Code: CodeHTTP, Message: clip(s)}
		}
	}
	var obj struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(trimmed, &obj); err == nil && (obj.Message != "" || obj.Type != "") {
		msg := obj.Message
		if msg == "" {
			msg = "上游返回错误"
		}
		e := &Error{Message: clip(msg)}
		switch obj.Type {
		case "server_error", "internal_error", "overloaded":
			e.Code = CodeLLM5xx
			e.Retryable = true
		default:
			e.Code = CodeHTTP
		}
		return e
	}
	return &Error{Code: CodeHTTP, Message: clip(string(trimmed))}
}

func decodeJSON(r io.Reader) (Response, error) {
	body, err := io.ReadAll(r)
	if errors.Is(err, errBodyTooLarge) || len(body) > maxJSONBody {
		return Response{}, &Error{Code: CodeInvalidResponse, Message: "响应体过大"}
	}
	if err != nil {
		return Response{}, classifyTransport(err)
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || !json.Valid(body) {
		return Response{}, &Error{Code: CodeInvalidJSON, Message: "响应不是合法 JSON: " + clip(string(body))}
	}
	var probe struct {
		Error   json.RawMessage   `json:"error"`
		Choices []json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return Response{}, &Error{Code: CodeInvalidJSON, Message: "响应不是合法 JSON", Err: err}
	}
	if len(bytes.TrimSpace(probe.Error)) > 0 && !bytes.Equal(bytes.TrimSpace(probe.Error), []byte("null")) && len(probe.Choices) == 0 {
		return Response{}, errorFromRaw(probe.Error)
	}
	var parsed wireCompletion
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Response{}, &Error{Code: CodeInvalidJSON, Message: "响应不是合法 JSON", Err: err}
	}
	if len(parsed.Choices) == 0 {
		return Response{}, &Error{Code: CodeEmptyResponse, Message: "响应没有 choices"}
	}
	return parsed.response(), nil
}

func decodeSSE(r io.Reader, on func(Delta) error) (Response, error) {
	var acc accumulator
	sawDone := false
	err := readSSE(r, func(payload string) error {
		var chunk wireChunk
		if jerr := json.Unmarshal([]byte(payload), &chunk); jerr != nil {
			return &Error{Code: CodeInvalidJSON, Message: "SSE 事件不是合法 JSON: " + clip(payload), Err: jerr}
		}
		if len(bytes.TrimSpace(chunk.Error)) > 0 && !bytes.Equal(bytes.TrimSpace(chunk.Error), []byte("null")) {
			return errorFromRaw(chunk.Error)
		}
		if chunk.ID != "" {
			acc.id = chunk.ID
		}
		if chunk.Model != "" {
			acc.model = chunk.Model
		}
		if chunk.Usage != nil {
			acc.usage = chunk.Usage.usage()
		}
		if len(chunk.Choices) == 0 {
			return nil
		}
		delta := acc.apply(chunk.Choices[0])
		if on != nil && !delta.empty() {
			return on(delta)
		}
		return nil
	})
	if errors.Is(err, errStreamEnd) {
		sawDone = true
		err = nil
	}
	if errors.Is(err, errLineTooLong) {
		return Response{}, &Error{Code: CodeInvalidResponse, Message: "SSE 行过长"}
	}
	if errors.Is(err, errBodyTooLarge) {
		return Response{}, &Error{Code: CodeInvalidResponse, Message: "响应体过大"}
	}
	if err != nil {
		var le *Error
		if errors.As(err, &le) {
			return Response{}, err
		}
		return Response{}, classifyTransport(err)
	}
	// 连接在结束标记之前断开。已经回调过的增量不算成功结果。
	if !sawDone && acc.finish == "" {
		return Response{}, streamTruncated("")
	}
	if !acc.usable() {
		return Response{}, &Error{Code: CodeEmptyResponse, Message: "响应没有内容"}
	}
	out := acc.response()
	if err := rejectIncompleteTools(out); err != nil {
		return Response{}, err
	}
	return out, nil
}

func rejectIncompleteTools(resp Response) error {
	for _, tc := range resp.Message.ToolCalls {
		if strings.TrimSpace(tc.ID) == "" || strings.TrimSpace(tc.Name) == "" {
			return &Error{Code: CodeInvalidResponse, Message: "tool_call 缺少 id 或 name"}
		}
	}
	return nil
}

func emitFinal(resp Response, on func(Delta) error) error {
	if on == nil {
		return nil
	}
	if resp.Message.Content != "" || resp.Message.ReasoningContent != "" {
		if err := on(Delta{Content: resp.Message.Content, ReasoningContent: resp.Message.ReasoningContent}); err != nil {
			return err
		}
	}
	for i, tc := range resp.Message.ToolCalls {
		frag := ToolCallFragment{Index: i, ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}
		if err := on(Delta{ToolCalls: []ToolCallFragment{frag}}); err != nil {
			return err
		}
	}
	if resp.FinishReason != "" {
		if err := on(Delta{FinishReason: resp.FinishReason}); err != nil {
			return err
		}
	}
	return nil
}

type accumulator struct {
	id        string
	model     string
	content   strings.Builder
	reasoning strings.Builder
	tools     map[int]*toolBuf
	finish    string
	usage     Usage
	sawChoice bool
}

func (a *accumulator) apply(ch wireChoice) Delta {
	a.sawChoice = true
	var delta Delta
	if ch.Delta != nil {
		if ch.Delta.Content != nil {
			delta.Content = *ch.Delta.Content
			a.content.WriteString(delta.Content)
		}
		if ch.Delta.ReasoningContent != nil {
			delta.ReasoningContent = *ch.Delta.ReasoningContent
			a.reasoning.WriteString(delta.ReasoningContent)
		}
		for _, tc := range ch.Delta.ToolCalls {
			frag := ToolCallFragment{
				Index:     tc.Index,
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			}
			delta.ToolCalls = append(delta.ToolCalls, frag)
			if a.tools == nil {
				a.tools = map[int]*toolBuf{}
			}
			buf := toolBufFor(a.tools, tc.Index)
			if tc.ID != "" {
				buf.id = tc.ID
			}
			buf.addName(tc.Function.Name)
			buf.args.WriteString(tc.Function.Arguments)
		}
	}
	if ch.FinishReason != nil && *ch.FinishReason != "" {
		delta.FinishReason = *ch.FinishReason
		a.finish = *ch.FinishReason
	}
	return delta
}

func (a accumulator) usable() bool {
	return a.sawChoice && (a.content.Len() > 0 || a.reasoning.Len() > 0 || len(a.tools) > 0 || a.finish != "")
}

func (a accumulator) response() Response {
	return Response{
		ID:    a.id,
		Model: a.model,
		Message: Message{
			Role:             RoleAssistant,
			Content:          a.content.String(),
			ReasoningContent: a.reasoning.String(),
			ToolCalls:        toolCallsFrom(a.tools),
		},
		FinishReason: a.finish,
		Usage:        a.usage,
	}
}

func marshalRequest(model string, req Request) ([]byte, error) {
	wire := wireRequest{
		Model:       model,
		Messages:    wireMessages(req.Messages),
		Temperature: req.Temperature,
		ToolChoice:  wireToolChoice(req.ToolChoice),
	}
	if req.MaxTokens > 0 {
		n := req.MaxTokens
		wire.MaxCompletionTokens = &n
	}
	if len(req.Tools) > 0 {
		wire.Tools = wireTools(req.Tools)
	}
	if req.wantsStream() {
		wire.Stream = true
		wire.StreamOptions = &wireStreamOptions{IncludeUsage: true}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&wire); err != nil {
		return nil, &Error{Code: CodeInvalidRequest, Message: "编码请求失败", Err: err}
	}
	return buf.Bytes(), nil
}

func wireMessages(msgs []Message) []wireMessage {
	out := make([]wireMessage, len(msgs))
	for i, m := range msgs {
		wm := wireMessage{
			Role:             string(m.Role),
			Name:             m.Name,
			ToolCallID:       m.ToolCallID,
			ReasoningContent: m.ReasoningContent,
		}
		if m.Role == RoleAssistant && m.Content == "" && len(m.ToolCalls) > 0 {
			wm.Content = nil
		} else {
			content := m.Content
			wm.Content = &content
		}
		for _, tc := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, wireReqToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: wireFunction{
					Name:      tc.Name,
					Arguments: tc.Arguments,
				},
			})
		}
		out[i] = wm
	}
	return out
}

func wireTools(tools []Tool) []wireTool {
	out := make([]wireTool, len(tools))
	for i, tool := range tools {
		params := bytes.TrimSpace(tool.Parameters)
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out[i] = wireTool{
			Type: "function",
			Function: wireToolDef{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  params,
			},
		}
	}
	return out
}

func wireToolChoice(choice string) any {
	switch choice {
	case "":
		return nil
	case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
		return choice
	default:
		return map[string]any{
			"type":     "function",
			"function": map[string]string{"name": choice},
		}
	}
}

type wireRequest struct {
	Model               string             `json:"model"`
	Messages            []wireMessage      `json:"messages"`
	Tools               []wireTool         `json:"tools,omitempty"`
	ToolChoice          any                `json:"tool_choice,omitempty"`
	Temperature         *float64           `json:"temperature,omitempty"`
	MaxCompletionTokens *int               `json:"max_completion_tokens,omitempty"`
	Stream              bool               `json:"stream,omitempty"`
	StreamOptions       *wireStreamOptions `json:"stream_options,omitempty"`
}

type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role             string            `json:"role"`
	Content          *string           `json:"content"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	Name             string            `json:"name,omitempty"`
	ToolCallID       string            `json:"tool_call_id,omitempty"`
	ToolCalls        []wireReqToolCall `json:"tool_calls,omitempty"`
}

type wireReqToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string      `json:"type"`
	Function wireToolDef `json:"function"`
}

type wireToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireCompletion struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Choices []wireChoice    `json:"choices"`
	Usage   *wireUsage      `json:"usage"`
	Error   json.RawMessage `json:"error"`
}

type wireChunk struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Choices []wireChoice    `json:"choices"`
	Usage   *wireUsage      `json:"usage"`
	Error   json.RawMessage `json:"error"`
}

type wireChoice struct {
	Index        int         `json:"index"`
	Message      *wireOutMsg `json:"message"`
	Delta        *wireOutMsg `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type wireOutMsg struct {
	Role             string             `json:"role"`
	Content          *string            `json:"content"`
	ReasoningContent *string            `json:"reasoning_content"`
	ToolCalls        []wireRespToolCall `json:"tool_calls"`
}

type wireRespToolCall struct {
	Index    int          `json:"index"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireUsage struct {
	PromptTokens            int `json:"prompt_tokens"`
	CompletionTokens        int `json:"completion_tokens"`
	TotalTokens             int `json:"total_tokens"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u wireUsage) usage() Usage {
	return Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		ReasoningTokens:  u.CompletionTokensDetails.ReasoningTokens,
	}
}

func (c wireCompletion) response() Response {
	ch := c.Choices[0]
	msg := Message{Role: RoleAssistant}
	if ch.Message != nil {
		if ch.Message.Content != nil {
			msg.Content = *ch.Message.Content
		}
		if ch.Message.ReasoningContent != nil {
			msg.ReasoningContent = *ch.Message.ReasoningContent
		}
		for _, tc := range ch.Message.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}
	if len(msg.ToolCalls) == 0 {
		msg.ToolCalls = nil
	}
	finish := ""
	if ch.FinishReason != nil {
		finish = *ch.FinishReason
	}
	resp := Response{
		ID:           c.ID,
		Model:        c.Model,
		Message:      msg,
		FinishReason: finish,
	}
	if c.Usage != nil {
		resp.Usage = c.Usage.usage()
	}
	return resp
}
