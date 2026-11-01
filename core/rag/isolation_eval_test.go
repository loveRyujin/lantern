package rag_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/core/rag"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

func TestIsolationLeakNamesTheSide(t *testing.T) {
	got := rag.IsolationLeak{
		QuerySide:   "云杉",
		QueryKB:     "/tmp/yunsan/kb.db",
		ForeignSide: "星禾",
		ChunkID:     "xinghe-manual#structural#0",
		DocID:       "xinghe-manual",
		Marker:      "星禾独有口径XH-MARKER-7f3a",
		Where:       rag.IsolationWhereSearchHit,
	}.String()
	want := `跨工作区泄露: 查询侧=云杉 库文件=/tmp/yunsan/kb.db 串入了 星禾 的片段 chunk_id=xinghe-manual#structural#0 doc_id=xinghe-manual 标记="星禾独有口径XH-MARKER-7f3a" 位置=search_hit`
	if got != want {
		t.Fatalf("got %s", got)
	}
	empty := rag.IsolationLeak{QuerySide: "星禾", QueryKB: "a.db", ForeignSide: "云杉", Marker: "m", Where: rag.IsolationWhereKBRow}.String()
	if !strings.Contains(empty, "chunk_id=(无 chunk_id)") || !strings.Contains(empty, "doc_id=(无 doc_id)") {
		t.Fatalf("空片段 = %s", empty)
	}
}

func TestWorkspaceIsolationEval(t *testing.T) {
	ctx := t.Context()
	emb := mustFake(t, "fake:deterministic", 8)
	root := t.TempDir()
	sides := []*isoSide{
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
		side.handle = mustWorkspace(t, root, side.id)
		importManual(t, side.handle, emb, side.docID, side.body)
		side.store = openStore(t, side.handle, rag.IdentityOf(emb))
		side.tool = mustSearch(t, emb, side.store)
		side.kb = mainDBFile(t, side.handle.KB)
		assertNoWorkspaceColumn(t, side.handle.KB)
		if !strings.HasSuffix(side.kb, string(filepath.Separator)+"kb.db") {
			t.Fatalf("%s 库文件 = %s", side.name, side.kb)
		}
	}
	if sides[0].kb == sides[1].kb || sides[0].handle.KB == sides[1].handle.KB {
		t.Fatalf("两个工作区打开了同一个库: %s", sides[0].kb)
	}
	assertSearchSourceHasNoWorkspaceFilter(t)

	var leaks []rag.IsolationLeak
	for _, side := range sides {
		other := sides[0]
		if other == side {
			other = sides[1]
		}
		ownText := chunkTextContaining(t, side.handle.KB, side.marker)
		own, err := side.tool.Execute(ctx, tools.Call{Args: []byte(`{"query":` + jsonString(ownText) + `}`)})
		if err != nil {
			t.Fatal(err)
		}
		var vectors int
		if err := side.handle.KB.QueryRowContext(ctx, `SELECT COUNT(*) FROM vectors`).Scan(&vectors); err != nil {
			t.Fatal(err)
		}
		if side.store.LastSearchStats().Chunks != vectors {
			t.Fatalf("%s 检索比较了 %d 个 chunk，本库 vectors 有 %d 行", side.name, side.store.LastSearchStats().Chunks, vectors)
		}
		payload := decodeSearch(t, own.Content)
		if !hitContains(payload, side.marker, side.docID) {
			t.Fatalf("%s 本库检索没有返回自己的片段: %s", side.name, own.Content)
		}
		leaks = append(leaks, leaksInHits(side, other, payload)...)
		leaks = append(leaks, leaksInRows(t, side, other)...)

		for _, foreign := range chunkTexts(t, other.handle.KB) {
			crossed, err := side.tool.Execute(ctx, tools.Call{Args: []byte(`{"query":` + jsonString(foreign) + `}`)})
			if err != nil {
				t.Fatal(err)
			}
			leaks = append(leaks, leaksInHits(side, other, decodeSearch(t, crossed.Content))...)
		}
	}
	if len(leaks) != 0 {
		t.Fatalf("跨工作区泄露数 = %d\n%s", len(leaks), joinLeaks(leaks))
	}
	t.Log("跨工作区泄露数 = 0")
}

type isoSide struct {
	name   string
	id     string
	docID  string
	marker string
	body   string
	handle *workspace.Handle
	store  *rag.SQLiteStore
	tool   *rag.SearchTool
	kb     string
}

func mainDBFile(t *testing.T, db *sql.DB) string {
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if file == "" {
		t.Fatal("kb.db 没有文件路径")
	}
	return file
}

func assertNoWorkspaceColumn(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"documents", "chunks", "vectors", "index_meta"} {
		rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var cid, notnull, pk int
			var name, typ string
			var dflt sql.NullString
			if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if name == "workspace_id" {
				rows.Close()
				t.Fatalf("%s 出现了 workspace_id 列", table)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
	}
}

func assertSearchSourceHasNoWorkspaceFilter(t *testing.T) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("找不到测试文件")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(file), "sqlite.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "workspace_id") {
		t.Fatal("sqlite.go 出现了 workspace_id，隔离不应靠共用表过滤")
	}
}

func chunkTexts(t *testing.T, db *sql.DB) []string {
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("库里没有片段")
	}
	return out
}

func chunkTextContaining(t *testing.T, db *sql.DB, marker string) string {
	t.Helper()
	for _, text := range chunkTexts(t, db) {
		if strings.Contains(text, marker) {
			return text
		}
	}
	t.Fatalf("没有含 %s 的片段", marker)
	return ""
}

func hitContains(payload searchView, marker, docID string) bool {
	for _, hit := range payload.Hits {
		if strings.Contains(hit.Text, marker) && hit.DocID == docID {
			return true
		}
	}
	return false
}

// leaksInHits 只看命中。query 字段会回写本次检索词，那不是另一库的片段。
func leaksInHits(side, other *isoSide, payload searchView) []rag.IsolationLeak {
	var leaks []rag.IsolationLeak
	for _, hit := range payload.Hits {
		if strings.Contains(hit.Text, other.marker) || hit.DocID == other.docID || strings.Contains(hit.ChunkID, other.docID) {
			leaks = append(leaks, rag.IsolationLeak{
				QuerySide:   side.name,
				QueryKB:     side.kb,
				ForeignSide: other.name,
				ChunkID:     hit.ChunkID,
				DocID:       hit.DocID,
				Marker:      other.marker,
				Where:       rag.IsolationWhereSearchHit,
			})
		}
	}
	return leaks
}

func leaksInRows(t *testing.T, side, other *isoSide) []rag.IsolationLeak {
	t.Helper()
	var leaks []rag.IsolationLeak
	rows, err := side.handle.KB.Query(`SELECT id, document_id, text FROM chunks`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
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
	docs, err := side.handle.KB.Query(`SELECT id, body FROM documents`)
	if err != nil {
		t.Fatal(err)
	}
	defer docs.Close()
	for docs.Next() {
		var id, body string
		if err := docs.Scan(&id, &body); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(body, other.marker) || id == other.docID {
			leaks = append(leaks, rag.IsolationLeak{
				QuerySide: side.name, QueryKB: side.kb, ForeignSide: other.name,
				DocID: id, Marker: other.marker, Where: rag.IsolationWhereKBRow,
			})
		}
	}
	if err := docs.Err(); err != nil {
		t.Fatal(err)
	}
	return leaks
}

func joinLeaks(leaks []rag.IsolationLeak) string {
	parts := make([]string, len(leaks))
	for i, leak := range leaks {
		parts[i] = leak.String()
	}
	return strings.Join(parts, "\n")
}
