package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/loveRyujin/lantern/core/errcode"
	"github.com/loveRyujin/lantern/core/tools"
)

const (
	// DefaultSearchK 是调用方省略 k 时的 top-k。
	DefaultSearchK = 5
	// MaxSearchK 是单次检索最多返回的条数，和 tools.SpecSearchDocs 里 k 的 maximum 相同。
	MaxSearchK = 20
	// DefaultEvidenceThreshold 是无依据拒答的默认阈值。
	//
	// 分数是 L2 归一化向量的点积，也就是余弦，范围 [-1, 1]。
	// 本次检索的最高分低于该值，或者库里没有命中时，search_docs 返回「未找到依据」，
	// 不把低分片段的正文和 chunk_id 交给模型。等于阈值仍算有依据。
	// 0.45 是起始常量，不是在金标集上测出来的召回切点，可以在构造工具时覆盖。
	DefaultEvidenceThreshold float32 = 0.45

	// SearchStatusOK 表示最高分达到阈值，hits 可以引用。
	SearchStatusOK = "ok"
	// SearchStatusNoEvidence 表示空命中或最高分低于阈值。
	SearchStatusNoEvidence = "no_evidence"
	// NoEvidenceMessage 是无依据路径的固定文案。最终回答必须原样包含这六个字。
	NoEvidenceMessage = "未找到依据"

	// CodeSearchFailed 是检索过程中的非预期失败。它不是空命中。
	CodeSearchFailed = "search_failed"
)

// SearchTool 是 search_docs。
//
// 它只使用构造时绑入的 Embedder 和 VectorStore。store 应当打开当前工作区自己的 kb.db。
// Execute 不读取 call.WorkspaceID，也不按 workspace 过滤。隔离来自打开不同的库文件。
type SearchTool struct {
	embedder  Embedder
	store     VectorStore
	threshold float32
	defaultK  int
}

// SearchOption 调整阈值或默认 top-k。
type SearchOption func(*SearchTool)

// WithEvidenceThreshold 覆盖 DefaultEvidenceThreshold。NaN 会在构造时拒绝。
func WithEvidenceThreshold(v float32) SearchOption {
	return func(t *SearchTool) { t.threshold = v }
}

// WithDefaultK 覆盖省略 k 时的 top-k，必须在 1 和 MaxSearchK 之间。
func WithDefaultK(k int) SearchOption {
	return func(t *SearchTool) { t.defaultK = k }
}

// NewSearchTool 绑定一对 Embedder 和 VectorStore。二者都必须非 nil。
func NewSearchTool(embedder Embedder, store VectorStore, opts ...SearchOption) (*SearchTool, error) {
	if embedder == nil || store == nil {
		return nil, fmt.Errorf("%w: search_docs 需要 Embedder 和 VectorStore", ErrInvalidArgument)
	}
	tool := &SearchTool{
		embedder:  embedder,
		store:     store,
		threshold: DefaultEvidenceThreshold,
		defaultK:  DefaultSearchK,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(tool)
		}
	}
	if tool.defaultK < 1 || tool.defaultK > MaxSearchK {
		return nil, fmt.Errorf("%w: 默认 k 必须在 1 和 %d 之间", ErrInvalidArgument, MaxSearchK)
	}
	if math.IsNaN(float64(tool.threshold)) {
		return nil, fmt.Errorf("%w: 证据阈值不能是 NaN", ErrInvalidArgument)
	}
	return tool, nil
}

// Spec 实现 tools.Tool。名字、Schema 与 tools.SpecSearchDocs 相同。
func (t *SearchTool) Spec() tools.Spec {
	return tools.SpecSearchDocs
}

type searchArgs struct {
	Query string `json:"query"`
	K     *int   `json:"k"`
}

type searchHitJSON struct {
	ChunkID   string  `json:"chunk_id"`
	DocID     string  `json:"doc_id"`
	Text      string  `json:"text"`
	TitlePath string  `json:"title_path"`
	Strategy  string  `json:"strategy"`
	Score     float64 `json:"score"`
}

type searchPayload struct {
	Status    string          `json:"status"`
	Message   string          `json:"message,omitempty"`
	Query     string          `json:"query"`
	Threshold float64         `json:"threshold"`
	MaxScore  *float64        `json:"max_score"`
	K         int             `json:"k"`
	Hits      []searchHitJSON `json:"hits"`
	Stats     searchStatsJSON `json:"stats"`
}

type searchStatsJSON struct {
	DurationNS int64 `json:"duration_ns"`
	Chunks     int   `json:"chunks"`
}

