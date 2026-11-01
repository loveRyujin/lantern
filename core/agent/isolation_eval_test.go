package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/rag"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

func TestWorkspaceIsolationQA(t *testing.T) {
	ctx := t.Context()
	emb, err := rag.NewFakeEmbedder("fake:deterministic", 8)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	sides := []*qaSide{
		{
			name: "星禾", id: "xinghe", docID: "xinghe-manual", marker: "星禾独有口径XH-MARKER-7f3a",
			body: "# 星禾手册\n\n## GMV\n\n星禾独有口径XH-MARKER-7f3a：已支付订单金额在退款后仍计入GMV。\n",
		},
		{
			name: "云杉", id: "yunsan", docID: "yunsan-manual", marker: "云杉独有口径YS-MARKER-91c2",
			body: "# 云杉手册\n\n## 退货率\n\n云杉独有口径YS-MARKER-91c2：退货单数除以支付单数得到退货率。\n",
		},
	}
	for _, side := range sides {
		h, err := workspace.Create(ctx, root, side.id)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Close() })
		side.handle = h
		if _, err := rag.ImportKB(ctx, h.KB, emb, rag.Document{
			ID: side.docID, Title: side.name, Format: rag.FormatMarkdown, Body: side.body,
		}, rag.WithStrategy(rag.StrategyStructural)); err != nil {
			t.Fatal(err)
		}
		store, err := rag.NewSQLiteStore(ctx, h.KB, rag.IdentityOf(emb))
		if err != nil {
			t.Fatal(err)
		}
		tool, err := rag.NewSearchTool(emb, store)
		if err != nil {
			t.Fatal(err)
		}
		side.tool = tool
		side.kb = qaDBFile(t, h.KB)
		side.ownText = qaChunkText(t, h.KB, side.marker)
	}
	if sides[0].kb == sides[1].kb {
		t.Fatalf("问答路径打开了同一个库文件: %s", sides[0].kb)
	}

	var leaks []rag.IsolationLeak
	for _, side := range sides {
		other := sides[0]
		if other == side {
			other = sides[1]
		}
		own := runIsolationQA(t, side, side.ownText)
		leaks = append(leaks, qaLeaks(t, side, other, own, true)...)
		for _, foreign := range qaChunkTexts(t, other.handle.KB) {
			crossed := runIsolationQA(t, side, foreign)
			leaks = append(leaks, qaLeaks(t, side, other, crossed, false)...)
		}
		leaks = append(leaks, qaRowLeaks(t, side, other)...)
	}
	if len(leaks) != 0 {
		parts := make([]string, len(leaks))
		for i, leak := range leaks {
			parts[i] = leak.String()
		}
		t.Fatalf("跨工作区泄露数 = %d\n%s", len(leaks), strings.Join(parts, "\n"))
	}
	t.Log("跨工作区泄露数 = 0")
}

type qaSide struct {
	name    string
	id      string
	docID   string
	marker  string
	body    string
	ownText string
	kb      string
	handle  *workspace.Handle
	tool    tools.Tool
}

type qaRun struct {
	answer string
	tool   string
	inner  string
}

func runIsolationQA(t *testing.T, side *qaSide, query string) qaRun {
	t.Helper()
	reg := &tools.Registry{}
	if err := reg.Register(side.tool); err != nil {
		t.Fatal(err)
	}
	prov := &isolationAnswerer{query: query}
	loop := &Loop{
		Provider: prov,
		Tools:    reg,
		Config:   Config{MaxSteps: 4, StepTimeout: time.Second, TotalTimeout: 5 * time.Second},
	}
	out, err := loop.Run(t.Context(), Input{
		RunID: "iso-" + side.id, WorkspaceID: side.id, Prompt: "请在当前工作区说明口径",
	})
	if err != nil || out.State != StateCompleted || out.ModelCalls != 2 {
		t.Fatalf("%s 问答 out=%+v err=%v", side.name, out, err)
	}
	if len(prov.reqs) != 2 {
		t.Fatalf("%s 模型请求 %d", side.name, len(prov.reqs))
	}
	var toolBody string
	for _, msg := range prov.reqs[1].Messages {
		if msg.Role == llm.RoleTool {
			toolBody = msg.Content
		}
	}
	if !strings.HasPrefix(toolBody, `<data source="search_docs">`) {
		t.Fatalf("%s 检索结果没有包进数据边界: %s", side.name, toolBody)
	}
	inner, ok := unwrapObservation(toolBody)
	if !ok {
		t.Fatalf("%s 观察值无法按数据边界解开: %s", side.name, toolBody)
	}
	return qaRun{answer: out.Text, tool: toolBody, inner: inner}
}

