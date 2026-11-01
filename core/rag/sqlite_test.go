package rag_test

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/rag"
	"github.com/loveRyujin/lantern/core/workspace"
)

func TestSQLiteStoreUpsertSearchDelete(t *testing.T) {
	ctx := t.Context()
	h := mustWorkspace(t, "", "xinghe")
	id := rag.EmbedderIdentity{ID: "fake:deterministic", Dim: 2}
	store := openStore(t, h, id)
	if err := store.Upsert(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Index(ctx); err != nil || ok {
		t.Fatalf("空批次创建了索引 ok=%v err=%v", ok, err)
	}
	hits, err := store.Search(ctx, []float32{1, 0}, 3)
	if err != nil || hits != nil {
		t.Fatalf("空库 hits=%v err=%v", hits, err)
	}
	if store.LastSearchStats().Chunks != 0 {
		t.Fatalf("空库统计 = %+v", store.LastSearchStats())
	}

	raw := []float32{3, 0}
	if err := store.Upsert(ctx, []rag.Chunk{
		{ID: "a", DocID: "d1", Text: "甲'口径", Vector: raw, TitlePath: "制度/口径", Strategy: rag.StrategyFixed},
		{ID: "c", DocID: "d1", Text: "丙", Vector: []float32{1, 1}, Strategy: rag.StrategyStructural},
		{ID: "b", DocID: "d2", Text: "乙", Vector: []float32{0, 4}},
	}); err != nil {
		t.Fatal(err)
	}
	if raw[0] != 3 {
		t.Fatal("入库改写了调用方的向量")
	}
	meta, ok, err := store.Index(ctx)
	if err != nil || !ok || !meta.Normalize || meta.EmbedderID != id.ID || meta.Dim != 2 || meta.CreatedAt.IsZero() {
		t.Fatalf("index_meta = %+v ok=%v err=%v", meta, ok, err)
	}
	assertBlob(t, h.KB, "a", []float32{1, 0})

	query := []float32{2, 0}
	hits, err = store.Search(ctx, query, 2)
	if err != nil {
		t.Fatal(err)
	}
	if query[0] != 2 {
		t.Fatal("检索改写了查询向量")
	}
	if len(hits) != 2 || hits[0].ChunkID != "a" || hits[1].ChunkID != "c" {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].Score != 1 || hits[0].Text != "甲'口径" || hits[0].TitlePath != "制度/口径" || hits[0].Strategy != rag.StrategyFixed {
		t.Fatalf("甲 = %+v", hits[0])
	}
	if hits[1].Strategy != rag.StrategyStructural {
		t.Fatalf("丙 = %+v", hits[1])
	}
	wantC, err := rag.Dot(mustNorm(t, []float32{1, 0}), mustNorm(t, []float32{1, 1}))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(float64(hits[1].Score-wantC)) > 1e-5 {
		t.Fatalf("丙的分数 = %v，想要 %v", hits[1].Score, wantC)
	}
	stats := store.LastSearchStats()
	if stats.Chunks != 3 || stats.Duration <= 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if _, err := store.Search(ctx, []float32{1, 0, 0}, 1); !errors.Is(err, rag.ErrDimension) {
		t.Fatalf("维度不符 err=%v", err)
	}
	if store.LastSearchStats() != stats {
		t.Fatal("失败的检索不应该更新统计")
	}

	if err := store.Delete(ctx, "missing"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "d1"); err != nil {
		t.Fatal(err)
	}
	n, err := store.Count(ctx)
	if err != nil || n != 1 {
		t.Fatalf("删除后 count=%d err=%v", n, err)
	}
	hits, err = store.Search(ctx, []float32{0, 1}, 3)
	if err != nil || len(hits) != 1 || hits[0].ChunkID != "b" || hits[0].Score != 1 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	if store.LastSearchStats().Chunks != 1 {
		t.Fatalf("删除后参与比较的 chunk 数 = %d", store.LastSearchStats().Chunks)
	}
	if err := store.Delete(ctx, "d2"); err != nil {
		t.Fatal(err)
	}
	hits, err = store.Search(ctx, []float32{1, 0}, 3)
	if err != nil || hits != nil {
		t.Fatalf("空库 hits=%v err=%v", hits, err)
	}
	if store.LastSearchStats().Chunks != 0 {
		t.Fatal("空库统计应该是 0 条")
	}
	var docs int
	if err := h.KB.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents`).Scan(&docs); err != nil {
		t.Fatal(err)
	}
	if docs != 2 {
		t.Fatalf("Delete 不应删掉 documents，还剩 %d", docs)
	}
}

func TestSQLiteStoreFakeEmbedderRoundTrip(t *testing.T) {
	ctx := t.Context()
	h := mustWorkspace(t, "", "ws")
	emb, err := rag.NewFakeEmbedder("fake:deterministic", 8)
	if err != nil {
		t.Fatal(err)
	}
	store := openStore(t, h, rag.IdentityOf(emb))
	vecs, err := emb.Embed(ctx, []string{"星禾口径", "另一句"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []rag.Chunk{
		{ID: "c1", DocID: "doc", Text: "星禾口径", Vector: vecs[0], EmbedderID: emb.ID()},
		{ID: "c2", DocID: "doc", Text: "另一句", Vector: vecs[1], EmbedderID: emb.ID()},
	}); err != nil {
		t.Fatal(err)
	}
	query, err := emb.Embed(ctx, []string{"星禾口径"})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := store.Search(ctx, query[0], 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ChunkID != "c1" || hits[0].Text != "星禾口径" || hits[0].Score < 0.99 {
		t.Fatalf("hits = %+v", hits)
	}
	if store.LastSearchStats().Chunks != 2 {
		t.Fatalf("stats = %+v", store.LastSearchStats())
	}
}

func TestSQLiteStorePersistsAcrossOpen(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h := mustWorkspace(t, root, "xinghe")
	id := rag.EmbedderIdentity{ID: "fake", Dim: 2}
	store := openStore(t, h, id)
	if err := store.Upsert(ctx, []rag.Chunk{{ID: "a", DocID: "d", Text: "留下", Vector: []float32{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := workspace.Open(ctx, root, "xinghe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	again := openStore(t, reopened, id)
	hits, err := again.Search(ctx, []float32{1, 0}, 1)
	if err != nil || len(hits) != 1 || hits[0].Text != "留下" || hits[0].Score != 1 {
		t.Fatalf("重开后 hits=%+v err=%v", hits, err)
	}
}

func TestSQLiteStoreMismatchFailClosed(t *testing.T) {
	ctx := t.Context()
	h := mustWorkspace(t, "", "yunsong")
	original := rag.EmbedderIdentity{ID: "ollama:bge-m3", Dim: 4}
	store := openStore(t, h, original)
	old := []float32{1, 0, 0, 0}
	if err := store.Upsert(ctx, []rag.Chunk{{ID: "old", DocID: "doc", Text: "旧索引", Vector: old, EmbedderID: original.ID}}); err != nil {
		t.Fatal(err)
	}
	current := rag.EmbedderIdentity{ID: "openai:text-embedding-3-small", Dim: 4}
	switched := openStore(t, h, current)
	hits, err := switched.Search(ctx, old, 3)
	assertMismatch(t, err, original, current)
	if hits != nil {
		t.Fatalf("不一致时不应该返回命中: %+v", hits)
	}
	if switched.LastSearchStats() != (rag.SearchStats{}) {
		t.Fatal("失败的检索不应该记统计")
	}
	err = switched.Upsert(ctx, []rag.Chunk{{ID: "new", DocID: "doc", Text: "新", Vector: old}})
	assertMismatch(t, err, original, current)
	n, err := switched.Count(ctx)
	if err != nil || n != 1 {
		t.Fatalf("失败的写入改变了库: %d %v", n, err)
	}
	if err := switched.Delete(ctx, "doc"); err != nil {
		t.Fatal(err)
	}
	n, err = switched.Count(ctx)
	if err != nil || n != 0 {
		t.Fatalf("不一致时删除失败 count=%d err=%v", n, err)
	}
	meta, ok, err := switched.Index(ctx)
	if err != nil || !ok || meta.EmbedderID != original.ID {
		t.Fatalf("Delete 不应改写 index_meta: %+v ok=%v err=%v", meta, ok, err)
	}
	hits, err = switched.Search(ctx, old, 1)
	assertMismatch(t, err, original, current)
	if hits != nil {
		t.Fatal(hits)
	}

	dimChanged := openStore(t, h, rag.EmbedderIdentity{ID: original.ID, Dim: 8})
	_, err = dimChanged.Search(ctx, old, 1)
	if !errors.Is(err, rag.ErrEmbedderMismatch) {
		t.Fatalf("同名不同维 err=%v", err)
	}
}

func TestSQLiteStoreRejectsPartialBatch(t *testing.T) {
	ctx := t.Context()
	h := mustWorkspace(t, "", "ws")
	id := rag.EmbedderIdentity{ID: "ollama:bge-m3", Dim: 2}
	store := openStore(t, h, id)
	err := store.Upsert(ctx, []rag.Chunk{
		{ID: "ok", DocID: "d", Vector: []float32{1, 0}, EmbedderID: id.ID},
		{ID: "bad", DocID: "d", Vector: []float32{0, 1}, EmbedderID: "other-model"},
	})
	assertMismatch(t, err, id, rag.EmbedderIdentity{ID: "other-model", Dim: 2})
	assertEmptyKB(t, h.KB)

	err = store.Upsert(ctx, []rag.Chunk{
		{ID: "ok", DocID: "d", Vector: []float32{1, 0}},
		{ID: "bad-dim", DocID: "d", Vector: []float32{1}},
	})
	if !errors.Is(err, rag.ErrDimension) {
		t.Fatalf("err=%v", err)
	}
	assertEmptyKB(t, h.KB)

	err = store.Upsert(ctx, []rag.Chunk{
		{ID: "ok", DocID: "d", Vector: []float32{1, 0}},
		{ID: "zero", DocID: "d", Vector: []float32{0, 0}},
	})
	if !errors.Is(err, rag.ErrZeroVector) {
		t.Fatalf("err=%v", err)
	}
	assertEmptyKB(t, h.KB)
}

func TestSQLiteStoreKeepsImportedDocument(t *testing.T) {
	ctx := t.Context()
	h := mustWorkspace(t, "", "ws")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.KB.ExecContext(ctx,
		`INSERT INTO documents (id, title, source, body, created_at, updated_at) VALUES ('d', '标题', 'src', '原文', ?, ?)`,
		now, now,
	); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, h, rag.EmbedderIdentity{ID: "fake", Dim: 2})
	if err := store.Upsert(ctx, []rag.Chunk{{ID: "a", DocID: "d", Text: "片段", Vector: []float32{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := h.KB.QueryRowContext(ctx, `SELECT body FROM documents WHERE id = 'd'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if body != "原文" {
		t.Fatalf("正文被改成 %q", body)
	}
	var ordinals []int
	if err := store.Upsert(ctx, []rag.Chunk{{ID: "b", DocID: "d", Text: "第二", Vector: []float32{0, 1}}}); err != nil {
		t.Fatal(err)
	}
	rows, err := h.KB.QueryContext(ctx, `SELECT ordinal FROM chunks WHERE document_id = 'd' ORDER BY ordinal`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		ordinals = append(ordinals, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ordinals) != 2 || ordinals[0] != 0 || ordinals[1] != 1 {
		t.Fatalf("ordinals = %v", ordinals)
	}
}

func TestSQLiteStoreTieBreakAndReplace(t *testing.T) {
	ctx := t.Context()
	h := mustWorkspace(t, "", "ws")
	store := openStore(t, h, rag.EmbedderIdentity{ID: "fake", Dim: 2})
	vec := []float32{1, 0}
	if err := store.Upsert(ctx, []rag.Chunk{
		{ID: "b", DocID: "d", Text: "b", Vector: vec},
		{ID: "a", DocID: "d", Text: "a", Vector: vec},
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := store.Search(ctx, vec, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].ChunkID != "a" || hits[1].ChunkID != "b" {
		t.Fatalf("平分时应按 id 排序: %+v", hits)
	}
	if err := store.Upsert(ctx, []rag.Chunk{{ID: "a", DocID: "d", Text: "a2", Vector: []float32{0, 1}}}); err != nil {
		t.Fatal(err)
	}
	hits, err = store.Search(ctx, []float32{0, 1}, 1)
	if err != nil || len(hits) != 1 || hits[0].Text != "a2" || hits[0].Score != 1 {
		t.Fatalf("替换后的命中 = %+v err=%v", hits, err)
	}
	err = store.Upsert(ctx, []rag.Chunk{{ID: "a", DocID: "other", Text: "x", Vector: vec}})
	if err == nil || !errors.Is(err, rag.ErrInvalidArgument) {
		t.Fatalf("err=%v", err)
	}
}

func TestSQLiteStoreNormalizeFalse(t *testing.T) {
	ctx := t.Context()
	h := mustWorkspace(t, "", "ws")
	id := rag.EmbedderIdentity{ID: "fake", Dim: 2}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.KB.ExecContext(ctx,
		`INSERT INTO index_meta (id, embedder_id, dim, normalize, created_at) VALUES (1, ?, ?, 0, ?)`,
		id.ID, id.Dim, now,
	); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, h, id)
	if err := store.Upsert(ctx, []rag.Chunk{{ID: "c", DocID: "d", Vector: []float32{1, 2}}}); err != nil {
		t.Fatal(err)
	}
	assertBlob(t, h.KB, "c", []float32{1, 2})
	hits, err := store.Search(ctx, []float32{1, 2}, 1)
	if err != nil || len(hits) != 1 || hits[0].Score != 5 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
}

func TestSQLiteStoreWorkspacesAreSeparateFiles(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	a := mustWorkspace(t, root, "xinghe")
	b := mustWorkspace(t, root, "yunsong")
	id := rag.EmbedderIdentity{ID: "fake", Dim: 2}
	storeA := openStore(t, a, id)
	storeB := openStore(t, b, id)
	if err := storeA.Upsert(ctx, []rag.Chunk{{ID: "c", DocID: "d", Text: "星禾", Vector: []float32{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	hits, err := storeB.Search(ctx, []float32{1, 0}, 3)
	if err != nil || hits != nil {
		t.Fatalf("另一个工作区看到了命中: %+v %v", hits, err)
	}
	var n int
	if err := b.KB.QueryRowContext(ctx, `SELECT COUNT(*) FROM vectors`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("云杉库里有 %d 条向量", n)
	}
	hits, err = storeA.Search(ctx, []float32{1, 0}, 1)
	if err != nil || len(hits) != 1 || hits[0].Text != "星禾" {
		t.Fatalf("星禾 hits=%+v err=%v", hits, err)
	}
}

func TestSQLiteStoreCorruptVectorFailClosed(t *testing.T) {
	ctx := t.Context()
	h := mustWorkspace(t, "", "ws")
	id := rag.EmbedderIdentity{ID: "fake", Dim: 2}
	store := openStore(t, h, id)
	if err := store.Upsert(ctx, []rag.Chunk{{ID: "a", DocID: "d", Text: "甲", Vector: []float32{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	hits, err := store.Search(ctx, []float32{1, 0}, 1)
	if err != nil || len(hits) != 1 {
		t.Fatal(err)
	}
	stats := store.LastSearchStats()
	if _, err := h.KB.ExecContext(ctx, `UPDATE vectors SET embedding = x'00' WHERE chunk_id = 'a'`); err != nil {
		t.Fatal(err)
	}
	hits, err = store.Search(ctx, []float32{1, 0}, 1)
	if hits != nil || !errors.Is(err, rag.ErrDimension) {
		t.Fatalf("坏 BLOB hits=%v err=%v", hits, err)
	}
	if store.LastSearchStats() != stats {
		t.Fatal("坏向量的检索不应该更新统计")
	}
	if _, err := h.KB.ExecContext(ctx, `UPDATE vectors SET embedding = x'0000000000000000', embedder_id = 'other' WHERE chunk_id = 'a'`); err != nil {
		t.Fatal(err)
	}
	hits, err = store.Search(ctx, []float32{1, 0}, 1)
	if hits != nil || !errors.Is(err, rag.ErrEmbedderMismatch) {
		t.Fatalf("串模型行 hits=%v err=%v", hits, err)
	}
}

func TestSQLiteStoreRejectsOrphanVectors(t *testing.T) {
	ctx := t.Context()
	h := mustWorkspace(t, "", "ws")
	id := rag.EmbedderIdentity{ID: "fake", Dim: 2}
	store := openStore(t, h, id)
	if err := store.Upsert(ctx, []rag.Chunk{{ID: "a", DocID: "d", Vector: []float32{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.KB.ExecContext(ctx, `DELETE FROM index_meta`); err != nil {
		t.Fatal(err)
	}
	hits, err := store.Search(ctx, []float32{1, 0}, 1)
	if hits != nil || err == nil {
		t.Fatalf("没有 index_meta 仍返回了命中: %+v %v", hits, err)
	}
	err = store.Upsert(ctx, []rag.Chunk{{ID: "b", DocID: "d", Vector: []float32{0, 1}}})
	if err == nil {
		t.Fatal("没有 index_meta 时不应接收新向量")
	}
	if err := store.Delete(ctx, "d"); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []rag.Chunk{{ID: "b", DocID: "d2", Text: "新", Vector: []float32{0, 1}}}); err != nil {
		t.Fatal(err)
	}
	hits, err = store.Search(ctx, []float32{0, 1}, 1)
	if err != nil || len(hits) != 1 || hits[0].ChunkID != "b" {
		t.Fatalf("清干净后 hits=%+v err=%v", hits, err)
	}
}

func TestSQLiteStoreRejectsWrongDatabase(t *testing.T) {
	ctx := t.Context()
	h := mustWorkspace(t, "", "ws")
	id := rag.EmbedderIdentity{ID: "fake", Dim: 2}
	if _, err := rag.NewSQLiteStore(ctx, nil, id); err == nil {
		t.Fatal("nil db 应该失败")
	}
	if _, err := rag.NewSQLiteStore(ctx, h.KB, rag.EmbedderIdentity{}); err == nil {
		t.Fatal("空身份应该失败")
	}
	if _, err := rag.NewSQLiteStore(ctx, h.Analytics, id); err == nil {
		t.Fatal("analytics.db 不应该被当成向量库")
	}
	store := openStore(t, h, id)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.Upsert(canceled, []rag.Chunk{{ID: "a", DocID: "d", Vector: []float32{1, 0}}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后的 Upsert err=%v", err)
	}
	assertEmptyKB(t, h.KB)
	if _, err := store.Search(ctx, []float32{1, 0}, 0); !errors.Is(err, rag.ErrInvalidArgument) {
		t.Fatalf("k=0 err=%v", err)
	}
}

func TestOllamaEmbedderWritesWorkspaceKB(t *testing.T) {
	ctx := t.Context()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Model != "bge-m3" {
			http.Error(w, "model", http.StatusBadRequest)
			return
		}
		out := make([][]float64, len(req.Input))
		for i, text := range req.Input {
			if text == "乙" {
				out[i] = []float64{0, 4}
				continue
			}
			out[i] = []float64{3, 0}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": out})
	}))
	t.Cleanup(srv.Close)

	emb, err := rag.NewOllamaEmbedder(
		rag.WithOllamaBaseURL(srv.URL),
		rag.WithOllamaDim(2),
	)
	if err != nil {
		t.Fatal(err)
	}
	h := mustWorkspace(t, "", "xinghe")
	store := openStore(t, h, rag.IdentityOf(emb))
	vecs, err := emb.Embed(ctx, []string{"甲", "乙"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, []rag.Chunk{
		{ID: "a", DocID: "d", Text: "甲", Vector: vecs[0], EmbedderID: emb.ID()},
		{ID: "b", DocID: "d", Text: "乙", Vector: vecs[1], EmbedderID: emb.ID()},
	}); err != nil {
		t.Fatal(err)
	}
	query, err := emb.Embed(ctx, []string{"甲"})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := store.Search(ctx, query[0], 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ChunkID != "a" || hits[0].Score != 1 {
		t.Fatalf("hits = %+v", hits)
	}
	if store.LastSearchStats().Chunks != 2 || store.LastSearchStats().Duration <= 0 {
		t.Fatalf("stats = %+v", store.LastSearchStats())
	}
	if err := store.Delete(ctx, "d"); err != nil {
		t.Fatal(err)
	}
	hits, err = store.Search(ctx, query[0], 1)
	if err != nil || hits != nil {
		t.Fatalf("删除后 hits=%v err=%v", hits, err)
	}
}

func mustWorkspace(t *testing.T, root, id string) *workspace.Handle {
	t.Helper()
	if root == "" {
		root = t.TempDir()
	}
	h, err := workspace.Create(t.Context(), root, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func openStore(t *testing.T, h *workspace.Handle, id rag.EmbedderIdentity) *rag.SQLiteStore {
	t.Helper()
	store, err := rag.NewSQLiteStore(t.Context(), h.KB, id)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func assertEmptyKB(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"documents", "chunks", "vectors", "index_meta"} {
		var n int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s 留下了 %d 行", table, n)
		}
	}
}

func assertBlob(t *testing.T, db *sql.DB, chunkID string, want []float32) {
	t.Helper()
	var blob []byte
	var dim int
	if err := db.QueryRowContext(t.Context(), `SELECT dim, embedding FROM vectors WHERE chunk_id = ?`, chunkID).Scan(&dim, &blob); err != nil {
		t.Fatal(err)
	}
	if dim != len(want) || len(blob) != len(want)*4 {
		t.Fatalf("dim=%d blob=%d", dim, len(blob))
	}
	got := make([]float32, len(want))
	for i := range want {
		got[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4 : i*4+4]))
	}
	if !vecClose(got, want) {
		t.Fatalf("blob = %v，想要 %v", got, want)
	}
}
