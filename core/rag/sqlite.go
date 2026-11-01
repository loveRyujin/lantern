package rag

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// SQLiteStore 是绑定单个工作区 kb.db 的 VectorStore。
//
// db 必须是这个工作区自己的库，例如 workspace.Handle.KB。
// 构造函数不接收 workspace，SQL 也不按工作区过滤；隔离来自打开不同的文件。
// 向量以 little-endian float32 BLOB 存放。index_meta.normalize 为真时，入库保存 L2 归一化副本，不改调用方的切片。
// 检索把该库中的向量载入内存，对已归一化向量做点积（即余弦），再用小顶堆取 top-k。
// 成功的 Search 记下耗时和参与比较的 chunk 数；失败不更新 LastSearchStats。
type SQLiteStore struct {
	db      *sql.DB
	current EmbedderIdentity
	mu      sync.Mutex
	stats   SearchStats
}

var _ VectorStore = (*SQLiteStore)(nil)

// NewSQLiteStore 检查 kb 已有工作区向量表，并记下当前 Embedder 身份。
// 索引身份和 current 不一致时仍然打开；之后 Upsert 和 Search 返回 ErrEmbedderMismatch。
func NewSQLiteStore(ctx context.Context, kb *sql.DB, current EmbedderIdentity) (*SQLiteStore, error) {
	if ctx == nil {
		return nil, errors.New("rag: ctx 不能为 nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if kb == nil {
		return nil, fmt.Errorf("%w: 缺少 kb.db", ErrInvalidArgument)
	}
	if err := current.Validate(); err != nil {
		return nil, err
	}
	if err := kb.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("rag: 打开 kb.db: %w", err)
	}
	if err := probeKBSchema(ctx, kb); err != nil {
		return nil, err
	}
	return &SQLiteStore{db: kb, current: current}, nil
}

// Index 读取 index_meta。还没有建索引时 ok 为 false。
func (s *SQLiteStore) Index(ctx context.Context) (IndexMeta, bool, error) {
	if err := s.check(ctx); err != nil {
		return IndexMeta{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, err := readMeta(ctx, s.db)
	if err != nil || meta == nil {
		return IndexMeta{}, false, err
	}
	return *meta, true, nil
}

// Count 返回当前向量条数。
func (s *SQLiteStore) Count(ctx context.Context) (int, error) {
	if err := s.check(ctx); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return countVectors(ctx, s.db)
}

// Upsert 写入或按 chunk ID 替换。空批次是 no-op，不会创建 index_meta。
// 整批在一个事务里提交；校验失败或中途出错时不留下这批的向量。
func (s *SQLiteStore) Upsert(ctx context.Context, chunks []Chunk) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if len(chunks) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	meta, err := readMeta(ctx, tx)
	if err != nil {
		return err
	}
	if err := mismatchError(meta, s.current); err != nil {
		return err
	}
	fresh := false
	if meta == nil {
		n, err := countVectors(ctx, tx)
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w: 存在向量但没有 index_meta", ErrInvalidArgument)
		}
		now := time.Now().UTC()
		meta = &IndexMeta{
			EmbedderID: s.current.ID,
			Dim:        s.current.Dim,
			Normalize:  true,
			CreatedAt:  now,
		}
		fresh = true
	}
	existing, err := loadChunkDocs(ctx, tx)
	if err != nil {
		return err
	}
	prepared, err := prepareChunks(*meta, chunks, existing)
	if err != nil {
		return err
	}
	if fresh {
		if err := insertMeta(ctx, tx, *meta); err != nil {
			return err
		}
	}
	if err := writeChunks(ctx, tx, *meta, prepared, existing); err != nil {
		return err
	}
	return tx.Commit()
}

// Delete 删除属于 docID 的全部分片及其向量。文档不存在时返回 nil。
// 不删除 documents 行，也不改 index_meta。模型不一致时仍允许删除。
func (s *SQLiteStore) Delete(ctx context.Context, docID string) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if docID == "" {
		return fmt.Errorf("%w: 缺少 doc_id", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM vectors WHERE chunk_id IN (SELECT id FROM chunks WHERE document_id = ?)`,
		docID,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE document_id = ?`, docID); err != nil {
		return err
	}
	return tx.Commit()
}

