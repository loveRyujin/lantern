package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/audit"
	"github.com/loveRyujin/lantern/core/errcode"
	"github.com/loveRyujin/lantern/core/events"
	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/policy"
	"github.com/loveRyujin/lantern/core/rag"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

func TestCitationPassRewriteAndRefuse(t *testing.T) {
	tool, gmvID := importedSearch(t, nil)

	t.Run("命中后的引用通过", func(t *testing.T) {
		h, out, err := runSearch(t, tool,
			searchStep(gmvText),
			llm.Say("退款后仍计入。[[cite:"+gmvID+"]]"),
			llm.Say("不应再调用模型"),
		)
		if err != nil || out.State != StateCompleted || out.ModelCalls != 2 {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		if !strings.Contains(out.Text, "[[cite:"+gmvID+"]]") {
			t.Fatalf("text=%s", out.Text)
		}
		if h.provider.n() != 2 {
			t.Fatalf("模型调用 %d", h.provider.n())
		}
	})

	t.Run("伪造引用重写一次后通过", func(t *testing.T) {
		h, out, err := runSearch(t, tool,
			searchStep(gmvText),
			llm.Say("编造的口径。[[cite:not-in-this-run]]"),
			llm.Say("退款后仍计入。[[cite:"+gmvID+"]]"),
			llm.Say("不应有第二次重写"),
		)
		if err != nil || out.State != StateCompleted || out.ModelCalls != 3 {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		if strings.Contains(out.Text, "not-in-this-run") || !strings.Contains(out.Text, gmvID) {
			t.Fatalf("text=%s", out.Text)
		}
		feedback := requestText(h.provider.requests()[2])
		if !strings.Contains(feedback, "不在本次检索结果中") || !strings.Contains(feedback, "not-in-this-run") || !strings.Contains(feedback, "仅有的一次重写机会") {
			t.Fatalf("打回文本 = %s", feedback)
		}
		var saw bool
		for _, ev := range h.log.Events() {
			if ev.Code == CodeCitationInvalid && ev.Status == "rejected" {
				saw = true
			}
		}
		if !saw {
			t.Fatalf("日志 = %+v", h.log.Events())
		}
	})

	t.Run("重写后仍伪造则不能完成", func(t *testing.T) {
		h, out, err := runSearch(t, tool,
			searchStep(gmvText),
			llm.Say("第一次伪造 [[cite:not-in-this-run]]"),
			llm.Say("不应被接受 [[cite:still-fake]]"),
			llm.Say("第三次回答不该被拿去完成 [[cite:"+gmvID+"]]"),
		)
		if out.State != StateFailed || errorCode(err) != CodeCitationInvalid || out.ModelCalls != 3 {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		if strings.Contains(out.Text, "不应被接受") || strings.Contains(out.Text, "第三次回答") {
			t.Fatalf("伪造回答进入了完成文本: %s", out.Text)
		}
		if !strings.Contains(out.Text, "still-fake") {
			t.Fatalf("错误里应点名仍未检索到的引用: %s", out.Text)
		}
		if h.provider.n() != 3 {
			t.Fatalf("多给了重写次数: %d", h.provider.n())
		}
	})
}

func TestNoEvidenceRewriteAndUnavailable(t *testing.T) {
	above := float32(2)
	tool, gmvID := importedSearch(t, &above)

	t.Run("低分直接说明未找到依据", func(t *testing.T) {
		h, out, err := runSearch(t, tool,
			searchStep(gmvText),
			llm.Say("未找到依据"),
			llm.Say("不应再调用"),
		)
		if err != nil || out.State != StateCompleted || out.Text != "未找到依据" || out.ModelCalls != 2 {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		body := strings.Join(toolContents(h.provider.requests()[1]), "\n")
		if !strings.Contains(body, `"status":"no_evidence"`) || !strings.Contains(body, rag.NoEvidenceMessage) || !strings.Contains(body, `"hits":[]`) || strings.Contains(body, gmvID) {
			t.Fatalf("工具结果 = %s", body)
		}
	})

	t.Run("低分后编造数值被打回", func(t *testing.T) {
		h, out, err := runSearch(t, tool,
			searchStep(gmvText),
			llm.Say("GMV 同比上升 20%"),
			llm.Say("未找到依据"),
			llm.Say("不应第三轮"),
		)
		if err != nil || out.State != StateCompleted || out.Text != "未找到依据" || out.ModelCalls != 3 {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		feedback := requestText(h.provider.requests()[2])
		if !strings.Contains(feedback, "无依据拒答未通过") || !strings.Contains(feedback, "未找到依据") {
			t.Fatalf("打回文本 = %s", feedback)
		}
		if strings.Contains(out.Text, "20%") {
			t.Fatal("编造数值被当成完成回答")
		}
	})

	t.Run("低分片段不能当作本次引用", func(t *testing.T) {
		_, out, err := runSearch(t, tool,
			searchStep(gmvText),
			llm.Say("依据如下 [[cite:"+gmvID+"]]"),
			llm.Say("仍引用 [[cite:"+gmvID+"]]，未找到依据"),
		)
		if out.State != StateFailed || errorCode(err) != CodeCitationInvalid {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		if strings.Contains(out.Text, "依据如下") {
			t.Fatalf("伪造引用被完成: %s", out.Text)
		}
	})

	t.Run("嵌入不可用不是空命中", func(t *testing.T) {
		downTool := downSearch(t)
		h, out, err := runSearch(t, downTool,
			searchStep(gmvText),
			llm.Say("依据 [[cite:"+gmvID+"]]"),
			llm.Say("嵌入服务不可用，无法给出口径"),
			llm.Say("不应再写"),
		)
		if err != nil || out.State != StateCompleted || out.ModelCalls != 3 {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		if strings.Contains(out.Text, gmvID) {
			t.Fatalf("不可用之后仍完成了伪造引用: %s", out.Text)
		}
		body := strings.Join(toolContents(h.provider.requests()[1]), "\n")
		if !strings.Contains(body, string(errcode.EmbeddingUnavailable)) || strings.Contains(body, `"status":"ok"`) || strings.Contains(body, `"hits"`) {
			t.Fatalf("工具反馈 = %s", body)
		}
	})
}

func TestMissingCitations(t *testing.T) {
	allowed := map[string]struct{}{"manual#structural#1": {}}
	missing := missingCitations("重复 [[cite:manual#structural#1]] 和 [[cite:manual#structural#1]]，外加 [[cite:fake]][[cite:]]", allowed)
	if len(missing) != 1 || missing[0] != "fake" {
		t.Fatalf("missing = %v", missing)
	}
	if missingCitations("没有引用", allowed) != nil {
		t.Fatal("无标记不应算引用")
	}
}

const gmvText = "已支付订单后来发生退款时仍计入"

func importedSearch(t *testing.T, threshold *float32) (tools.Tool, string) {
	t.Helper()
	emb, err := rag.NewFakeEmbedder("fake:deterministic", 8)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Create(t.Context(), t.TempDir(), "xinghe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	_, err = rag.ImportKB(t.Context(), ws.KB, emb, rag.Document{
		ID: "manual", Title: "手册", Format: rag.FormatMarkdown,
		Body: "# 手册\n\n引言\n\n## GMV\n\n" + gmvText + "\n",
	}, rag.WithStrategy(rag.StrategyStructural))
	if err != nil {
		t.Fatal(err)
	}
	store, err := rag.NewSQLiteStore(t.Context(), ws.KB, rag.IdentityOf(emb))
	if err != nil {
		t.Fatal(err)
	}
	var opts []rag.SearchOption
	if threshold != nil {
		opts = append(opts, rag.WithEvidenceThreshold(*threshold))
	}
	tool, err := rag.NewSearchTool(emb, store, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return tool, "manual#structural#1"
}

func downSearch(t *testing.T) tools.Tool {
	t.Helper()
	emb, err := rag.NewFakeEmbedder("fake:deterministic", 8)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Create(t.Context(), t.TempDir(), "xinghe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	_, err = rag.ImportKB(t.Context(), ws.KB, emb, rag.Document{
		ID: "manual", Title: "手册", Format: rag.FormatMarkdown,
		Body: "# 手册\n\n## GMV\n\n" + gmvText + "\n",
	}, rag.WithStrategy(rag.StrategyStructural))
	if err != nil {
		t.Fatal(err)
	}
	store, err := rag.NewSQLiteStore(t.Context(), ws.KB, rag.IdentityOf(emb))
	if err != nil {
		t.Fatal(err)
	}
	tool, err := rag.NewSearchTool(failEmbed{Embedder: emb, err: &rag.EmbedderUnavailableError{
		Reason: rag.EmbedderUnreachable, Guidance: "请启动 Ollama 并执行 ollama pull bge-m3。",
	}}, store)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

type failEmbed struct {
	rag.Embedder
	err error
}

func (f failEmbed) Embed(context.Context, []string) ([][]float32, error) {
	return nil, f.err
}

func runSearch(t *testing.T, tool tools.Tool, steps ...llm.Step) (*harness, Outcome, error) {
	t.Helper()
	reg := &tools.Registry{}
	if err := reg.Register(tool); err != nil {
		t.Fatal(err)
	}
	prov := &captureProvider{inner: llm.NewMock(llm.MustNewScript(steps...))}
	h := &harness{
		t:        t,
		reg:      reg,
		pol:      &countingPolicy{base: policy.Default()},
		tickets:  &memTickets{},
		approver: &approval.Fake{},
		events:   &events.Recorder{},
		log:      &MemoryLog{},
		audit:    &audit.Memory{},
		provider: prov,
	}
	h.loop = &Loop{
		Provider: prov,
		Tools:    reg,
		Policy:   h.pol,
		Approver: h.approver,
		Tickets:  h.tickets,
		Events:   h.events,
		Log:      h.log,
		Audit:    h.audit,
		Config:   Config{MaxSteps: 6, StepTimeout: time.Second, TotalTimeout: 5 * time.Second},
	}
	out, err := h.run()
	return h, out, err
}

func searchStep(query string) llm.Step {
	raw, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		panic(err)
	}
	return llm.CallTools(llm.ToolCall{ID: "s1", Name: "search_docs", Arguments: string(raw)})
}
