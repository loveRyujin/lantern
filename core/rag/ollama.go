package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/loveRyujin/lantern/core/errcode"
)

const (
	// DefaultOllamaID 是默认 Embedder 身份。
	DefaultOllamaID = "ollama:bge-m3"
	// DefaultOllamaModel 是发给 Ollama /api/embed 的模型名。
	DefaultOllamaModel = "bge-m3"
	// DefaultOllamaDim 是 bge-m3 当前的向量维度。
	DefaultOllamaDim = 1024
	// DefaultOllamaBaseURL 是本机 Ollama 的默认根地址，不含路径。
	DefaultOllamaBaseURL = "http://127.0.0.1:11434"

	// EmbedderUnreachable 表示 TCP 连不上 Ollama。
	EmbedderUnreachable = "unreachable"
	// EmbedderModelMissing 表示服务在，但请求的模型还没拉取。
	EmbedderModelMissing = "model_missing"
	// EmbedderUpstream 表示 Ollama 返回了不能使用的 HTTP 状态。
	EmbedderUpstream = "upstream"
	// EmbedderBadResponse 表示 HTTP 成功但正文不是可用向量。
	EmbedderBadResponse = "bad_response"

	ollamaUserAgent = "lantern/0.3 core/rag"
	maxEmbedBody    = 32 << 20
)

// ErrEmbedderUnavailable 表示这次没有得到可用向量。
//
// errors.Is(err, ErrEmbedderUnavailable) 对任何 *EmbedderUnavailableError 都为真。
// Code 返回 embedding_unavailable。调用方应展示 Guidance，不得把空切片当成嵌入结果。
var ErrEmbedderUnavailable = &EmbedderUnavailableError{}

// EmbedderUnavailableError 是一次嵌入服务失败。Reason 区分连不上、缺模型和坏响应。
type EmbedderUnavailableError struct {
	// Reason 是 EmbedderUnreachable、EmbedderModelMissing、EmbedderUpstream 或 EmbedderBadResponse。
	Reason string
	// Model 是请求里的 Ollama 模型名，例如 bge-m3。
	Model string
	// Endpoint 是实际请求的 /api/embed 地址。
	Endpoint string
	// Detail 是截断后的传输或响应说明，只给日志看。
	Detail string
	// Guidance 是可以展示给用户的安装或 pull 引导。
	Guidance string
}

