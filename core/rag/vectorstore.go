package rag

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// StrategyFixed 是固定长度切分，设计上的窗口是 512 token、重叠 64。
	// token 数先按字符数近似，切分器本身不在本次接口里。
	StrategyFixed = "fixed"
	// StrategyStructural 是按标题和口径条目切分。
	StrategyStructural = "structural"
)

// Chunk 是一条可写入向量库的片段。
//
// Vector 的长度必须等于索引维度。Normalize 为 true 时，库会保存 L2 归一化副本，不修改调用方的切片。
// EmbedderID 为空表示沿用打开库时的当前模型；非空时必须和索引里的 embedder_id 相同。
type Chunk struct {
	// ID 是片段标识，在一个库里唯一。
	ID string
	// DocID 是所属文档。Delete 按它删除。
	DocID string
	// Text 是片段正文。
	Text string
	// EmbedderID 为空表示沿用当前模型；非空时必须和索引一致。
	EmbedderID string
	// Vector 是向量，长度必须等于索引维度。
	Vector []float32
	// TitlePath 是标题路径，没有时为空。
	TitlePath string
	// Strategy 是切分策略，见 StrategyFixed 和 StrategyStructural。
	Strategy string
}

// Hit 是一条检索结果。Score 是点积；向量已归一化时就是余弦。
type Hit struct {
	// ChunkID 是片段标识。
	ChunkID string
	// DocID 是文档标识。
	DocID string
	// Text 是片段正文。
	Text string
	// TitlePath 是标题路径，没有时为空。
	TitlePath string
	// Strategy 是切分策略，见 StrategyFixed 和 StrategyStructural。
	Strategy string
	// Score 是点积。向量已归一化时就是余弦。
	Score float32
}

// IndexMeta 对应 kb.db 的 index_meta。一个工作区只有一行。
type IndexMeta struct {
	// EmbedderID 是建索引时的模型标识。
	EmbedderID string
	// Dim 是向量维度。
	Dim int
	// Normalize 为 true 时，入库和查询都做 L2 归一化，点积即余弦。
	Normalize bool
	// CreatedAt 是这行元数据写下的时间。
	CreatedAt time.Time
}

// Identity 返回这条元数据对应的模型身份。
func (m IndexMeta) Identity() EmbedderIdentity {
	return EmbedderIdentity{ID: m.EmbedderID, Dim: m.Dim}
}

// SearchStats 是一次成功检索的记账，对应技术设计里要记录的耗时和 chunk 数。
type SearchStats struct {
	// Duration 是这次检索的耗时。
	Duration time.Duration
	// Chunks 是参与比较的 chunk 数，不是返回条数。
	Chunks int
}

// VectorStore 是绑定单个工作区的向量库。
//
// 工作区不作为方法参数：调用方打开的就是那个工作区自己的库，检索时不再按 workspace 过滤。
// Upsert 整批成功或整批失败，不会留下半批向量。
// 当前模型与 index_meta 的 ID 或维度不同时，Upsert 和 Search 返回 ErrEmbedderMismatch，
// Search 的命中切片为 nil。Delete 仍然允许，避免为了清理旧文档而混入新模型的向量。
type VectorStore interface {
	Upsert(ctx context.Context, chunks []Chunk) error
	Delete(ctx context.Context, docID string) error
	Search(ctx context.Context, vec []float32, k int) ([]Hit, error)
	// LastSearchStats 返回最近一次成功 Search 的统计。
	// 失败的 Search 不更新它。没有成功过时为零值。
	LastSearchStats() SearchStats
}

type stored struct {
	id     string
	docID  string
	vector []float32
	hit    Hit
}

// FakeStore 是进程内的 VectorStore。它不写 kb.db，检索算法和以后的默认实现相同：归一化后点积，小顶堆取 top-k。
type FakeStore struct {
	mu          sync.Mutex
	workspaceID string
	current     EmbedderIdentity
	meta        *IndexMeta
	chunks      map[string]stored
	order       []string
	stats       SearchStats
}

var _ VectorStore = (*FakeStore)(nil)

// NewFakeStore 打开一个内存向量库。
//
// workspaceID 必须非空，用来把实例绑到一个工作区。
// current 是当前配置的 Embedder。indexed 为 nil 表示还没有 index_meta，第一次成功 Upsert 时写入，并默认做 L2 归一化。
// indexed 与 current 的 ID 或维度不同时仍然打开；之后 Upsert 和 Search 返回 ErrEmbedderMismatch。
// chunks 只在 indexed 非 nil 时允许出现，表示这份索引里已经有的片段。
func NewFakeStore(workspaceID string, current EmbedderIdentity, indexed *IndexMeta, chunks []Chunk) (*FakeStore, error) {
	if workspaceID == "" {
		return nil, fmt.Errorf("%w: 缺少 workspace", ErrInvalidArgument)
	}
	if err := current.Validate(); err != nil {
		return nil, err
	}
	store := &FakeStore{
		workspaceID: workspaceID,
		current:     current,
		chunks:      map[string]stored{},
	}
	if indexed == nil {
		if len(chunks) > 0 {
			return nil, fmt.Errorf("%w: 空索引不能带有 chunk", ErrInvalidArgument)
		}
		return store, nil
	}
	meta := *indexed
	if meta.EmbedderID == "" || meta.Dim <= 0 {
		return nil, fmt.Errorf("%w: index_meta 不完整", ErrInvalidArgument)
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now().UTC()
	}
	prepared, err := prepareChunks(meta, chunks, nil)
	if err != nil {
		return nil, err
	}
	store.meta = &meta
	store.apply(prepared)
	return store, nil
}

