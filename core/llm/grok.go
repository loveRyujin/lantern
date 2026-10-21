package llm

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const (
	userAgent     = "lantern/0.3 core/llm"
	maxJSONBody   = 8 << 20
	maxStreamBody = 32 << 20
)

var errBodyTooLarge = errors.New("response body too large")

// Grok 调用 grok-4.6 的 /v1/chat/completions。
// 密钥顺序：WithAPIKey、环境变量 XAI_API_KEY、环境变量 GROK_API_KEY、SecretStore。
type Grok struct {
	baseURL    string
	model      string
	apiKey     string
	secrets    SecretStore
	secretName string
	client     *http.Client
	lookupEnv  func(string) (string, bool)
}

// GrokOption 修改 Grok 的默认配置。
type GrokOption func(*Grok)

// NewGrok 使用默认的 grok-4.6 地址和模型。
func NewGrok(opts ...GrokOption) *Grok {
	g := &Grok{
		baseURL:    DefaultBaseURL,
		model:      DefaultModel,
		secretName: DefaultSecretName,
		client:     defaultClient(),
	}
	for _, opt := range opts {
		opt(g)
	}
	if g.client == nil {
		g.client = defaultClient()
	}
	return g
}

// WithAPIKey 设置显式密钥，优先于环境变量和 SecretStore。
func WithAPIKey(key string) GrokOption {
	return func(g *Grok) { g.apiKey = key }
}

// WithSecrets 设置密钥存储。只在环境变量为空时读取。
func WithSecrets(store SecretStore) GrokOption {
	return func(g *Grok) { g.secrets = store }
}

// WithSecretName 设置 SecretStore 里的密钥名，默认 grok_api_key。
func WithSecretName(name string) GrokOption {
	return func(g *Grok) { g.secretName = name }
}

// WithBaseURL 设置 API 根地址，例如 https://api.x.ai/v1。测试可传入 httptest 的地址。
func WithBaseURL(baseURL string) GrokOption {
	return func(g *Grok) { g.baseURL = baseURL }
}

// WithModel 设置模型 ID，默认 grok-4.6。
func WithModel(model string) GrokOption {
	return func(g *Grok) { g.model = model }
}

// WithHTTPClient 替换 HTTP 客户端。传入 nil 时恢复默认客户端。
func WithHTTPClient(client *http.Client) GrokOption {
	return func(g *Grok) { g.client = client }
}

// WithEnvLookup 替换环境变量读取。nil 表示 os.LookupEnv。
func WithEnvLookup(fn func(string) (string, bool)) GrokOption {
	return func(g *Grok) { g.lookupEnv = fn }
}

func (g *Grok) ID() string {
	return "grok:" + g.modelName()
}

