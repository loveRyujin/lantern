package llm

import (
	"errors"
	"fmt"
	"strings"

	"github.com/loveRyujin/lantern/core/errcode"
)

// 错误码是 Agent 重试和评测断言的稳定契约。
const (
	CodeTimeout           = "timeout"
	CodeLLM5xx            = "llm_5xx"
	CodeInvalidJSON       = "invalid_json"
	CodeOllamaUnreachable = "ollama_unreachable"
	CodeMissingAPIKey     = "missing_api_key"
	CodeSecretNotFound    = "secret_not_found"
	CodeSecret            = "secret_error"
	CodeScriptExhausted   = "script_exhausted"
	CodeScriptMismatch    = "script_mismatch"
	CodeInvalidScript     = "invalid_script"
	CodeInvalidRequest    = "invalid_request"
	CodeUnauthorized      = "unauthorized"
	CodeRateLimited       = "rate_limited"
	CodeHTTP              = "http_error"
	CodeTransport         = "transport"
	CodeCanceled          = "canceled"
	CodeEmptyResponse     = "empty_response"
	CodeInvalidResponse   = "invalid_response"
	// CodeStreamTruncated 与 errcode.StreamTruncated 相同，避免两处码值漂移。
	CodeStreamTruncated = string(errcode.StreamTruncated)
)

// 哨兵错误只带错误码。[Error.Is] 按错误码比较，调用方用 errors.Is 判断即可。
var (
	ErrTimeout           = &Error{Code: CodeTimeout, Retryable: true}
	ErrLLM5xx            = &Error{Code: CodeLLM5xx, Retryable: true}
	ErrInvalidJSON       = &Error{Code: CodeInvalidJSON}
	ErrOllamaUnreachable = &Error{Code: CodeOllamaUnreachable}
	ErrMissingAPIKey     = &Error{Code: CodeMissingAPIKey}
	ErrSecretNotFound    = &Error{Code: CodeSecretNotFound}
	ErrScriptExhausted   = &Error{Code: CodeScriptExhausted}
	ErrScriptMismatch    = &Error{Code: CodeScriptMismatch}
	ErrInvalidScript     = &Error{Code: CodeInvalidScript}
	ErrInvalidRequest    = &Error{Code: CodeInvalidRequest}
	ErrCanceled          = &Error{Code: CodeCanceled}
	ErrEmptyResponse     = &Error{Code: CodeEmptyResponse}
	ErrStreamTruncated   = &Error{Code: CodeStreamTruncated, Retryable: true}
)

// Error 是 Provider 返回的结构化错误。Message 可以记日志，不应包含 API Key。
type Error struct {
	Code      string
	Status    int
	Retryable bool
	Message   string
	Err       error
}

func (e *Error) Error() string {
	if e == nil {
		return "llm: <nil>"
	}
	var b strings.Builder
	b.WriteString("llm: ")
	if e.Code != "" {
		b.WriteString(e.Code)
	} else {
		b.WriteString("error")
	}
	if e.Status != 0 {
		fmt.Fprintf(&b, ": HTTP %d", e.Status)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

// Unwrap 让 errors.Is 继续匹配 context.DeadlineExceeded 等底层错误。
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is 在错误码相同时匹配。哨兵值只设置 Code。
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok || t == nil || e == nil || t.Code == "" {
		return false
	}
	return e.Code == t.Code
}

// Retryable 报告 err 是否值得按策略重试。不是 [*Error] 时返回 false。
func Retryable(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Retryable
	}
	return false
}

func invalidRequest(format string, args ...any) *Error {
	return &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf(format, args...)}
}

func invalidScript(format string, args ...any) *Error {
	return &Error{Code: CodeInvalidScript, Message: fmt.Sprintf(format, args...)}
}

func clip(s string) string {
	const max = 512
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}

// redact 抹掉密钥。短于 8 个字符的密钥不替换，避免把普通单词清掉。
func redact(msg, secret string) string {
	if len(secret) < 8 || msg == "" || !strings.Contains(msg, secret) {
		return msg
	}
	return strings.ReplaceAll(msg, secret, "[REDACTED]")
}

func scrubError(err error, secret string) error {
	if err == nil || len(secret) < 8 {
		return err
	}
	code, coded := errcode.CodeOf(err)
	var e *Error
	if !errors.As(err, &e) || e == nil {
		return scrubBare(err, secret)
	}
	cp := *e
	cp.Message = clip(redact(e.Message, secret))
	cp.Err = scrubBare(e.Err, secret)
	if coded {
		return &codedError{inner: &cp, stable: code}
	}
	return &cp
}

// codedError 把 errcode.Coded 挂在 *Error 上。
// *Error 的字段已经叫 Code，不能再声明同名方法。
type codedError struct {
	inner  *Error
	stable errcode.Code
}

func (e *codedError) Error() string {
	if e == nil || e.inner == nil {
		return "llm: <nil>"
	}
	return e.inner.Error()
}

func (e *codedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.inner
}

func (e *codedError) Is(target error) bool {
	if e == nil || e.inner == nil {
		return false
	}
	return e.inner.Is(target)
}

func (e *codedError) Code() errcode.Code {
	if e == nil {
		return ""
	}
	return e.stable
}

func streamTruncated(message string) error {
	if message == "" {
		message = "SSE 流在收到 [DONE] 或 finish_reason 之前断开"
	}
	return &codedError{
		inner:  &Error{Code: CodeStreamTruncated, Retryable: true, Message: message},
		stable: errcode.StreamTruncated,
	}
}

func scrubBare(err error, secret string) error {
	if err == nil || len(secret) < 8 || !strings.Contains(err.Error(), secret) {
		return err
	}
	return errors.New(clip(redact(err.Error(), secret)))
}