// Error 实现 error。零值只返回码值，便于日志识别哨兵。
func (e *EmbedderUnavailableError) Error() string {
	if e == nil || (e.Reason == "" && e.Guidance == "" && e.Detail == "") {
		return string(errcode.EmbeddingUnavailable)
	}
	var b strings.Builder
	b.WriteString(string(errcode.EmbeddingUnavailable))
	if e.Reason != "" {
		b.WriteString(": ")
		b.WriteString(e.Reason)
	}
	if e.Guidance != "" {
		b.WriteString(": ")
		b.WriteString(e.Guidance)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	return b.String()
}

// Code 返回 embedding_unavailable。
func (e *EmbedderUnavailableError) Code() errcode.Code {
	if e == nil {
		return ""
	}
	return errcode.EmbeddingUnavailable
}

// Is 让所有嵌入不可用错误都匹配哨兵 ErrEmbedderUnavailable。
func (e *EmbedderUnavailableError) Is(target error) bool {
	return e != nil && target == ErrEmbedderUnavailable
}

// OllamaEmbedder 调用本机 Ollama 的 /api/embed。
// 默认身份是 ollama:bge-m3，维度 1024。返回前会做 L2 归一化。
type OllamaEmbedder struct {
	baseURL string
	model   string
	dim     int
	client  *http.Client
}

var _ Embedder = (*OllamaEmbedder)(nil)

// OllamaOption 修改 OllamaEmbedder 的默认配置。
type OllamaOption func(*OllamaEmbedder)

// NewOllamaEmbedder 使用本机 bge-m3。选项不合法时返回错误，不返回半配置的客户端。
func NewOllamaEmbedder(opts ...OllamaOption) (*OllamaEmbedder, error) {
	e := &OllamaEmbedder{
		baseURL: DefaultOllamaBaseURL,
		model:   DefaultOllamaModel,
		dim:     DefaultOllamaDim,
		client:  defaultOllamaClient(),
	}
	for _, opt := range opts {
		opt(e)
	}
	if e.client == nil {
		e.client = defaultOllamaClient()
	}
	if err := e.validate(); err != nil {
		return nil, err
	}
	return e, nil
}

// WithOllamaBaseURL 设置 Ollama 根地址，例如 http://127.0.0.1:11434。不要带 /api/embed。
func WithOllamaBaseURL(baseURL string) OllamaOption {
	return func(e *OllamaEmbedder) { e.baseURL = baseURL }
}

// WithOllamaModel 设置 Ollama 模型名。身份是 "ollama:" 加这个名字。
// 传入已经带 ollama: 前缀的字符串时会去掉一层前缀。
func WithOllamaModel(model string) OllamaOption {
	return func(e *OllamaEmbedder) { e.model = model }
}

// WithOllamaDim 设置期望维度。响应长度不符时 Embed 返回 ErrDimension，不截断也不补零。
func WithOllamaDim(dim int) OllamaOption {
	return func(e *OllamaEmbedder) { e.dim = dim }
}

// WithOllamaHTTPClient 替换 HTTP 客户端。传入 nil 时恢复默认客户端。
func WithOllamaHTTPClient(client *http.Client) OllamaOption {
	return func(e *OllamaEmbedder) { e.client = client }
}

// ID 返回 ollama:<model>。默认是 ollama:bge-m3。
func (e *OllamaEmbedder) ID() string {
	if e == nil {
		return ""
	}
	return "ollama:" + e.model
}

// Dim 返回构造时的维度。默认是 1024。
func (e *OllamaEmbedder) Dim() int {
	if e == nil {
		return 0
	}
	return e.dim
}

// Embed 按 texts 的顺序返回 L2 归一化向量。texts 为空时返回 nil, nil，不访问网络。
// Ollama 不可达或模型未拉取时返回 ErrEmbedderUnavailable，不返回空向量，也不 panic。
func (e *OllamaEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if e == nil {
		return nil, errors.New("rag: Ollama Embedder 为 nil")
	}
	if ctx == nil {
		return nil, errors.New("rag: ctx 不能为 nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(texts) == 0 {
		return nil, nil
	}
	endpoint, err := e.endpoint()
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}{Model: e.model, Input: texts})
	if err != nil {
		return nil, e.unavailable(EmbedderBadResponse, endpoint, "构造请求失败", err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, e.unavailable(EmbedderBadResponse, endpoint, "构造请求失败", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ollamaUserAgent)

	resp, err := e.client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, e.unavailable(EmbedderUnreachable, endpoint, installGuidance(endpoint), err.Error())
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxEmbedBody+1))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, e.unavailable(EmbedderBadResponse, endpoint, "读取 Ollama 响应失败，没有得到向量。", err.Error())
	}
	if len(body) > maxEmbedBody {
		return nil, e.unavailable(EmbedderBadResponse, endpoint, "Ollama 响应过大，没有得到向量。", "")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, e.httpFailure(endpoint, resp.StatusCode, body)
	}
	var decoded struct {
		Embeddings [][]float64 `json:"embeddings"`
		Error      string      `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, e.unavailable(EmbedderBadResponse, endpoint, "Ollama 响应不是 JSON，没有得到向量。", clip(err.Error()))
	}
	if decoded.Error != "" && len(decoded.Embeddings) == 0 {
		return nil, e.httpFailure(endpoint, resp.StatusCode, body)
	}
	if len(decoded.Embeddings) != len(texts) {
		return nil, e.unavailable(
			EmbedderBadResponse,
			endpoint,
			"Ollama 返回的向量条数和输入不一致，没有得到向量。",
			fmt.Sprintf("got %d want %d", len(decoded.Embeddings), len(texts)),
		)
	}
	out := make([][]float32, len(decoded.Embeddings))
	for i, raw := range decoded.Embeddings {
		if len(raw) != e.dim {
			return nil, &DimensionError{Got: len(raw), Want: e.dim}
		}
		vec := make([]float32, len(raw))
		for j, x := range raw {
			vec[j] = float32(x)
		}
		norm, err := L2Normalize(vec)
		if err != nil {
			return nil, err
		}
		out[i] = norm
	}
	return out, nil
}

func (e *OllamaEmbedder) validate() error {
	e.model = strings.TrimSpace(e.model)
	e.model = strings.TrimPrefix(e.model, "ollama:")
	e.model = strings.TrimSpace(e.model)
	if e.model == "" || strings.ContainsAny(e.model, " \r\n\t") {
		return fmt.Errorf("%w: Ollama 模型名不合法", ErrInvalidArgument)
	}
	if e.dim <= 0 {
		return fmt.Errorf("%w: Ollama 维度必须为正", ErrInvalidArgument)
	}
	if _, err := e.endpoint(); err != nil {
		return err
	}
	return nil
}

func (e *OllamaEmbedder) endpoint() (string, error) {
	raw := strings.TrimSpace(e.baseURL)
	if raw == "" {
		raw = DefaultOllamaBaseURL
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%w: Ollama 地址不合法", ErrInvalidArgument)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("%w: Ollama 地址不要带路径", ErrInvalidArgument)
	}
	u.Path = "/api/embed"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func (e *OllamaEmbedder) httpFailure(endpoint string, status int, body []byte) error {
	detail := clip(string(body))
	if status == http.StatusNotFound || missingModel(detail) {
		return e.unavailable(EmbedderModelMissing, endpoint, pullGuidance(e.model), detail)
	}
	return e.unavailable(EmbedderUpstream, endpoint, "Ollama 返回异常。请查看本机 Ollama 服务是否正常。", fmt.Sprintf("HTTP %d %s", status, detail))
}

func (e *OllamaEmbedder) unavailable(reason, endpoint, guidance, detail string) error {
	return &EmbedderUnavailableError{
		Reason:   reason,
		Model:    e.model,
		Endpoint: endpoint,
		Detail:   clip(detail),
		Guidance: guidance,
	}
}

func installGuidance(endpoint string) string {
	return fmt.Sprintf("无法连接本机 Ollama（%s）。请安装并启动 Ollama，默认地址是 %s。", endpoint, DefaultOllamaBaseURL)
}

func pullGuidance(model string) string {
	return fmt.Sprintf("Ollama 尚未提供模型 %s。请在本机执行：ollama pull %s", model, model)
}

func missingModel(body string) bool {
	text := strings.ToLower(body)
	return strings.Contains(text, "not found") || strings.Contains(text, "try pulling")
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	const max = 240
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "…"
}

func defaultOllamaClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