// WorkspaceID 返回构造时绑定的工作区。
func (s *FakeStore) WorkspaceID() string { return s.workspaceID }

// Index 返回 index_meta。库还是空的、且没有预置元数据时，ok 为 false。
func (s *FakeStore) Index() (IndexMeta, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.meta == nil {
		return IndexMeta{}, false
	}
	return *s.meta, true
}

// Count 返回当前 chunk 数。
func (s *FakeStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.chunks)
}

// Upsert 写入或按 chunk ID 替换。空批次是 no-op，不会创建 index_meta。
func (s *FakeStore) Upsert(ctx context.Context, chunks []Chunk) error {
	if ctx == nil {
		return errors.New("rag: ctx 不能为 nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(chunks) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.mismatchLocked(); err != nil {
		return err
	}
	meta := s.meta
	fresh := false
	if meta == nil {
		meta = &IndexMeta{
			EmbedderID: s.current.ID,
			Dim:        s.current.Dim,
			Normalize:  true,
			CreatedAt:  time.Now().UTC(),
		}
		fresh = true
	}
	prepared, err := prepareChunks(*meta, chunks, s.chunks)
	if err != nil {
		return err
	}
	if fresh {
		s.meta = meta
	}
	s.apply(prepared)
	return nil
}

// Delete 删除属于 docID 的全部分片。文档不存在时返回 nil。
// 模型不一致时仍允许删除，这样清理旧文档不会写入新模型的向量。
func (s *FakeStore) Delete(ctx context.Context, docID string) error {
	if ctx == nil {
		return errors.New("rag: ctx 不能为 nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if docID == "" {
		return fmt.Errorf("%w: 缺少 doc_id", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.order[:0]
	for _, id := range s.order {
		if s.chunks[id].docID == docID {
			delete(s.chunks, id)
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
	return nil
}

// Search 用点积取 top-k。k 必须大于 0。
// 模型不一致时返回 ErrEmbedderMismatch 且命中为 nil，不更新统计。
// 查询向量长度不等于索引维度时返回 ErrDimension，同样不返回命中。
func (s *FakeStore) Search(ctx context.Context, vec []float32, k int) ([]Hit, error) {
	if ctx == nil {
		return nil, errors.New("rag: ctx 不能为 nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if k <= 0 {
		return nil, fmt.Errorf("%w: k 必须大于 0", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.mismatchLocked(); err != nil {
		return nil, err
	}
	if s.meta == nil || len(s.order) == 0 {
		s.stats = SearchStats{Chunks: len(s.order)}
		return nil, nil
	}
	if len(vec) != s.meta.Dim {
		return nil, &DimensionError{Got: len(vec), Want: s.meta.Dim}
	}
	query := append([]float32(nil), vec...)
	var err error
	if s.meta.Normalize {
		query, err = L2Normalize(query)
		if err != nil {
			return nil, err
		}
	} else if err = finite(query); err != nil {
		return nil, err
	}
	start := time.Now()
	items := make([]scoredHit, 0, len(s.order))
	for _, id := range s.order {
		item := s.chunks[id]
		score, derr := Dot(query, item.vector)
		if derr != nil {
			return nil, derr
		}
		hit := item.hit
		hit.Score = score
		items = append(items, scoredHit{score: score, id: id, hit: hit})
	}
	s.stats = SearchStats{Duration: time.Since(start), Chunks: len(s.order)}
	return pickTop(items, k), nil
}

// LastSearchStats 返回最近一次成功检索的统计。
func (s *FakeStore) LastSearchStats() SearchStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

func (s *FakeStore) mismatchLocked() error {
	if s.meta == nil {
		return nil
	}
	have := s.meta.Identity()
	if have == s.current {
		return nil
	}
	return &EmbedderMismatchError{Have: have, Want: s.current}
}

func (s *FakeStore) apply(items []stored) {
	for _, item := range items {
		if _, ok := s.chunks[item.id]; !ok {
			s.order = append(s.order, item.id)
		}
		s.chunks[item.id] = item
	}
}

func prepareChunks(meta IndexMeta, chunks []Chunk, existing map[string]stored) ([]stored, error) {
	seen := make(map[string]struct{}, len(chunks))
	out := make([]stored, 0, len(chunks))
	for _, ch := range chunks {
		if ch.ID == "" || ch.DocID == "" {
			return nil, fmt.Errorf("%w: chunk 缺少 id 或 doc_id", ErrInvalidArgument)
		}
		if _, dup := seen[ch.ID]; dup {
			return nil, fmt.Errorf("%w: 重复的 chunk id %s", ErrInvalidArgument, ch.ID)
		}
		seen[ch.ID] = struct{}{}
		if prev, ok := existing[ch.ID]; ok && prev.docID != ch.DocID {
			return nil, fmt.Errorf("%w: chunk %s 的 doc_id 不能改变", ErrInvalidArgument, ch.ID)
		}
		if ch.EmbedderID != "" && ch.EmbedderID != meta.EmbedderID {
			return nil, &EmbedderMismatchError{
				Have: meta.Identity(),
				Want: EmbedderIdentity{ID: ch.EmbedderID, Dim: len(ch.Vector)},
			}
		}
		vec, err := prepareVector(ch.Vector, meta.Dim, meta.Normalize)
		if err != nil {
			return nil, err
		}
		out = append(out, stored{
			id:     ch.ID,
			docID:  ch.DocID,
			vector: vec,
			hit: Hit{
				ChunkID:   ch.ID,
				DocID:     ch.DocID,
				Text:      ch.Text,
				TitlePath: ch.TitlePath,
				Strategy:  ch.Strategy,
			},
		})
	}
	return out, nil
}