// Chat 发送一轮 Chat Completions。wantsStream 为真时使用 SSE，并把增量交给 OnDelta。
// 默认客户端不设超时，调用方必须用 ctx 设置截止时间。推理模型可能要数分钟，这里不会偷偷加一个短超时。
func (g *Grok) Chat(ctx context.Context, req Request) (resp Response, err error) {
	if err := ctx.Err(); err != nil {
		return Response{}, classifyContext(err)
	}
	req.ToolChoice = strings.TrimSpace(req.ToolChoice)
	if err := validateRequest(req); err != nil {
		return Response{}, err
	}
	key, err := g.resolveAPIKey(ctx)
	if err != nil {
		return Response{}, scrubError(err, key)
	}
	defer func() {
		if err != nil {
			err = scrubError(err, key)
		}
	}()
	endpoint, err := chatCompletionsURL(g.baseURL)
	if err != nil {
		return Response{}, err
	}
	payload, err := marshalRequest(g.modelName(), req)
	if err != nil {
		return Response{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return Response{}, &Error{Code: CodeInvalidRequest, Message: "构造请求失败", Err: err}
	}
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set("Content-Type", "application/json")
	if req.wantsStream() {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	httpReq.Header.Set("User-Agent", userAgent)

	httpResp, err := g.httpClient().Do(httpReq)
	if err != nil {
		return Response{}, classifyTransport(err)
	}
	defer httpResp.Body.Close()
	return g.decode(httpResp, req.wantsStream(), req.OnDelta)
}

func (g *Grok) modelName() string {
	if g == nil || strings.TrimSpace(g.model) == "" {
		return DefaultModel
	}
	return strings.TrimSpace(g.model)
}

func (g *Grok) httpClient() *http.Client {
	if g != nil && g.client != nil {
		return g.client
	}
	return defaultClient()
}

func (g *Grok) resolveAPIKey(ctx context.Context) (string, error) {
	if key, err := normalizeKey(g.apiKey); err != nil || key != "" {
		return key, err
	}
	if key, err := normalizeKey(g.getenv(EnvAPIKey)); err != nil || key != "" {
		return key, err
	}
	if key, err := normalizeKey(g.getenv(EnvAPIKeyAlias)); err != nil || key != "" {
		return key, err
	}
	if g.secrets != nil {
		name := strings.TrimSpace(g.secretName)
		if name == "" {
			name = DefaultSecretName
		}
		got, err := g.secrets.Get(ctx, name)
		if err != nil {
			if !errors.Is(err, ErrSecretNotFound) {
				return "", &Error{Code: CodeSecret, Message: "读取密钥失败", Err: err}
			}
		} else if key, keyErr := normalizeKey(got); keyErr != nil || key != "" {
			return key, keyErr
		}
	}
	return "", &Error{
		Code:    CodeMissingAPIKey,
		Message: "缺少 grok API Key（环境变量 XAI_API_KEY 或 GROK_API_KEY，或 Secrets）",
	}
}

func (g *Grok) getenv(key string) string {
	lookup := envLookup
	if g != nil && g.lookupEnv != nil {
		lookup = g.lookupEnv
	}
	v, ok := lookup(key)
	if !ok {
		return ""
	}
	return v
}

func envLookup(key string) (string, bool) {
	return os.LookupEnv(key)
}

func normalizeKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if len(key) >= 7 && strings.EqualFold(key[:7], "bearer ") {
		key = strings.TrimSpace(key[7:])
	}
	if key == "" {
		return "", nil
	}
	if strings.ContainsAny(key, "\r\n\x00") {
		return "", invalidRequest("API Key 含有非法字符")
	}
	return key, nil
}

// defaultClient 不设置 Timeout。推理模型可能要数分钟，超时只由调用方的 ctx 决定。
func defaultClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func chatCompletionsURL(base string) (string, error) {
	raw := strings.TrimSpace(base)
	if raw == "" {
		raw = DefaultBaseURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", invalidRequest("grok base URL 无效")
	}
	if u.User != nil {
		return "", invalidRequest("grok base URL 不能包含用户信息")
	}
	if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
		return "", invalidRequest("grok base URL 使用 http 时必须是本机回环地址")
	}
	p := strings.TrimRight(u.Path, "/")
	if p == "" {
		u.Path = "/chat/completions"
	} else {
		u.Path = p + "/chat/completions"
	}
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func classifyTransport(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: CodeTimeout, Retryable: true, Message: "请求超时", Err: err}
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Code: CodeCanceled, Message: "请求已取消", Err: err}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &Error{Code: CodeTimeout, Retryable: true, Message: "请求超时", Err: err}
	}
	return &Error{Code: CodeTransport, Retryable: true, Message: "连接 grok 失败", Err: err}
}

func httpStatusError(status int, message string) *Error {
	e := &Error{Status: status, Message: strings.TrimSpace(message)}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Code = CodeUnauthorized
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		e.Code = CodeTimeout
		e.Retryable = true
	case status == http.StatusTooManyRequests:
		e.Code = CodeRateLimited
		e.Retryable = true
	case status >= 500 && status <= 599:
		e.Code = CodeLLM5xx
		e.Retryable = true
	default:
		e.Code = CodeHTTP
	}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	return e
}
