package rag_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/core/errcode"
	"github.com/loveRyujin/lantern/core/rag"
	"github.com/loveRyujin/lantern/core/workspace"
)

func TestImportFixedAndStructural(t *testing.T) {
	ctx := t.Context()
	emb := mustFake(t, "fake:deterministic", 8)
	store := mustMemory(t, emb)
	doc := rag.Document{
		ID:     "manual",
		Title:  "手册",
		Source: "mem",
		Body:   "# 手册\n\n引言\n\n## GMV\n\n已支付订单后来发生退款时仍计入\n",
	}
	structural, err := rag.Import(ctx, store, emb, doc, rag.WithStrategy(rag.StrategyStructural))
	if err != nil {
		t.Fatal(err)
	}
	if structural.Skipped || structural.Chunks != 2 || structural.Strategy != rag.StrategyStructural {
		t.Fatalf("structural = %+v", structural)
	}
	hits := searchText(t, ctx, store, emb, "已支付订单后来发生退款时仍计入")
	if len(hits) != 1 || hits[0].TitlePath != "手册 / GMV" || hits[0].Strategy != rag.StrategyStructural || hits[0].DocID != "manual" {
		t.Fatalf("命中 = %+v", hits)
	}
	if hits[0].ChunkID != "manual#structural#1" {
		t.Fatalf("chunk id = %s", hits[0].ChunkID)
	}

	fixedStore := mustMemory(t, emb)
	fixedDoc := doc
	fixedDoc.ID = "fixed-doc"
	fixedDoc.Body = "abcdefghijklmnopqrstuvwxyz"
	fixed, err := rag.Import(ctx, fixedStore, emb, fixedDoc, rag.WithStrategy(rag.StrategyFixed), rag.WithFixedRunes(8, 2))
	if err != nil {
		t.Fatal(err)
	}
	if fixed.Chunks < 2 {
		t.Fatalf("fixed = %+v", fixed)
	}
	hits = searchText(t, ctx, fixedStore, emb, "abcdefgh")
	if len(hits) != 1 || hits[0].Text != "abcdefgh" || hits[0].Strategy != rag.StrategyFixed || hits[0].TitlePath != "" || hits[0].DocID != "fixed-doc" {
		t.Fatalf("fixed 命中 = %+v", hits)
	}
}

func TestImportReplacesWithoutOrphans(t *testing.T) {
	ctx := t.Context()
	emb := mustFake(t, "fake:deterministic", 8)
	h := mustWorkspace(t, "", "xinghe")
	other := mustWorkspace(t, "", "yunsong")
	first := rag.Document{ID: "manual", Title: "第一版", Source: "v1", Body: "# 手册\n\n## 甲\n\n甲的正文\n\n## 乙\n\n乙的正文\n"}
	res, err := rag.ImportKB(ctx, h.KB, emb, first, rag.WithStrategy(rag.StrategyStructural))
	if err != nil || res.Chunks != 2 {
		t.Fatalf("首次 = %+v err=%v", res, err)
	}
	created := docColumn(t, h, "manual", "created_at")
	second := first
	second.Title = "第二版"
	second.Source = "v2"
	second.Body = "# 手册\n\n## 丙\n\n丙的正文\n"
	res, err = rag.ImportKB(ctx, h.KB, emb, second, rag.WithStrategy(rag.StrategyStructural))
	if err != nil || res.Chunks != 1 || res.Skipped {
		t.Fatalf("再次 = %+v err=%v", res, err)
	}
	if n := countWhere(t, h, `SELECT COUNT(*) FROM chunks WHERE document_id = ?`, "manual"); n != 1 {
		t.Fatalf("manual 片段 = %d", n)
	}
	if n := countWhere(t, h, `SELECT COUNT(*) FROM chunks WHERE text LIKE ?`, "%甲的正文%"); n != 0 {
		t.Fatalf("旧片段还在: %d", n)
	}
	if n := countWhere(t, h, `SELECT COUNT(*) FROM vectors v LEFT JOIN chunks c ON c.id = v.chunk_id WHERE c.id IS NULL`); n != 0 {
		t.Fatalf("孤儿向量 = %d", n)
	}
	var title, source, body, createdAgain string
	if err := h.KB.QueryRowContext(ctx, `SELECT title, source, body, created_at FROM documents WHERE id = ?`, "manual").
		Scan(&title, &source, &body, &createdAgain); err != nil {
		t.Fatal(err)
	}
	if title != "第二版" || source != "v2" || body != second.Body || createdAgain != created {
		t.Fatalf("文档行 title=%s source=%s created=%s/%s body=%q", title, source, created, createdAgain, body)
	}
	var path, strategy string
	if err := h.KB.QueryRowContext(ctx, `SELECT title_path, strategy FROM chunks WHERE document_id = ?`, "manual").
		Scan(&path, &strategy); err != nil {
		t.Fatal(err)
	}
	if path != "手册 / 丙" || strategy != rag.StrategyStructural {
		t.Fatalf("path=%s strategy=%s", path, strategy)
	}

	kept, err := rag.ImportKB(ctx, other.KB, emb, first, rag.WithStrategy(rag.StrategyStructural))
	if err != nil || kept.Chunks != 2 {
		t.Fatalf("另一个工作区 = %+v err=%v", kept, err)
	}
	if n := countWhere(t, h, `SELECT COUNT(*) FROM chunks`); n != 1 {
		t.Fatalf("星禾被另一个工作区影响: %d", n)
	}
	if n := countWhere(t, other, `SELECT COUNT(*) FROM chunks WHERE text LIKE ?`, "%丙的正文%"); n != 0 {
		t.Fatalf("云杉看到了星禾的新正文: %d", n)
	}
}

