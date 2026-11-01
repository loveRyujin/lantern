package rag_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/errcode"
	"github.com/loveRyujin/lantern/core/rag"
)

func TestOllamaEmbedderDefaults(t *testing.T) {
	emb, err := rag.NewOllamaEmbedder()
	if err != nil {
		t.Fatal(err)
	}
	if emb.ID() != rag.DefaultOllamaID || emb.ID() != "ollama:bge-m3" || emb.Dim() != rag.DefaultOllamaDim || emb.Dim() != 1024 {
		t.Fatalf("身份 = %s/%d", emb.ID(), emb.Dim())
	}
	if _, err := emb.Embed(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := rag.NewOllamaEmbedder(rag.WithOllamaDim(0)); err == nil {
		t.Fatal("维度 0 应该被拒绝")
	}
	if _, err := rag.NewOllamaEmbedder(rag.WithOllamaBaseURL("ftp://127.0.0.1:11434")); err == nil {
		t.Fatal("非 http 地址应该被拒绝")
	}
	if _, err := rag.NewOllamaEmbedder(rag.WithOllamaBaseURL("http://127.0.0.1:11434/v1")); err == nil {
		t.Fatal("带路径的地址应该被拒绝")
	}
	prefixed, err := rag.NewOllamaEmbedder(rag.WithOllamaModel("ollama:bge-m3"))
	if err != nil {
		t.Fatal(err)
	}
	if prefixed.ID() != "ollama:bge-m3" {
		t.Fatalf("前缀被重复加上: %s", prefixed.ID())
	}
}

func TestOllamaEmbedderNormalizesBatch(t *testing.T) {
	var gotModel string
	var gotInput []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/embed" {
			t.Errorf("请求 = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %s", r.Header.Get("Content-Type"))
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		gotModel = req.Model
		gotInput = append([]string(nil), req.Input...)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": [][]float64{{3, 0}, {0, 4}},
		})
	}))
	t.Cleanup(srv.Close)

	emb, err := rag.NewOllamaEmbedder(
		rag.WithOllamaBaseURL(srv.URL+"/"),
		rag.WithOllamaModel("ollama:bge-m3"),
		rag.WithOllamaDim(2),
	)
	if err != nil {
		t.Fatal(err)
	}
	vecs, err := emb.Embed(context.Background(), []string{"甲", "乙"})
	if err != nil {
		t.Fatal(err)
	}
	if gotModel != "bge-m3" || len(gotInput) != 2 || gotInput[0] != "甲" || gotInput[1] != "乙" {
		t.Fatalf("请求模型/输入 = %s %#v", gotModel, gotInput)
	}
	if len(vecs) != 2 || !vecClose(vecs[0], []float32{1, 0}) || !vecClose(vecs[1], []float32{0, 1}) {
		t.Fatalf("归一化结果 = %#v", vecs)
	}
	for _, vec := range vecs {
		if math.Abs(float64(norm2(vec))-1) > 1e-5 {
			t.Fatalf("范数 = %v", norm2(vec))
		}
	}
}

func TestOllamaEmbedderRejectsWrongDimension(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": [][]float64{{1, 0, 0, 0}},
		})
	}))
	t.Cleanup(srv.Close)

	emb, err := rag.NewOllamaEmbedder(rag.WithOllamaBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if emb.Dim() != 1024 || emb.ID() != "ollama:bge-m3" {
		t.Fatalf("默认身份 = %s/%d", emb.ID(), emb.Dim())
	}
	vecs, err := emb.Embed(context.Background(), []string{"口径"})
	if vecs != nil {
		t.Fatalf("维度不符仍返回了向量: %#v", vecs)
	}
	var dimErr *rag.DimensionError
	if !errors.As(err, &dimErr) || dimErr.Got != 4 || dimErr.Want != 1024 {
		t.Fatalf("err=%v", err)
	}
}

func TestOllamaEmbedderUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("关闭后的服务不应该被调用成功")
	}))
	endpoint := srv.URL
	srv.Close()

	emb, err := rag.NewOllamaEmbedder(rag.WithOllamaBaseURL(endpoint))
	if err != nil {
		t.Fatal(err)
	}
	vecs, err := emb.Embed(context.Background(), []string{"口径"})
	if vecs != nil {
		t.Fatalf("不可达仍返回了向量: %#v", vecs)
	}
	assertUnavailable(t, err, rag.EmbedderUnreachable)
	if !strings.Contains(err.Error(), "安装") || !strings.Contains(err.Error(), "127.0.0.1:11434") {
		t.Fatalf("引导文案 = %v", err)
	}
}

func TestOllamaEmbedderModelMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"model \"bge-m3\" not found, try pulling it first"}`)
	}))
	t.Cleanup(srv.Close)

	emb, err := rag.NewOllamaEmbedder(rag.WithOllamaBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	vecs, err := emb.Embed(context.Background(), []string{"口径"})
	if vecs != nil {
		t.Fatalf("缺模型仍返回了向量: %#v", vecs)
	}
	assertUnavailable(t, err, rag.EmbedderModelMissing)
	if !strings.Contains(err.Error(), "ollama pull bge-m3") {
		t.Fatalf("引导文案 = %v", err)
	}
}

func TestOllamaEmbedderRejectsEmptyAndUpstream(t *testing.T) {
	var mode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "empty":
			_, _ = io.WriteString(w, `{"embeddings":[]}`)
		case "null":
			_, _ = io.WriteString(w, `{"embeddings":[null]}`)
		case "status":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
		case "zero":
			_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{{0, 0}}})
		case "redirect":
			http.Redirect(w, r, "http://example.com/steal", http.StatusFound)
		default:
			t.Errorf("未知模式 %s", mode)
		}
	}))
	t.Cleanup(srv.Close)
	emb, err := rag.NewOllamaEmbedder(rag.WithOllamaBaseURL(srv.URL), rag.WithOllamaDim(2))
	if err != nil {
		t.Fatal(err)
	}

	mode = "empty"
	vecs, err := emb.Embed(context.Background(), []string{"甲"})
	if vecs != nil {
		t.Fatal(vecs)
	}
	assertUnavailable(t, err, rag.EmbedderBadResponse)

	mode = "null"
	vecs, err = emb.Embed(context.Background(), []string{"甲"})
	if vecs != nil || err == nil {
		t.Fatalf("vecs=%v err=%v", vecs, err)
	}

	mode = "status"
	vecs, err = emb.Embed(context.Background(), []string{"甲"})
	if vecs != nil {
		t.Fatal(vecs)
	}
	assertUnavailable(t, err, rag.EmbedderUpstream)

	mode = "zero"
	vecs, err = emb.Embed(context.Background(), []string{"甲"})
	if vecs != nil || !errors.Is(err, rag.ErrZeroVector) {
		t.Fatalf("零向量 vecs=%v err=%v", vecs, err)
	}

	mode = "redirect"
	vecs, err = emb.Embed(context.Background(), []string{"甲"})
	if vecs != nil {
		t.Fatal(vecs)
	}
	assertUnavailable(t, err, rag.EmbedderUpstream)
	if strings.Contains(err.Error(), "example.com") && strings.Contains(err.Error(), "steal-body") {
		t.Fatal("不应该跟随重定向去读取外部正文")
	}
}

func TestOllamaEmbedderCanceled(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	t.Cleanup(srv.Close)
	emb, err := rag.NewOllamaEmbedder(rag.WithOllamaBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	vecs, err := emb.Embed(ctx, []string{"甲"})
	if vecs != nil || !errors.Is(err, context.Canceled) || errors.Is(err, rag.ErrEmbedderUnavailable) {
		t.Fatalf("vecs=%v err=%v", vecs, err)
	}
	if called {
		t.Fatal("已取消的请求不应该访问 Ollama")
	}
	var nilEmb *rag.OllamaEmbedder
	if _, err := nilEmb.Embed(context.Background(), []string{"甲"}); err == nil {
		t.Fatal("nil Embedder 应该返回错误")
	}
}

func TestOllamaLiveEmbed(t *testing.T) {
	if os.Getenv("LANTERN_OLLAMA") != "1" {
		t.Skip("LANTERN_OLLAMA=1 才连接本机 Ollama；默认 CI 不依赖它")
	}
	emb, err := rag.NewOllamaEmbedder()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	vecs, err := emb.Embed(ctx, []string{"口径说明"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 1 || len(vecs[0]) != 1024 {
		t.Fatalf("live 向量条数 = %d", len(vecs))
	}
	if math.Abs(float64(norm2(vecs[0]))-1) > 1e-4 {
		t.Fatalf("live 范数 = %v", norm2(vecs[0]))
	}
}

func assertUnavailable(t *testing.T, err error, reason string) {
	t.Helper()
	if !errors.Is(err, rag.ErrEmbedderUnavailable) {
		t.Fatalf("errors.Is(ErrEmbedderUnavailable) = false, err=%v", err)
	}
	var unavailable *rag.EmbedderUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("errors.As 失败: %v", err)
	}
	if unavailable.Reason != reason || unavailable.Model != "bge-m3" {
		t.Fatalf("Reason/Model = %s / %s", unavailable.Reason, unavailable.Model)
	}
	if unavailable.Code() != errcode.EmbeddingUnavailable {
		t.Fatalf("Code = %s", unavailable.Code())
	}
	got, ok := errcode.CodeOf(err)
	if !ok || got != errcode.EmbeddingUnavailable {
		t.Fatalf("CodeOf = %q %v", got, ok)
	}
	if unavailable.Guidance == "" {
		t.Fatal("缺少引导文案")
	}
}

func norm2(v []float32) float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return float32(math.Sqrt(sum))
}
