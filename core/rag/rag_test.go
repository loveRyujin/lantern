package rag_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/loveRyujin/lantern/core/errcode"
	"github.com/loveRyujin/lantern/core/rag"
)

func TestFakeEmbedderDeterministic(t *testing.T) {
	a, err := rag.NewFakeEmbedder("fake:deterministic", 32)
	if err != nil {
		t.Fatal(err)
	}
	b, err := rag.NewFakeEmbedder("", 32)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() != b.ID() || a.Dim() != 32 {
		t.Fatalf("身份 = %s/%d", a.ID(), a.Dim())
	}
	ctx := context.Background()
	first, err := a.Embed(ctx, []string{"口径", "另一句"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Embed(ctx, []string{"口径", "另一句"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || len(first[0]) != 32 || !vecClose(first[0], second[0]) || vecClose(first[0], first[1]) {
		t.Fatal("相同文本应该得到相同向量，不同文本不应该相同")
	}
	if _, err := a.Embed(ctx, nil); err != nil {
		t.Fatal(err)
	}
	norm, err := rag.L2Normalize(first[0])
	if err != nil {
		t.Fatal(err)
	}
	if !vecClose(norm, first[0]) {
		t.Fatal("Embed 应该返回已经归一化的向量")
	}
	if _, err := rag.NewFakeEmbedder("x", 0); err == nil {
		t.Fatal("维度 0 应该被拒绝")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := a.Embed(cancelled, []string{"x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后的 Embed err=%v", err)
	}
}

func TestVectorStoreCosineAndDimension(t *testing.T) {
	ctx := context.Background()
	current := rag.EmbedderIdentity{ID: "ollama:bge-m3", Dim: 2}
	store, err := rag.NewFakeStore("xinghe", current, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Index(); ok {
		t.Fatal("空批次不应该创建 index_meta")
	}
	err = store.Upsert(ctx, []rag.Chunk{
		{ID: "a", DocID: "d1", Text: "甲", Vector: []float32{3, 0}, Strategy: rag.StrategyFixed},
		{ID: "c", DocID: "d1", Text: "丙", Vector: []float32{1, 1}, Strategy: rag.StrategyStructural},
		{ID: "b", DocID: "d2", Text: "乙", Vector: []float32{0, 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	meta, ok := store.Index()
	if !ok || !meta.Normalize || meta.EmbedderID != current.ID || meta.Dim != 2 || meta.CreatedAt.IsZero() {
		t.Fatalf("index_meta = %+v ok=%v", meta, ok)
	}
	hits, err := store.Search(ctx, []float32{2, 0}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].ChunkID != "a" || hits[1].ChunkID != "c" {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].Score != 1 {
		t.Fatalf("甲的余弦 = %v，想要 1", hits[0].Score)
	}
	wantC, err := rag.Dot(mustNorm(t, []float32{1, 0}), mustNorm(t, []float32{1, 1}))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(float64(hits[1].Score-wantC)) > 1e-5 {
		t.Fatalf("丙的分数 = %v，想要 %v", hits[1].Score, wantC)
	}
	stats := store.LastSearchStats()
	if stats.Chunks != 3 || stats.Duration < 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if _, err := store.Search(ctx, []float32{1, 0, 0}, 1); !errors.Is(err, rag.ErrDimension) {
		t.Fatalf("维度不符 err=%v", err)
	}
	if store.LastSearchStats().Chunks != 3 {
		t.Fatal("失败的检索不应该更新统计")
	}

	wide := make([]float32, 1024)
	wide[0] = 1
	indexed := &rag.IndexMeta{EmbedderID: "ollama:bge-m3", Dim: 1024, Normalize: true}
	big, err := rag.NewFakeStore("xinghe", rag.EmbedderIdentity{ID: "ollama:bge-m3", Dim: 1024}, indexed, []rag.Chunk{{
		ID: "c1", DocID: "doc", Text: "口径", EmbedderID: "ollama:bge-m3", Vector: wide,
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = big.Search(ctx, []float32{1, 0, 0}, 1)
	if !errors.Is(err, rag.ErrDimension) {
		t.Fatalf("1024 维索引拒绝其他维度: %v", err)
	}
	var dimErr *rag.DimensionError
	if !errors.As(err, &dimErr) || dimErr.Got != 3 || dimErr.Want != 1024 {
		t.Fatalf("DimensionError = %+v", dimErr)
	}
}

func TestVectorStoreTieBreakAndReplace(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t, "ws", rag.EmbedderIdentity{ID: "fake", Dim: 2}, nil)
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
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ChunkID != "a" || hits[0].Text != "a2" || hits[0].Score != 1 {
		t.Fatalf("替换后的命中 = %+v", hits)
	}
	if err := store.Delete(ctx, "missing"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "d"); err != nil {
		t.Fatal(err)
	}
	if store.Count() != 0 {
		t.Fatalf("删除后还剩 %d", store.Count())
	}
	hits, err = store.Search(ctx, vec, 3)
	if err != nil || hits != nil {
		t.Fatalf("空库 hits=%v err=%v", hits, err)
	}
}

func TestEmbedderMismatch(t *testing.T) {
	ctx := context.Background()
	oldVec := []float32{1, 0, 0, 0}
	indexed := &rag.IndexMeta{EmbedderID: "ollama:bge-m3", Dim: 4, Normalize: true}
	current := rag.EmbedderIdentity{ID: "openai:text-embedding-3-small", Dim: 4}
	store, err := rag.NewFakeStore("yunsong", current, indexed, []rag.Chunk{{
		ID: "old", DocID: "doc", Text: "旧索引", EmbedderID: "ollama:bge-m3", Vector: oldVec,
	}})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := store.Search(ctx, oldVec, 3)
	assertMismatch(t, err, rag.EmbedderIdentity{ID: "ollama:bge-m3", Dim: 4}, current)
	if hits != nil {
		t.Fatalf("不一致时不应该返回命中: %+v", hits)
	}
	err = store.Upsert(ctx, []rag.Chunk{{ID: "new", DocID: "doc", Text: "新", Vector: oldVec, EmbedderID: current.ID}})
	assertMismatch(t, err, rag.EmbedderIdentity{ID: "ollama:bge-m3", Dim: 4}, current)
	if store.Count() != 1 {
		t.Fatalf("失败的写入改变了库: %d", store.Count())
	}
	if _, ok := errcode.CodeOf(err); !ok || !errors.Is(err, rag.ErrEmbedderMismatch) {
		t.Fatal("错误码不可识别")
	}

	sameDim := rag.EmbedderIdentity{ID: "other:1024", Dim: 1024}
	wide := make([]float32, 1024)
	wide[0] = 1
	meta1024 := &rag.IndexMeta{EmbedderID: "ollama:bge-m3", Dim: 1024, Normalize: true}
	store1024, err := rag.NewFakeStore("yunsong", sameDim, meta1024, []rag.Chunk{{
		ID: "c", DocID: "d", Vector: wide, EmbedderID: "ollama:bge-m3",
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store1024.Search(ctx, wide, 1)
	assertMismatch(t, err, rag.EmbedderIdentity{ID: "ollama:bge-m3", Dim: 1024}, sameDim)

	dimChanged := rag.EmbedderIdentity{ID: "ollama:bge-m3", Dim: 8}
	storeDim, err := rag.NewFakeStore("yunsong", dimChanged, indexed, []rag.Chunk{{
		ID: "old", DocID: "doc", Vector: oldVec, EmbedderID: "ollama:bge-m3",
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = storeDim.Search(ctx, oldVec, 1)
	if !errors.Is(err, rag.ErrEmbedderMismatch) {
		t.Fatalf("同名不同维 err=%v", err)
	}
}

func TestUpsertRejectsForeignChunkWithoutPartialWrite(t *testing.T) {
	ctx := context.Background()
	id := rag.EmbedderIdentity{ID: "ollama:bge-m3", Dim: 2}
	store := mustStore(t, "ws", id, nil)
	err := store.Upsert(ctx, []rag.Chunk{
		{ID: "ok", DocID: "d", Vector: []float32{1, 0}, EmbedderID: id.ID},
		{ID: "bad", DocID: "d", Vector: []float32{0, 1}, EmbedderID: "other-model"},
	})
	assertMismatch(t, err, id, rag.EmbedderIdentity{ID: "other-model", Dim: 2})
	if store.Count() != 0 {
		t.Fatalf("半批写入, count=%d", store.Count())
	}
	if _, ok := store.Index(); ok {
		t.Fatal("失败的首批写入不应该创建 index_meta")
	}
	err = store.Upsert(ctx, []rag.Chunk{
		{ID: "ok", DocID: "d", Vector: []float32{1, 0}},
		{ID: "bad-dim", DocID: "d", Vector: []float32{1}},
	})
	if !errors.Is(err, rag.ErrDimension) || store.Count() != 0 {
		t.Fatalf("err=%v count=%d", err, store.Count())
	}
}

func TestDeleteStillWorksWhenEmbedderMismatches(t *testing.T) {
	ctx := context.Background()
	indexed := &rag.IndexMeta{EmbedderID: "ollama:bge-m3", Dim: 2, Normalize: true}
	store, err := rag.NewFakeStore("ws", rag.EmbedderIdentity{ID: "other", Dim: 2}, indexed, []rag.Chunk{
		{ID: "a", DocID: "d1", Vector: []float32{1, 0}},
		{ID: "b", DocID: "d2", Vector: []float32{0, 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "d1"); err != nil {
		t.Fatal(err)
	}
	if store.Count() != 1 {
		t.Fatalf("count=%d", store.Count())
	}
}

func TestStoresAreWorkspaceBound(t *testing.T) {
	ctx := context.Background()
	id := rag.EmbedderIdentity{ID: "fake", Dim: 2}
	a := mustStore(t, "xinghe", id, nil)
	b := mustStore(t, "yunsong", id, nil)
	if err := a.Upsert(ctx, []rag.Chunk{{ID: "c", DocID: "d", Text: "星禾", Vector: []float32{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	hits, err := b.Search(ctx, []float32{1, 0}, 3)
	if err != nil || len(hits) != 0 {
		t.Fatalf("另一个工作区看到了命中: %+v %v", hits, err)
	}
	if a.WorkspaceID() != "xinghe" || b.WorkspaceID() != "yunsong" {
		t.Fatal("工作区绑定错误")
	}
}

func TestNormalizeFalseKeepsRawDot(t *testing.T) {
	ctx := context.Background()
	meta := &rag.IndexMeta{EmbedderID: "fake", Dim: 2, Normalize: false}
	store, err := rag.NewFakeStore("ws", meta.Identity(), meta, []rag.Chunk{{
		ID: "c", DocID: "d", Vector: []float32{1, 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := store.Search(ctx, []float32{1, 2}, 1)
	if err != nil || len(hits) != 1 || hits[0].Score != 5 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
}

func TestL2NormalizeAndDot(t *testing.T) {
	got, err := rag.L2Normalize([]float32{3, 0})
	if err != nil || got[0] != 1 || got[1] != 0 {
		t.Fatalf("normalize = %v %v", got, err)
	}
	if _, err := rag.L2Normalize([]float32{0, 0}); !errors.Is(err, rag.ErrZeroVector) {
		t.Fatal(err)
	}
	if _, err := rag.L2Normalize([]float32{float32(math.NaN())}); !errors.Is(err, rag.ErrInvalidArgument) {
		t.Fatal(err)
	}
	score, err := rag.Dot([]float32{1, 0}, []float32{0, 1})
	if err != nil || score != 0 {
		t.Fatalf("dot = %v %v", score, err)
	}
}

func TestEmptyWorkspaceRejected(t *testing.T) {
	_, err := rag.NewFakeStore("", rag.EmbedderIdentity{ID: "fake", Dim: 2}, nil, nil)
	if !errors.Is(err, rag.ErrInvalidArgument) {
		t.Fatal(err)
	}
}

func mustStore(t *testing.T, ws string, id rag.EmbedderIdentity, meta *rag.IndexMeta) *rag.FakeStore {
	t.Helper()
	store, err := rag.NewFakeStore(ws, id, meta, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func mustNorm(t *testing.T, v []float32) []float32 {
	t.Helper()
	out, err := rag.L2Normalize(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertMismatch(t *testing.T, err error, have, want rag.EmbedderIdentity) {
	t.Helper()
	if !errors.Is(err, rag.ErrEmbedderMismatch) {
		t.Fatalf("errors.Is(ErrEmbedderMismatch) = false, err=%v", err)
	}
	var mismatch *rag.EmbedderMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("errors.As 失败: %v", err)
	}
	if mismatch.Have != have || mismatch.Want != want {
		t.Fatalf("Have/Want = %+v / %+v", mismatch.Have, mismatch.Want)
	}
	if mismatch.Code() != errcode.EmbeddingMismatch {
		t.Fatalf("Code = %q", mismatch.Code())
	}
	got, ok := errcode.CodeOf(err)
	if !ok || got != errcode.EmbeddingMismatch {
		t.Fatalf("CodeOf = %q %v", got, ok)
	}
}

func vecClose(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(float64(a[i]-b[i])) > 1e-5 {
			return false
		}
	}
	return true
}
