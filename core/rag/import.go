package rag

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	// FormatMarkdown 是 Markdown 文档。
	FormatMarkdown = "markdown"
	// FormatText 是纯文本。切分规则和 Markdown 相同：有标题就按标题切。
	FormatText = "text"
)

// ErrUnsupportedFormat 表示这次导入不接受该文件。PDF 和其他扩展名都是这个错误。
var ErrUnsupportedFormat = errors.New("rag: 不支持的文档格式")

// Document 是一篇要导入的文档。ID 在一个工作区的 kb.db 里唯一，再次导入同一 ID 会替换旧片段。
type Document struct {
	// ID 是文档标识。空字符串和只含空白的值都会拒绝。
	ID string
	// Title 写入 documents.title。空白时用 ID。
	Title string
	// Source 写入 documents.source，例如文件路径。可以为空。
	Source string
	// Body 是原文。切分看正文，不看标题。
	Body string
	// Format 是 FormatMarkdown 或 FormatText。导入逻辑不靠它区分切分规则。
	Format string
}

// ImportResult 是一次导入的结果。出错时不要读取它。
type ImportResult struct {
	// DocID 是这次导入的文档。
	DocID string
	// Strategy 是实际使用的切分策略。
	Strategy string
	// Chunks 是写入的片段数。跳过时为 0。
	Chunks int
	// Skipped 表示正文没有切出任何片段（空文档或只有空白、只有标题）。
	// 这种情况下不调用 Embedder，也不改向量库。
	Skipped bool
}

// Option 调整导入使用的切分策略和 fixed 窗口。
type Option func(*importConfig)

type importConfig struct {
	strategy  string
	size      int
	overlap   int
	windowSet bool
}

// WithStrategy 选择 StrategyFixed 或 StrategyStructural。不设置时 Import 会拒绝。
func WithStrategy(strategy string) Option {
	return func(cfg *importConfig) { cfg.strategy = strategy }
}

// WithFixedRunes 设置 fixed 的窗口和重叠，单位都是 Unicode 码点。
// structural 忽略这项。不设置时使用 FixedWindowRunes 和 FixedOverlapRunes。
func WithFixedRunes(size, overlap int) Option {
	return func(cfg *importConfig) {
		cfg.size = size
		cfg.overlap = overlap
		cfg.windowSet = true
	}
}

// documentReplacer 在校验通过后才替换一个文档的全部分片。
// SQLiteStore 把这一步放在同一个事务里；FakeStore 在同一把锁里做完。
type documentReplacer interface {
	ReplaceDocument(ctx context.Context, doc Document, chunks []Chunk) error
}

// Import 把文档切分、嵌入并写入 store。
//
// 顺序固定为：切分 → 若没有片段则跳过（不嵌入、不写库）→
// 确认 store 能一次替换整篇文档 → 已知库先比对 index_meta →
// 一次 Embed 拿到全部向量 → ReplaceDocument。
// Embed 返回错误、条数不对或维度不对时，不调用替换，已有片段保持原样。
// Ollama 不可用时错误是 ErrEmbedderUnavailable，码为 embedding_unavailable，不会写入空向量。
// store 必须实现 ReplaceDocument（SQLiteStore 和 FakeStore 都实现了）。
// 不走「先 Delete 再 Upsert」：那两步各自提交，中间失败会把旧片段删掉。
func Import(ctx context.Context, store VectorStore, embedder Embedder, doc Document, opts ...Option) (ImportResult, error) {
	if ctx == nil {
		return ImportResult{}, errors.New("rag: ctx 不能为 nil")
	}
	if err := ctx.Err(); err != nil {
		return ImportResult{}, err
	}
	if store == nil {
		return ImportResult{}, fmt.Errorf("%w: 缺少向量库", ErrInvalidArgument)
	}
	if embedder == nil {
		return ImportResult{}, errors.New("rag: Embedder 为 nil")
	}
	if err := validateDocument(doc); err != nil {
		return ImportResult{}, err
	}
	cfg := importConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	parts, err := splitDocument(doc.Body, cfg)
	if err != nil {
		return ImportResult{}, err
	}
	result := ImportResult{DocID: doc.ID, Strategy: cfg.strategy}
	if len(parts) == 0 {
		result.Skipped = true
		return result, nil
	}
	if _, ok := store.(documentReplacer); !ok {
		return ImportResult{}, fmt.Errorf("%w: 向量库不能在一次替换里写入文档", ErrInvalidArgument)
	}
	if err := rejectIndexedMismatch(ctx, store, embedder); err != nil {
		return ImportResult{}, err
	}
	texts := make([]string, len(parts))
	for i, part := range parts {
		texts[i] = part.Text
	}
	vectors, err := embedder.Embed(ctx, texts)
	if err != nil {
		return ImportResult{}, err
	}
	if len(vectors) != len(parts) {
		return ImportResult{}, fmt.Errorf("%w: Embedder 返回的向量条数和片段不一致", ErrInvalidArgument)
	}
	for i := range parts {
		if len(vectors[i]) != embedder.Dim() {
			return ImportResult{}, &DimensionError{Got: len(vectors[i]), Want: embedder.Dim()}
		}
		parts[i].ID = chunkID(doc.ID, cfg.strategy, i)
		parts[i].DocID = doc.ID
		parts[i].EmbedderID = embedder.ID()
		parts[i].Vector = vectors[i]
	}
	if err := ctx.Err(); err != nil {
		return ImportResult{}, err
	}
	if err := store.(documentReplacer).ReplaceDocument(ctx, doc, parts); err != nil {
		return ImportResult{}, err
	}
	result.Chunks = len(parts)
	return result, nil
}