func qaLeaks(t *testing.T, side, other *qaSide, run qaRun, wantOwn bool) []rag.IsolationLeak {
	t.Helper()
	var leaks []rag.IsolationLeak
	leaks = append(leaks, leaksInObservation(side, other, run.inner, rag.IsolationWherePrompt)...)
	answerInner, ok := unwrapObservation(run.answer)
	if !ok {
		answerInner = run.answer
	}
	leaks = append(leaks, leaksInObservation(side, other, answerInner, rag.IsolationWhereAnswer)...)
	if strings.Contains(run.inner, other.docID) {
		leaks = append(leaks, rag.IsolationLeak{
			QuerySide: side.name, QueryKB: side.kb, ForeignSide: other.name,
			DocID: other.docID, Marker: other.marker, Where: rag.IsolationWherePrompt,
		})
	}
	if wantOwn {
		var payload searchPayloadView
		if err := json.Unmarshal([]byte(run.inner), &payload); err != nil {
			t.Fatalf("%s 本库结果不是检索 JSON: %v body=%s", side.name, err, run.inner)
		}
		found := false
		for _, hit := range payload.Hits {
			if strings.Contains(hit.Text, side.marker) && hit.DocID == side.docID {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s 问答没有返回本库片段: %s", side.name, run.inner)
		}
	}
	return leaks
}

type searchPayloadView struct {
	Hits []struct {
		ChunkID string `json:"chunk_id"`
		DocID   string `json:"doc_id"`
		Text    string `json:"text"`
	} `json:"hits"`
}

// leaksInObservation 只把命中里的外库片段算泄露。检索词回写在 query 字段，不算串库。
func leaksInObservation(side, other *qaSide, body, where string) []rag.IsolationLeak {
	var payload searchPayloadView
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		if strings.Contains(body, other.marker) || strings.Contains(body, other.docID) {
			return []rag.IsolationLeak{{
				QuerySide: side.name, QueryKB: side.kb, ForeignSide: other.name,
				Marker: other.marker, Where: where,
			}}
		}
		return nil
	}
	var leaks []rag.IsolationLeak
	for _, hit := range payload.Hits {
		if strings.Contains(hit.Text, other.marker) || hit.DocID == other.docID || strings.Contains(hit.ChunkID, other.docID) {
			leaks = append(leaks, rag.IsolationLeak{
				QuerySide: side.name, QueryKB: side.kb, ForeignSide: other.name,
				ChunkID: hit.ChunkID, DocID: hit.DocID, Marker: other.marker, Where: where,
			})
		}
	}
	return leaks
}

func qaRowLeaks(t *testing.T, side, other *qaSide) []rag.IsolationLeak {
	t.Helper()
	rows, err := side.handle.KB.Query(`SELECT id, document_id, text FROM chunks`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var leaks []rag.IsolationLeak
	for rows.Next() {
		var id, docID, text string
		if err := rows.Scan(&id, &docID, &text); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text, other.marker) || docID == other.docID {
			leaks = append(leaks, rag.IsolationLeak{
				QuerySide: side.name, QueryKB: side.kb, ForeignSide: other.name,
				ChunkID: id, DocID: docID, Marker: other.marker, Where: rag.IsolationWhereKBRow,
			})
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return leaks
}

func unwrapObservation(content string) (string, bool) {
	const open = "<data source=\""
	if !strings.HasPrefix(strings.ToLower(content), open) {
		return "", false
	}
	rest := content[len(open):]
	q := strings.Index(rest, "\">\n")
	if q < 0 || !strings.HasSuffix(rest[q:], DataBoundaryClose) {
		return "", false
	}
	body := rest[q+len("\">\n"):]
	return strings.TrimSuffix(body, DataBoundaryClose), true
}

func qaDBFile(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`PRAGMA database_list`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var file string
	for rows.Next() {
		var seq int
		var name, path string
		if err := rows.Scan(&seq, &name, &path); err != nil {
			t.Fatal(err)
		}
		if name == "main" {
			file = path
		}
	}
	if err := rows.Err(); err != nil || file == "" || !strings.HasSuffix(file, string(filepath.Separator)+"kb.db") {
		t.Fatalf("库文件 = %s err=%v", file, err)
	}
	return file
}

func qaChunkTexts(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT text FROM chunks ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(err)
		}
		out = append(out, text)
	}
	if err := rows.Err(); err != nil || len(out) == 0 {
		t.Fatalf("片段 err=%v n=%d", err, len(out))
	}
	return out
}

func qaChunkText(t *testing.T, db *sql.DB, marker string) string {
	t.Helper()
	for _, text := range qaChunkTexts(t, db) {
		if strings.Contains(text, marker) {
			return text
		}
	}
	t.Fatalf("没有含 %s 的片段", marker)
	return ""
}

type isolationAnswerer struct {
	query string
	n     int
	reqs  []llm.Request
}

func (p *isolationAnswerer) ID() string { return "mock" }

func (p *isolationAnswerer) Chat(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	copied := req
	copied.Messages = append([]llm.Message(nil), req.Messages...)
	p.n++
	p.reqs = append(p.reqs, copied)
	if p.n == 1 {
		raw, err := json.Marshal(map[string]string{"query": p.query})
		if err != nil {
			return llm.Response{}, err
		}
		return llm.Response{
			FinishReason: llm.FinishToolCalls,
			Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
				ID: "s1", Name: "search_docs", Arguments: string(raw),
			}}},
		}, nil
	}
	if p.n > 2 {
		return llm.Response{}, errors.New("隔离评测多调用了模型")
	}
	var b strings.Builder
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleTool {
			b.WriteString(msg.Content)
		}
	}
	return llm.Response{
		FinishReason: llm.FinishStop,
		Message:      llm.Message{Role: llm.RoleAssistant, Content: b.String()},
	}, nil
}