func TestImportSkipsBlankWithoutTouchingIndex(t *testing.T) {
	ctx := t.Context()
	emb := mustFake(t, "fake:deterministic", 4)
	counter := &countingEmbedder{Embedder: emb}
	h := mustWorkspace(t, "", "xinghe")
	doc := rag.Document{ID: "manual", Title: "手册", Body: "有内容"}
	if _, err := rag.ImportKB(ctx, h.KB, counter, doc, rag.WithStrategy(rag.StrategyFixed)); err != nil {
		t.Fatal(err)
	}
	before := countWhere(t, h, `SELECT COUNT(*) FROM chunks`)
	calls := counter.calls
	for _, body := range []string{"", " \n\t", "# 只有标题\n"} {
		counter.calls = 0
		res, err := rag.ImportKB(ctx, h.KB, counter, rag.Document{ID: "manual", Body: body}, rag.WithStrategy(rag.StrategyStructural))
		if err != nil {
			t.Fatal(err)
		}
		if !res.Skipped || res.Chunks != 0 || counter.calls != 0 {
			t.Fatalf("body %q result=%+v calls=%d", body, res, counter.calls)
		}
	}
	if countWhere(t, h, `SELECT COUNT(*) FROM chunks`) != before || counter.calls != 0 {
		t.Fatal("空白导入改动了索引或调用了嵌入")
	}
	if calls != 1 {
		t.Fatalf("首次嵌入次数 = %d", calls)
	}
	store := mustMemory(t, emb)
	res, err := rag.Import(ctx, store, counter, rag.Document{ID: "empty", Body: "   "}, rag.WithStrategy(rag.StrategyFixed))
	if err != nil || !res.Skipped || store.Count() != 0 {
		t.Fatalf("内存库空白 = %+v count=%d err=%v", res, store.Count(), err)
	}
	if _, ok := store.Index(); ok {
		t.Fatal("跳过不应创建 index_meta")
	}
}