// ImportKB 把文档写入这个工作区自己的 kb.db。
// kb 应是 workspace.Handle.KB。库的当前模型和 embedder 的 ID、维度一致；不一致时不嵌入、不写入。
func ImportKB(ctx context.Context, kb *sql.DB, embedder Embedder, doc Document, opts ...Option) (ImportResult, error) {
	if embedder == nil {
		return ImportResult{}, errors.New("rag: Embedder 为 nil")
	}
	store, err := NewSQLiteStore(ctx, kb, IdentityOf(embedder))
	if err != nil {
		return ImportResult{}, err
	}
	return Import(ctx, store, embedder, doc, opts...)
}

// ReadDocument 读取 Markdown 或 TXT。docID 由调用方给定，再次导入时用同一个值。
// 扩展名 .pdf 返回 ErrUnsupportedFormat。正文必须是合法 UTF-8。
func ReadDocument(path, docID string) (Document, error) {
	if err := validateDocID(docID); err != nil {
		return Document{}, err
	}
	format, err := formatOf(path)
	if err != nil {
		return Document{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Document{}, fmt.Errorf("rag: 读取文档: %w", err)
	}
	if !utf8.Valid(body) {
		return Document{}, fmt.Errorf("%w: 文档不是 UTF-8", ErrInvalidArgument)
	}
	base := filepath.Base(path)
	return Document{
		ID:     docID,
		Title:  strings.TrimSuffix(base, filepath.Ext(base)),
		Source: filepath.Clean(path),
		Body:   string(body),
		Format: format,
	}, nil
}

// ImportFile 读取一个 Markdown 或 TXT，并写入 kb 所在工作区。
func ImportFile(ctx context.Context, kb *sql.DB, embedder Embedder, path, docID string, opts ...Option) (ImportResult, error) {
	doc, err := ReadDocument(path, docID)
	if err != nil {
		return ImportResult{}, err
	}
	return ImportKB(ctx, kb, embedder, doc, opts...)
}

func splitDocument(body string, cfg importConfig) ([]Chunk, error) {
	switch cfg.strategy {
	case StrategyFixed:
		size, overlap := FixedWindowRunes, FixedOverlapRunes
		if cfg.windowSet {
			size, overlap = cfg.size, cfg.overlap
		}
		return SplitFixed(body, size, overlap)
	case StrategyStructural:
		return SplitStructural(body), nil
	default:
		return nil, fmt.Errorf("%w: 切分策略必须是 fixed 或 structural", ErrInvalidArgument)
	}
}

func chunkID(docID, strategy string, ordinal int) string {
	return fmt.Sprintf("%s#%s#%d", docID, strategy, ordinal)
}

func validateDocument(doc Document) error {
	return validateDocID(doc.ID)
}

func validateDocID(id string) error {
	if strings.TrimSpace(id) == "" || len(id) > 512 || strings.ContainsAny(id, "\x00\r\n") {
		return fmt.Errorf("%w: doc_id 不合法", ErrInvalidArgument)
	}
	return nil
}

func formatOf(path string) (string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return FormatMarkdown, nil
	case ".txt", ".text":
		return FormatText, nil
	case ".pdf":
		return "", fmt.Errorf("%w: PDF 不在导入范围", ErrUnsupportedFormat)
	default:
		return "", fmt.Errorf("%w: 只接受 Markdown 和 TXT", ErrUnsupportedFormat)
	}
}

func rejectIndexedMismatch(ctx context.Context, store VectorStore, embedder Embedder) error {
	have, ok, err := indexedIdentity(ctx, store)
	if err != nil || !ok {
		return err
	}
	want := IdentityOf(embedder)
	if have != want {
		return &EmbedderMismatchError{Have: have, Want: want}
	}
	return nil
}

func indexedIdentity(ctx context.Context, store VectorStore) (EmbedderIdentity, bool, error) {
	switch s := store.(type) {
	case *SQLiteStore:
		meta, ok, err := s.Index(ctx)
		if err != nil || !ok {
			return EmbedderIdentity{}, false, err
		}
		return meta.Identity(), true, nil
	case *FakeStore:
		meta, ok := s.Index()
		if !ok {
			return EmbedderIdentity{}, false, nil
		}
		return meta.Identity(), true, nil
	default:
		return EmbedderIdentity{}, false, nil
	}
}
