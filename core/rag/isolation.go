package rag

import "fmt"

const (
	// IsolationWhereSearchHit 表示命中列表里出现了另一个工作区的片段。
	IsolationWhereSearchHit = "search_hit"
	// IsolationWhereKBRow 表示当前打开的 kb.db 的行里出现了另一个工作区的正文。
	IsolationWhereKBRow = "kb_row"
	// IsolationWherePrompt 表示另一工作区的片段进入了模型请求。
	IsolationWherePrompt = "prompt"
	// IsolationWhereAnswer 表示另一工作区的片段进入了最终回答。
	IsolationWhereAnswer = "answer"
)

// IsolationLeak 是一次跨工作区评测发现的串库。
//
// 查询侧是这次打开并检索的库。串入侧是不该出现的另一个工作区。
// 字符串会点名库文件、chunk_id、doc_id 和位置，方便看出是哪一侧串了。
type IsolationLeak struct {
	QuerySide   string
	QueryKB     string
	ForeignSide string
	ChunkID     string
	DocID       string
	Marker      string
	Where       string
}

// String 实现评测失败时的固定句式。
func (l IsolationLeak) String() string {
	chunk := l.ChunkID
	if chunk == "" {
		chunk = "(无 chunk_id)"
	}
	doc := l.DocID
	if doc == "" {
		doc = "(无 doc_id)"
	}
	return fmt.Sprintf("跨工作区泄露: 查询侧=%s 库文件=%s 串入了 %s 的片段 chunk_id=%s doc_id=%s 标记=%q 位置=%s",
		l.QuerySide, l.QueryKB, l.ForeignSide, chunk, doc, l.Marker, l.Where)
}