func TestImportEmbedderFailureWritesNothing(t *testing.T) {
	ctx := t.Context()
	emb := mustFake(t, "fake:deterministic", 8)
	h := mustWorkspace(t, "", "xinghe")
	doc := rag.Document{ID: "manual", Title: "手册", Body: "旧正文留在库里"}
	if _, err := rag.ImportKB(ctx, h.KB, emb, doc, rag.WithStrategy(rag.StrategyFixed)); err != nil {
		t.Fatal(err)
	}
	down := &countingEmbedder{
		Embedder: emb,
		err: &rag.EmbedderUnavailableError{
			Reason:   rag.EmbedderUnreachable,
			Model:    "bge-m3",
			Guidance: "请安装并启动 Ollama",
		},
	}
	_, err := rag.ImportKB(ctx, h.KB, down, rag.Document{ID: "manual", Body: "新正文不应落库"}, rag.WithStrategy(rag.StrategyFixed))
	if !errors.Is(err, rag.ErrEmbedderUnavailable) {
		t.Fatalf("err = %v", err)
	}
	code, ok := errcode.CodeOf(err)
	if !ok || code != errcode.EmbeddingUnavailable || !strings.Contains(err.Error(), "请安装并启动 Ollama") {
		t.Fatalf("code=%s ok=%v err=%v", code, ok, err)
	}
	if down.calls != 1 {
		t.Fatalf("嵌入次数 = %d", down.calls)
	}
	if n := countWhere(t, h, `SELECT COUNT(*) FROM chunks WHERE text = ?`, "旧正文留在库里"); n != 1 {
		t.Fatalf("旧片段 = %d", n)
	}
	if n := countWhere(t, h, `SELECT COUNT(*) FROM chunks WHERE text = ?`, "新正文不应落库"); n != 0 {
		t.Fatalf("失败的导入写入了新正文: %d", n)
	}

	fresh := mustWorkspace(t, "", "empty")
	down.calls = 0
	_, err = rag.ImportKB(ctx, fresh.KB, down, rag.Document{ID: "new", Body: "不会写入"}, rag.WithStrategy(rag.StrategyFixed))
	if !errors.Is(err, rag.ErrEmbedderUnavailable) {
		t.Fatal(err)
	}
	for _, table := range []string{"documents", "chunks", "vectors", "index_meta"} {
		if n := countWhere(t, fresh, `SELECT COUNT(*) FROM `+table); n != 0 {
			t.Fatalf("%s = %d", table, n)
		}
	}

	bad := &countingEmbedder{Embedder: emb, vectors: [][]float32{{1}}}
	_, err = rag.ImportKB(ctx, h.KB, bad, rag.Document{ID: "manual", Body: "维度不对"}, rag.WithStrategy(rag.StrategyFixed))
	var dimErr *rag.DimensionError
	if !errors.As(err, &dimErr) || dimErr.Want != emb.Dim() {
		t.Fatalf("维度错误 = %v", err)
	}
	if countWhere(t, h, `SELECT COUNT(*) FROM chunks WHERE text = ?`, "旧正文留在库里") != 1 {
		t.Fatal("维度不符时改写了旧片段")
	}
}

func TestImportMismatchDoesNotEmbed(t *testing.T) {
	ctx := t.Context()
	emb := mustFake(t, "ollama:bge-m3", 8)
	h := mustWorkspace(t, "", "xinghe")
	if _, err := rag.ImportKB(ctx, h.KB, emb, rag.Document{ID: "manual", Body: "已有索引"}, rag.WithStrategy(rag.StrategyFixed)); err != nil {
		t.Fatal(err)
	}
	other := &countingEmbedder{Embedder: mustFake(t, "openai:text-embedding-3-small", 8)}
	_, err := rag.ImportKB(ctx, h.KB, other, rag.Document{ID: "manual", Body: "另一套模型"}, rag.WithStrategy(rag.StrategyFixed))
	if !errors.Is(err, rag.ErrEmbedderMismatch) {
		t.Fatalf("err = %v", err)
	}
	if other.calls != 0 {
		t.Fatalf("模型不一致仍去嵌入: %d", other.calls)
	}
	if countWhere(t, h, `SELECT COUNT(*) FROM chunks WHERE text = ?`, "已有索引") != 1 {
		t.Fatal("不一致的导入改了旧片段")
	}
	code, ok := errcode.CodeOf(err)
	if !ok || code != errcode.EmbeddingMismatch {
		t.Fatalf("code=%s err=%v", code, err)
	}
}