// Execute 先嵌入 query，再在绑定的库上检索。
//
// 嵌入服务不可用、模型不一致或检索失败时返回带码错误，Content 为空。
// 空命中或最高分低于阈值时仍然成功返回，status 为 no_evidence，hits 为空。
func (t *SearchTool) Execute(ctx context.Context, call tools.Call) (tools.Result, error) {
	if t == nil || t.embedder == nil || t.store == nil {
		return tools.Result{}, errors.New("rag: search_docs 没有 Embedder 或 VectorStore")
	}
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	query, k, err := t.parseArgs(call.Args)
	if err != nil {
		return tools.Result{}, err
	}
	if err := rejectIndexedMismatch(ctx, t.store, t.embedder); err != nil {
		return tools.Result{}, searchFailure(err)
	}
	vectors, err := t.embedder.Embed(ctx, []string{query})
	if err != nil {
		return tools.Result{}, searchFailure(err)
	}
	if len(vectors) != 1 || len(vectors[0]) != t.embedder.Dim() {
		got := 0
		if len(vectors) == 1 {
			got = len(vectors[0])
		}
		return tools.Result{}, searchFailure(&DimensionError{Got: got, Want: t.embedder.Dim()})
	}
	hits, err := t.store.Search(ctx, vectors[0], k)
	if err != nil {
		return tools.Result{}, searchFailure(err)
	}
	stats := t.store.LastSearchStats()
	payload := searchPayload{
		Query:     query,
		Threshold: float64(t.threshold),
		K:         k,
		Hits:      []searchHitJSON{},
		Stats: searchStatsJSON{
			DurationNS: stats.Duration.Nanoseconds(),
			Chunks:     stats.Chunks,
		},
	}
	max, hasMax := maxScore(hits)
	if hasMax {
		score := float64(max)
		payload.MaxScore = &score
	}
	if !hasMax || max < t.threshold {
		payload.Status = SearchStatusNoEvidence
		payload.Message = NoEvidenceMessage
		body, err := json.Marshal(payload)
		if err != nil {
			return tools.Result{}, searchFailure(err)
		}
		return tools.Result{
			Content: string(body),
			Summary: fmt.Sprintf("未找到依据（比较了 %d 个 chunk）", stats.Chunks),
		}, nil
	}
	payload.Status = SearchStatusOK
	payload.Hits = make([]searchHitJSON, len(hits))
	for i, hit := range hits {
		payload.Hits[i] = searchHitJSON{
			ChunkID:   hit.ChunkID,
			DocID:     hit.DocID,
			Text:      hit.Text,
			TitlePath: hit.TitlePath,
			Strategy:  hit.Strategy,
			Score:     float64(hit.Score),
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return tools.Result{}, searchFailure(err)
	}
	return tools.Result{
		Content: string(body),
		Summary: fmt.Sprintf("检索 %d 个 chunk，返回 %d 条", stats.Chunks, len(hits)),
	}, nil
}

func (t *SearchTool) parseArgs(raw []byte) (string, int, error) {
	if len(raw) == 0 {
		return "", 0, invalidSearchArgs("参数必须是 JSON 对象")
	}
	var args searchArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", 0, invalidSearchArgs("参数不是合法 JSON")
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return "", 0, invalidSearchArgs("query 不能为空")
	}
	if len(query) > 8192 {
		return "", 0, invalidSearchArgs("query 超过长度上限")
	}
	k := t.defaultK
	if args.K != nil {
		k = *args.K
	}
	if k < 1 || k > MaxSearchK {
		return "", 0, invalidSearchArgs(fmt.Sprintf("k 必须在 1 和 %d 之间", MaxSearchK))
	}
	return query, k, nil
}

func maxScore(hits []Hit) (float32, bool) {
	if len(hits) == 0 {
		return 0, false
	}
	max := hits[0].Score
	for _, hit := range hits[1:] {
		if hit.Score > max {
			max = hit.Score
		}
	}
	return max, true
}

func invalidSearchArgs(message string) error {
	return &tools.Error{Code: tools.CodeArgumentsInvalid, Retryable: true, Message: message}
}

func searchFailure(err error) error {
	switch {
	case errors.Is(err, ErrEmbedderUnavailable):
		return &tools.Error{
			Code:    string(errcode.EmbeddingUnavailable),
			Message: unavailableGuidance(err),
			Err:     err,
		}
	case errors.Is(err, ErrEmbedderMismatch):
		return &tools.Error{
			Code:    string(errcode.EmbeddingMismatch),
			Message: "索引与当前嵌入模型不一致，无法检索",
			Err:     err,
		}
	case errors.Is(err, ErrDimension):
		return &tools.Error{
			Code:    CodeSearchFailed,
			Message: "查询向量维度与索引不一致，无法检索",
			Err:     err,
		}
	default:
		return &tools.Error{Code: CodeSearchFailed, Message: "检索失败", Err: err}
	}
}

func unavailableGuidance(err error) string {
	var unavailable *EmbedderUnavailableError
	if errors.As(err, &unavailable) && unavailable.Guidance != "" {
		return unavailable.Guidance
	}
	return "嵌入服务不可用，无法检索。请确认本机 Ollama 已启动，并已拉取 bge-m3。"
}

// EvidenceFromSearch 从一次成功的 search_docs 观察值里取出可引用的 chunk_id。
//
// ok 为 false 表示这不是本工具的 JSON。status 为 no_evidence，或 ok 但没有 chunk_id 时，
// noEvidence 为 true，ids 为 nil：低分片段即使出现在 JSON 里也不能引用。
func EvidenceFromSearch(content string) (ids []string, noEvidence bool, ok bool) {
	var payload struct {
		Status string `json:"status"`
		Hits   []struct {
			ChunkID string `json:"chunk_id"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return nil, false, false
	}
	switch payload.Status {
	case SearchStatusOK:
		for _, hit := range payload.Hits {
			if hit.ChunkID != "" {
				ids = append(ids, hit.ChunkID)
			}
		}
		return ids, len(ids) == 0, true
	case SearchStatusNoEvidence:
		return nil, true, true
	default:
		return nil, false, false
	}
}