// Search 用点积取 top-k。k 必须大于 0。
// 模型不一致时返回 ErrEmbedderMismatch，命中为 nil，不更新统计。
// 查询向量长度不等于索引维度时返回 ErrDimension，同样不返回命中。
func (s *SQLiteStore) Search(ctx context.Context, vec []float32, k int) ([]Hit, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if k <= 0 {
		return nil, fmt.Errorf("%w: k 必须大于 0", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	meta, err := readMeta(ctx, s.db)
	if err != nil {
		return nil, err
	}
	if err := mismatchError(meta, s.current); err != nil {
		return nil, err
	}
	n, err := countVectors(ctx, s.db)
	if err != nil {
		return nil, err
	}
	if meta == nil {
		if n > 0 {
			return nil, fmt.Errorf("%w: 存在向量但没有 index_meta", ErrInvalidArgument)
		}
		s.stats = SearchStats{}
		return nil, nil
	}
	if n == 0 {
		s.stats = SearchStats{}
		return nil, nil
	}
	if len(vec) != meta.Dim {
		return nil, &DimensionError{Got: len(vec), Want: meta.Dim}
	}
	query, err := prepareVector(vec, meta.Dim, meta.Normalize)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.document_id, c.text, c.title_path, c.strategy,
		       v.embedder_id, v.dim, v.embedding
		FROM vectors AS v
		INNER JOIN chunks AS c ON c.id = v.chunk_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]scoredHit, 0, n)
	for rows.Next() {
		var id, docID, text, title, strategy, embedder string
		var dim int
		var blob []byte
		if err := rows.Scan(&id, &docID, &text, &title, &strategy, &embedder, &dim, &blob); err != nil {
			return nil, err
		}
		rowID := EmbedderIdentity{ID: embedder, Dim: dim}
		if rowID != meta.Identity() {
			return nil, &EmbedderMismatchError{Have: rowID, Want: s.current}
		}
		vector, err := decodeVec(blob, meta.Dim)
		if err != nil {
			return nil, err
		}
		score, err := Dot(query, vector)
		if err != nil {
			return nil, err
		}
		items = append(items, scoredHit{
			score: score,
			id:    id,
			hit: Hit{
				ChunkID:   id,
				DocID:     docID,
				Text:      text,
				TitlePath: title,
				Strategy:  strategy,
			},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.stats = SearchStats{Duration: time.Since(start), Chunks: len(items)}
	return pickTop(items, k), nil
}

// LastSearchStats 返回最近一次成功检索的统计。
func (s *SQLiteStore) LastSearchStats() SearchStats {
	if s == nil {
		return SearchStats{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

func (s *SQLiteStore) check(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: 缺少 kb.db", ErrInvalidArgument)
	}
	if ctx == nil {
		return errors.New("rag: ctx 不能为 nil")
	}
	return ctx.Err()
}

type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func probeKBSchema(ctx context.Context, db *sql.DB) error {
	probes := []string{
		`SELECT id, title, source, body, created_at, updated_at FROM documents LIMIT 0`,
		`SELECT id, document_id, ordinal, text, embedder_id, title_path, strategy FROM chunks LIMIT 0`,
		`SELECT chunk_id, embedder_id, dim, embedding FROM vectors LIMIT 0`,
		`SELECT id, embedder_id, dim, normalize, created_at FROM index_meta LIMIT 0`,
	}
	for _, query := range probes {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return fmt.Errorf("%w: kb.db 缺少向量库表或列（需要 documents、chunks.title_path、chunks.strategy、vectors、index_meta）: %w", ErrInvalidArgument, err)
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

func readMeta(ctx context.Context, q rowQueryer) (*IndexMeta, error) {
	var embedder, created string
	var dim, norm int
	err := q.QueryRowContext(ctx,
		`SELECT embedder_id, dim, normalize, created_at FROM index_meta WHERE id = 1`,
	).Scan(&embedder, &dim, &norm, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if embedder == "" || dim <= 0 {
		return nil, fmt.Errorf("%w: index_meta 不完整", ErrInvalidArgument)
	}
	ts, err := parseMetaTime(created)
	if err != nil {
		return nil, err
	}
	return &IndexMeta{
		EmbedderID: embedder,
		Dim:        dim,
		Normalize:  norm != 0,
		CreatedAt:  ts,
	}, nil
}

func parseMetaTime(created string) (time.Time, error) {
	if ts, err := time.Parse(time.RFC3339Nano, created); err == nil {
		return ts, nil
	}
	if ts, err := time.Parse(time.RFC3339, created); err == nil {
		return ts, nil
	}
	return time.Time{}, fmt.Errorf("%w: index_meta.created_at 无法解析", ErrInvalidArgument)
}

func countVectors(ctx context.Context, q rowQueryer) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM vectors`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func loadChunkDocs(ctx context.Context, tx *sql.Tx) (map[string]stored, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, document_id FROM chunks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]stored{}
	for rows.Next() {
		var id, docID string
		if err := rows.Scan(&id, &docID); err != nil {
			return nil, err
		}
		out[id] = stored{id: id, docID: docID}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func insertMeta(ctx context.Context, tx *sql.Tx, meta IndexMeta) error {
	norm := 0
	if meta.Normalize {
		norm = 1
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO index_meta (id, embedder_id, dim, normalize, created_at) VALUES (1, ?, ?, ?, ?)`,
		meta.EmbedderID, meta.Dim, norm, meta.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

func writeChunks(ctx context.Context, tx *sql.Tx, meta IndexMeta, items []stored, existing map[string]stored) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	nextOrdinal := map[string]int{}
	for _, item := range items {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO documents (id, title, source, body, created_at, updated_at)
			 VALUES (?, ?, '', '', ?, ?)
			 ON CONFLICT(id) DO NOTHING`,
			item.docID, item.docID, now, now,
		); err != nil {
			return err
		}
		if _, ok := existing[item.id]; ok {
			if _, err := tx.ExecContext(ctx,
				`UPDATE chunks SET text = ?, embedder_id = ?, title_path = ?, strategy = ? WHERE id = ?`,
				item.hit.Text, meta.EmbedderID, item.hit.TitlePath, item.hit.Strategy, item.id,
			); err != nil {
				return err
			}
		} else {
			ordinal, err := nextChunkOrdinal(ctx, tx, item.docID, nextOrdinal)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO chunks (id, document_id, ordinal, text, embedder_id, title_path, strategy)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				item.id, item.docID, ordinal, item.hit.Text, meta.EmbedderID, item.hit.TitlePath, item.hit.Strategy,
			); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO vectors (chunk_id, embedder_id, dim, embedding)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(chunk_id) DO UPDATE SET
			   embedder_id = excluded.embedder_id,
			   dim = excluded.dim,
			   embedding = excluded.embedding`,
			item.id, meta.EmbedderID, meta.Dim, encodeVec(item.vector),
		); err != nil {
			return err
		}
	}
	return nil
}

func nextChunkOrdinal(ctx context.Context, tx *sql.Tx, docID string, next map[string]int) (int, error) {
	if n, ok := next[docID]; ok {
		next[docID] = n + 1
		return n, nil
	}
	var max sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(ordinal) FROM chunks WHERE document_id = ?`, docID).Scan(&max); err != nil {
		return 0, err
	}
	n := 0
	if max.Valid {
		n = int(max.Int64) + 1
	}
	next[docID] = n + 1
	return n, nil
}

func encodeVec(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, x := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(x))
	}
	return buf
}

func decodeVec(b []byte, dim int) ([]float32, error) {
	if dim <= 0 || len(b) != dim*4 {
		got := 0
		if len(b) > 0 && len(b)%4 == 0 {
			got = len(b) / 4
		}
		return nil, &DimensionError{Got: got, Want: dim}
	}
	out := make([]float32, dim)
	for i := 0; i < dim; i++ {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4 : i*4+4]))
	}
	if err := finite(out); err != nil {
		return nil, err
	}
	return out, nil
}