func TestImportReplaceRollsBack(t *testing.T) {
	ctx := t.Context()
	emb := mustFake(t, "fake:deterministic", 8)
	h := mustWorkspace(t, "", "xinghe")
	doc := rag.Document{ID: "manual", Title: "第一版", Source: "v1", Body: "第一版正文"}
	if _, err := rag.ImportKB(ctx, h.KB, emb, doc, rag.WithStrategy(rag.StrategyFixed)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.KB.ExecContext(ctx, `
		CREATE TRIGGER rag_import_fail BEFORE INSERT ON chunks
		BEGIN
			SELECT RAISE(ABORT, 'boom');
		END`); err != nil {
		t.Fatal(err)
	}
	_, err := rag.ImportKB(ctx, h.KB, emb, rag.Document{
		ID: "manual", Title: "第二版", Source: "v2", Body: "第二版正文",
	}, rag.WithStrategy(rag.StrategyFixed))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if countWhere(t, h, `SELECT COUNT(*) FROM chunks WHERE text = ?`, "第一版正文") != 1 {
		t.Fatal("回滚后旧片段不在了")
	}
	if countWhere(t, h, `SELECT COUNT(*) FROM chunks WHERE text = ?`, "第二版正文") != 0 {
		t.Fatal("失败的替换留下了新片段")
	}
	if countWhere(t, h, `SELECT COUNT(*) FROM vectors v LEFT JOIN chunks c ON c.id = v.chunk_id WHERE c.id IS NULL`) != 0 {
		t.Fatal("回滚后留下孤儿向量")
	}
	if docColumn(t, h, "manual", "body") != "第一版正文" || docColumn(t, h, "manual", "title") != "第一版" {
		t.Fatal("回滚后文档正文被改掉了")
	}
	store := openStore(t, h, rag.IdentityOf(emb))
	hits, err := store.Search(ctx, mustVec(t, ctx, emb, "第一版正文"), 1)
	if err != nil || len(hits) != 1 || hits[0].Text != "第一版正文" {
		t.Fatalf("回滚后检索 = %+v err=%v", hits, err)
	}
}

func TestImportFileAndRejectPDF(t *testing.T) {
	ctx := t.Context()
	emb := mustFake(t, "fake:deterministic", 8)
	h := mustWorkspace(t, "", "xinghe")
	dir := t.TempDir()
	md := filepath.Join(dir, "指标口径.md")
	if err := os.WriteFile(md, []byte("# 手册\n\n## GMV\n\n定义在这里\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	txt := filepath.Join(dir, "备注.txt")
	if err := os.WriteFile(txt, []byte("纯文本没有标题\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := rag.ImportFile(ctx, h.KB, emb, md, "星禾/指标口径手册.md", rag.WithStrategy(rag.StrategyStructural))
	if err != nil || res.Chunks != 1 {
		t.Fatalf("md = %+v err=%v", res, err)
	}
	var title, formatBody string
	if err := h.KB.QueryRowContext(ctx, `SELECT title, body FROM documents WHERE id = ?`, "星禾/指标口径手册.md").Scan(&title, &formatBody); err != nil {
		t.Fatal(err)
	}
	if title != "指标口径" || !strings.Contains(formatBody, "定义在这里") {
		t.Fatalf("title=%s body=%s", title, formatBody)
	}
	var path string
	if err := h.KB.QueryRowContext(ctx, `SELECT title_path FROM chunks WHERE document_id = ?`, "星禾/指标口径手册.md").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != "手册 / GMV" {
		t.Fatalf("path = %s", path)
	}
	res, err = rag.ImportFile(ctx, h.KB, emb, txt, "note", rag.WithStrategy(rag.StrategyStructural))
	if err != nil || res.Chunks != 1 {
		t.Fatalf("txt = %+v err=%v", res, err)
	}
	if _, err := rag.ReadDocument(filepath.Join(dir, "扫描件.pdf"), "pdf"); !errors.Is(err, rag.ErrUnsupportedFormat) {
		t.Fatalf("pdf err = %v", err)
	}
	if _, err := rag.ReadDocument(filepath.Join(dir, "表.docx"), "docx"); !errors.Is(err, rag.ErrUnsupportedFormat) {
		t.Fatalf("docx err = %v", err)
	}
	bad := filepath.Join(dir, "坏.txt")
	if err := os.WriteFile(bad, []byte{0xff, 0xfe, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := rag.ReadDocument(bad, "bad"); err == nil {
		t.Fatal("非法 UTF-8 应该拒绝")
	}
	if countWhere(t, h, `SELECT COUNT(*) FROM documents WHERE id = ?`, "pdf") != 0 {
		t.Fatal("拒绝的格式写入了文档")
	}
}

func TestImportRejectsStoreWithoutReplace(t *testing.T) {
	emb := &countingEmbedder{Embedder: mustFake(t, "fake:deterministic", 4)}
	_, err := rag.Import(t.Context(), readonlyStore{}, emb, rag.Document{ID: "manual", Body: "有内容"}, rag.WithStrategy(rag.StrategyFixed))
	if err == nil || !errors.Is(err, rag.ErrInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
	if emb.calls != 0 {
		t.Fatalf("不能替换的库仍去嵌入: %d", emb.calls)
	}
}

type readonlyStore struct{}

func (readonlyStore) Upsert(context.Context, []rag.Chunk) error {
	return errors.New("不应写入")
}
func (readonlyStore) Delete(context.Context, string) error { return errors.New("不应删除") }
func (readonlyStore) Search(context.Context, []float32, int) ([]rag.Hit, error) {
	return nil, errors.New("不应检索")
}
func (readonlyStore) LastSearchStats() rag.SearchStats { return rag.SearchStats{} }

func TestReplaceDocumentRejectsForeignChunk(t *testing.T) {
	ctx := t.Context()
	emb := mustFake(t, "fake:deterministic", 2)
	store := mustMemory(t, emb)
	vec := mustVec(t, ctx, emb, "甲")
	if err := store.Upsert(ctx, []rag.Chunk{{ID: "a", DocID: "other", Text: "甲", Vector: vec}}); err != nil {
		t.Fatal(err)
	}
	err := store.ReplaceDocument(ctx, rag.Document{ID: "manual", Body: "新"}, []rag.Chunk{{
		ID: "a", DocID: "manual", Text: "新", Vector: vec,
	}})
	if err == nil {
		t.Fatal("占用别的文档的 chunk id 应该失败")
	}
	if store.Count() != 1 {
		t.Fatalf("失败的替换改了库: %d", store.Count())
	}
	hits, err := store.Search(ctx, vec, 1)
	if err != nil || len(hits) != 1 || hits[0].DocID != "other" || hits[0].Text != "甲" {
		t.Fatalf("hits = %+v err=%v", hits, err)
	}
}

type countingEmbedder struct {
	rag.Embedder
	calls   int
	err     error
	vectors [][]float32
}

func (e *countingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.calls++
	if e.err != nil {
		return nil, e.err
	}
	if e.vectors != nil {
		return e.vectors, nil
	}
	return e.Embedder.Embed(ctx, texts)
}

func mustFake(t *testing.T, id string, dim int) *rag.FakeEmbedder {
	t.Helper()
	emb, err := rag.NewFakeEmbedder(id, dim)
	if err != nil {
		t.Fatal(err)
	}
	return emb
}

func mustMemory(t *testing.T, emb rag.Embedder) *rag.FakeStore {
	t.Helper()
	store, err := rag.NewFakeStore("xinghe", rag.IdentityOf(emb), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func mustVec(t *testing.T, ctx context.Context, emb rag.Embedder, text string) []float32 {
	t.Helper()
	vecs, err := emb.Embed(ctx, []string{text})
	if err != nil || len(vecs) != 1 {
		t.Fatalf("embed %q: %v", text, err)
	}
	return vecs[0]
}

func searchText(t *testing.T, ctx context.Context, store *rag.FakeStore, emb rag.Embedder, text string) []rag.Hit {
	t.Helper()
	hits, err := store.Search(ctx, mustVec(t, ctx, emb, text), 1)
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func countWhere(t *testing.T, h *workspace.Handle, query string, args ...any) int {
	t.Helper()
	var n int
	if err := h.KB.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func docColumn(t *testing.T, h *workspace.Handle, id, column string) string {
	t.Helper()
	var query string
	switch column {
	case "created_at":
		query = `SELECT created_at FROM documents WHERE id = ?`
	case "body":
		query = `SELECT body FROM documents WHERE id = ?`
	case "title":
		query = `SELECT title FROM documents WHERE id = ?`
	default:
		t.Fatalf("未知列 %s", column)
	}
	var v string
	if err := h.KB.QueryRowContext(t.Context(), query, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}
