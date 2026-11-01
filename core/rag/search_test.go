package rag_test

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/core/errcode"
	"github.com/loveRyujin/lantern/core/rag"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

const (
	gmvBody = "# 手册\n\n引言\n\n## GMV\n\n已支付订单后来发生退款时仍计入\n"
	gmvText = "已支付订单后来发生退款时仍计入"
	gmvID   = "manual#structural#1"
)

func TestSearchDocsHitMetadataAndStats(t *testing.T) {
	ctx := t.Context()
	emb := mustFake(t, "fake:deterministic", 8)
	h := mustWorkspace(t, "", "xinghe")
	importManual(t, h, emb, "manual", gmvBody)
	store := openStore(t, h, rag.IdentityOf(emb))
	tool := mustSearch(t, emb, store)

	reg := &tools.Registry{}
	if err := reg.Register(tool); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Prepare(tools.CallInput{Name: "search_docs", Arguments: `{"query":"q","k":21}`}); err == nil {
		t.Fatal("k 超过 MaxSearchK 应被 Schema 拒绝")
	}
	if _, err := reg.Prepare(tools.CallInput{Name: "search_docs", Arguments: `{"query":"","k":1}`}); err == nil {
		t.Fatal("空 query 应被 Schema 拒绝")
	}
	prepared, err := reg.Prepare(tools.CallInput{Name: "search_docs", Arguments: `{"query":` + jsonString(gmvText) + `}`})
	if err != nil {
		t.Fatal(err)
	}

	result, err := tool.Execute(ctx, tools.Call{Name: prepared.Name, Args: prepared.Args, WorkspaceID: "别的工作区"})
	if err != nil {
		t.Fatal(err)
	}
	payload := decodeSearch(t, result.Content)
	if payload.Status != rag.SearchStatusOK || payload.Message != "" || payload.K != rag.DefaultSearchK {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Threshold != float64(rag.DefaultEvidenceThreshold) || payload.Stats.Chunks != 2 || payload.Stats.DurationNS <= 0 {
		t.Fatalf("stats/threshold = %+v", payload)
	}
	if len(payload.Hits) != 2 {
		t.Fatalf("hits = %+v", payload.Hits)
	}
	hit := payload.Hits[0]
	if hit.Score != 1 || hit.ChunkID != gmvID || hit.DocID != "manual" || hit.Text != gmvText || hit.TitlePath != "手册 / GMV" || hit.Strategy != rag.StrategyStructural {
		t.Fatalf("top hit = %+v", hit)
	}
	if store.LastSearchStats().Chunks != payload.Stats.Chunks {
		t.Fatalf("LastSearchStats = %+v", store.LastSearchStats())
	}
	if strings.Contains(result.Content, "别的工作区") {
		t.Fatal("结果里出现了调用参数中的 workspace")
	}

	limited, err := tool.Execute(ctx, tools.Call{Args: []byte(`{"query":` + jsonString(gmvText) + `,"k":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	one := decodeSearch(t, limited.Content)
	if one.K != 1 || len(one.Hits) != 1 || one.Hits[0].ChunkID != gmvID {
		t.Fatalf("top-k = %+v", one)
	}
}

func TestSearchDocsEmptyAndLowScoreRefuse(t *testing.T) {
	ctx := t.Context()
	emb := mustFake(t, "fake:deterministic", 8)
	emptyWS := mustWorkspace(t, "", "empty")
	emptyStore := openStore(t, emptyWS, rag.IdentityOf(emb))
	emptyTool := mustSearch(t, emb, emptyStore)
	empty, err := emptyTool.Execute(ctx, tools.Call{Args: []byte(`{"query":"有没有口径"}`)})
	if err != nil {
		t.Fatal(err)
	}
	payload := decodeSearch(t, empty.Content)
	if payload.Status != rag.SearchStatusNoEvidence || payload.Message != rag.NoEvidenceMessage || payload.MaxScore != nil || len(payload.Hits) != 0 || payload.Stats.Chunks != 0 {
		t.Fatalf("空库 = %+v", payload)
	}

	h := mustWorkspace(t, "", "xinghe")
	importManual(t, h, emb, "manual", gmvBody)
	store := openStore(t, h, rag.IdentityOf(emb))
	above := math.Nextafter32(1, 2)
	tool := mustSearch(t, emb, store, rag.WithEvidenceThreshold(above))
	low, err := tool.Execute(ctx, tools.Call{Args: []byte(`{"query":` + jsonString(gmvText) + `}`)})
	if err != nil {
		t.Fatal(err)
	}
	payload = decodeSearch(t, low.Content)
	if payload.Status != rag.SearchStatusNoEvidence || payload.Message != rag.NoEvidenceMessage || len(payload.Hits) != 0 {
		t.Fatalf("低分 = %+v", payload)
	}
	if payload.MaxScore == nil || *payload.MaxScore != 1 || strings.Contains(low.Content, gmvID) || strings.Contains(low.Content, "手册 / GMV") {
		t.Fatalf("低分片段泄漏: %s", low.Content)
	}
	if _, noEvidence, ok := rag.EvidenceFromSearch(low.Content); !ok || !noEvidence {
		t.Fatal("低分结果应记为无依据")
	}

	equal := mustSearch(t, emb, store, rag.WithEvidenceThreshold(1))
	okResult, err := equal.Execute(ctx, tools.Call{Args: []byte(`{"query":` + jsonString(gmvText) + `}`)})
	if err != nil {
		t.Fatal(err)
	}
	payload = decodeSearch(t, okResult.Content)
	if payload.Status != rag.SearchStatusOK || len(payload.Hits) == 0 || payload.Hits[0].ChunkID != gmvID {
		t.Fatalf("等于阈值应保留依据: %+v", payload)
	}
}

func TestSearchDocsWorkspaceIsolation(t *testing.T) {
	ctx := t.Context()
	emb := mustFake(t, "fake:deterministic", 8)
	root := t.TempDir()
	xinghe := mustWorkspace(t, root, "xinghe")
	yunsong := mustWorkspace(t, root, "yunsong")
	importManual(t, xinghe, emb, "xinghe-manual", "# 星禾手册\n\n## GMV\n\n星禾独有口径：已支付订单金额在退款后仍计入GMV。\n")
	importManual(t, yunsong, emb, "yunsong-manual", "# 云松手册\n\n## 退货率\n\n云松独有口径：退货单数除以支付单数得到退货率。\n")

	xingheTool := mustSearch(t, emb, openStore(t, xinghe, rag.IdentityOf(emb)))
	yunsongTool := mustSearch(t, emb, openStore(t, yunsong, rag.IdentityOf(emb)))
	xingheHit, err := xingheTool.Execute(ctx, tools.Call{Args: []byte(`{"query":"星禾独有口径：已支付订单金额在退款后仍计入GMV。"}`)})
	if err != nil {
		t.Fatal(err)
	}
	yunsongHit, err := yunsongTool.Execute(ctx, tools.Call{Args: []byte(`{"query":"云松独有口径：退货单数除以支付单数得到退货率。"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(xingheHit.Content, "星禾独有口径") || strings.Contains(xingheHit.Content, "云松独有口径") {
		t.Fatalf("星禾结果 = %s", xingheHit.Content)
	}
	if !strings.Contains(yunsongHit.Content, "云松独有口径") || strings.Contains(yunsongHit.Content, "星禾独有口径") {
		t.Fatalf("云松结果 = %s", yunsongHit.Content)
	}
	crossed, err := yunsongTool.Execute(ctx, tools.Call{Args: []byte(`{"query":"星禾独有口径：已支付订单金额在退款后仍计入GMV。"}`)})
	if err != nil {
		t.Fatal(err)
	}
	crossedPayload := decodeSearch(t, crossed.Content)
	for _, hit := range crossedPayload.Hits {
		if strings.Contains(hit.Text, "星禾独有口径") || strings.Contains(hit.ChunkID, "xinghe-manual") || hit.DocID == "xinghe-manual" {
			t.Fatalf("云松库返回了星禾片段: %+v", hit)
		}
	}
	if strings.Contains(crossed.Content, "xinghe-manual") {
		t.Fatalf("云松结果里出现了星禾文档: %s", crossed.Content)
	}
	if n := countWhere(t, yunsong, `SELECT COUNT(*) FROM chunks WHERE text LIKE ?`, "%星禾独有口径%"); n != 0 {
		t.Fatalf("云松 kb.db 里有星禾片段 %d 条", n)
	}
	if n := countWhere(t, xinghe, `SELECT COUNT(*) FROM chunks WHERE text LIKE ?`, "%云松独有口径%"); n != 0 {
		t.Fatalf("星禾 kb.db 里有云松片段 %d 条", n)
	}
}

func TestSearchDocsEmbedderUnavailableIsNotEmptyHit(t *testing.T) {
	ctx := t.Context()
	emb := mustFake(t, "fake:deterministic", 8)
	h := mustWorkspace(t, "", "xinghe")
	importManual(t, h, emb, "manual", gmvBody)
	store := openStore(t, h, rag.IdentityOf(emb))
	down := &countingEmbedder{Embedder: emb, err: &rag.EmbedderUnavailableError{
		Reason:   rag.EmbedderUnreachable,
		Model:    "bge-m3",
		Guidance: "本机没有连上 Ollama。请先启动 Ollama，并执行 ollama pull bge-m3。",
	}}
	tool := mustSearch(t, down, store)
	result, err := tool.Execute(ctx, tools.Call{Args: []byte(`{"query":` + jsonString(gmvText) + `}`)})
	if result.Content != "" || result.Summary != "" {
		t.Fatalf("失败却返回了正文: %+v", result)
	}
	if code, ok := tools.CodeOf(err); !ok || code != string(errcode.EmbeddingUnavailable) {
		t.Fatalf("工具码 = %v err=%v", code, err)
	}
	if got, ok := errcode.CodeOf(err); !ok || got != errcode.EmbeddingUnavailable {
		t.Fatalf("稳定码 = %v ok=%v", got, ok)
	}
	if !errors.Is(err, rag.ErrEmbedderUnavailable) || !strings.Contains(err.Error(), "ollama pull bge-m3") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), gmvText) || strings.Contains(err.Error(), `"hits"`) {
		t.Fatalf("错误被包装成命中: %v", err)
	}
	if down.calls != 1 || store.LastSearchStats().Chunks != 0 {
		t.Fatalf("calls=%d stats=%+v，嵌入失败后仍检索了库", down.calls, store.LastSearchStats())
	}

	other := mustFake(t, "fake:other", 8)
	counted := &countingEmbedder{Embedder: other}
	mismatch := mustSearch(t, counted, openStore(t, h, rag.IdentityOf(other)))
	_, err = mismatch.Execute(ctx, tools.Call{Args: []byte(`{"query":"口径"}`)})
	if code, _ := tools.CodeOf(err); code != string(errcode.EmbeddingMismatch) || !errors.Is(err, rag.ErrEmbedderMismatch) {
		t.Fatalf("mismatch = %v", err)
	}
	if counted.calls != 0 {
		t.Fatalf("模型不一致仍去嵌入了 %d 次", counted.calls)
	}
}

func TestEvidenceFromSearchIgnoresUncitedHits(t *testing.T) {
	ids, noEvidence, ok := rag.EvidenceFromSearch(`{"status":"no_evidence","hits":[{"chunk_id":"secret"}]}`)
	if !ok || !noEvidence || ids != nil {
		t.Fatalf("ids=%v no=%v ok=%v", ids, noEvidence, ok)
	}
	ids, noEvidence, ok = rag.EvidenceFromSearch(`{"status":"ok","hits":[{"chunk_id":"a"},{"chunk_id":"b"}]}`)
	if !ok || noEvidence || len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("ids=%v no=%v ok=%v", ids, noEvidence, ok)
	}
	if _, _, ok = rag.EvidenceFromSearch("not-json"); ok {
		t.Fatal("非 JSON 不应记为检索结果")
	}
}

func importManual(t *testing.T, h *workspace.Handle, emb *rag.FakeEmbedder, id, body string) {
	t.Helper()
	_, err := rag.ImportKB(t.Context(), h.KB, emb, rag.Document{
		ID: id, Title: id, Body: body, Format: rag.FormatMarkdown,
	}, rag.WithStrategy(rag.StrategyStructural))
	if err != nil {
		t.Fatal(err)
	}
}

func jsonString(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

type searchView struct {
	Status    string   `json:"status"`
	Message   string   `json:"message"`
	Threshold float64  `json:"threshold"`
	MaxScore  *float64 `json:"max_score"`
	K         int      `json:"k"`
	Hits      []struct {
		ChunkID   string  `json:"chunk_id"`
		DocID     string  `json:"doc_id"`
		Text      string  `json:"text"`
		TitlePath string  `json:"title_path"`
		Strategy  string  `json:"strategy"`
		Score     float64 `json:"score"`
	} `json:"hits"`
	Stats struct {
		DurationNS int64 `json:"duration_ns"`
		Chunks     int   `json:"chunks"`
	} `json:"stats"`
}

func decodeSearch(t *testing.T, content string) searchView {
	t.Helper()
	var payload searchView
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		t.Fatalf("解析检索结果: %v body=%s", err, content)
	}
	return payload
}

func mustSearch(t *testing.T, emb rag.Embedder, store rag.VectorStore, opts ...rag.SearchOption) *rag.SearchTool {
	t.Helper()
	tool, err := rag.NewSearchTool(emb, store, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Spec().Name != "search_docs" {
		t.Fatalf("spec = %s", tool.Spec().Name)
	}
	return tool
}
