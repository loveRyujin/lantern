package rag

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// SplitFixed 按 Unicode 码点切固定窗口。
//
// size 是窗口长度，overlap 是相邻窗口共用的码点数，都必须满足 0 ≤ overlap < size。
// 正文会先去掉 BOM、把 CR/LF 折成 `\n`，再去掉首尾空白。窗口按码点滑动，不会从多字节字符中间切开。
// 窗口内的原文保留，包括其中的空白，这样重叠的码点能原样对上。
// 设计里的 512 token、重叠 64 对应默认的 FixedWindowRunes 和 FixedOverlapRunes；这里不调用分词器。
// 正文为空或只剩空白时返回 nil。某个窗口去掉空白后为空则不产出该窗口。
func SplitFixed(body string, size, overlap int) ([]Chunk, error) {
	if size <= 0 || overlap < 0 || overlap >= size {
		return nil, fmt.Errorf("%w: fixed 窗口必须是正的码点数，且重叠小于窗口", ErrInvalidArgument)
	}
	text := strings.TrimSpace(normalizeText(body))
	if text == "" {
		return nil, nil
	}
	runes := []rune(text)
	if len(runes) <= size {
		return []Chunk{{
			Text:     text,
			Strategy: StrategyFixed,
		}}, nil
	}
	step := size - overlap
	out := make([]Chunk, 0, (len(runes)+step-1)/step)
	for start := 0; start < len(runes); {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}
		frag := string(runes[start:end])
		if strings.TrimSpace(frag) != "" {
			out = append(out, Chunk{Text: frag, Strategy: StrategyFixed})
		}
		if end == len(runes) {
			break
		}
		start += step
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// SplitStructural 按 Markdown 标题和口径条目切分。
//
// 标题是 ATX（`#` 到 `######`，井号后要有空格）。标题路径用 TitlePathSeparator 连接，
// 从文档标题到当前标题。围栏代码块（``` 或 ~~~）里的井号不当成标题。
// 口径条目是列表项，形如 `- 名称：定义` 或 `* 名称: 定义`，冒号可以是全角或半角。
// 条目的 title_path 挂在当前标题下面，正文是冒号后面的定义；空行结束这个条目。
// 没有标题也没有条目时，整篇非空正文是一条 title_path 为空的片段。
// 只有标题、没有正文时不产生片段。空文档和纯空白返回 nil。
func SplitStructural(body string) []Chunk {
	text := normalizeText(body)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	s := &structuralSplitter{}
	for _, line := range strings.Split(text, "\n") {
		s.line(line)
	}
	s.flush()
	if len(s.out) == 0 {
		return nil
	}
	return s.out
}

func normalizeText(body string) string {
	body = strings.TrimPrefix(body, "\uFEFF")
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	return body
}

type atxHeading struct {
	level int
	title string
}

type structuralSplitter struct {
	stack   []atxHeading
	path    string
	entry   string
	buf     []string
	inFence bool
	out     []Chunk
}

func (s *structuralSplitter) line(line string) {
	if isFence(line) {
		s.inFence = !s.inFence
		s.buf = append(s.buf, line)
		return
	}
	if !s.inFence {
		if level, title, ok := parseATX(line); ok {
			s.flush()
			s.push(level, title)
			return
		}
		if label, rest, ok := parseEntry(line); ok {
			s.flush()
			s.entry = label
			s.buf = append(s.buf, rest)
			return
		}
	}
	if s.entry != "" && strings.TrimSpace(line) == "" {
		s.flush()
		return
	}
	s.buf = append(s.buf, line)
}

func (s *structuralSplitter) push(level int, title string) {
	for len(s.stack) > 0 && s.stack[len(s.stack)-1].level >= level {
		s.stack = s.stack[:len(s.stack)-1]
	}
	s.stack = append(s.stack, atxHeading{level: level, title: title})
	parts := make([]string, len(s.stack))
	for i, h := range s.stack {
		parts[i] = h.title
	}
	s.path = strings.Join(parts, TitlePathSeparator)
}

func (s *structuralSplitter) flush() {
	text := strings.TrimSpace(strings.Join(s.buf, "\n"))
	s.buf = s.buf[:0]
	entry := s.entry
	s.entry = ""
	if text == "" {
		return
	}
	path := s.path
	if entry != "" {
		if path != "" {
			path += TitlePathSeparator
		}
		path += entry
	}
	s.out = append(s.out, Chunk{
		Text:      text,
		TitlePath: path,
		Strategy:  StrategyStructural,
	})
}

func isFence(line string) bool {
	trim := strings.TrimSpace(line)
	return strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~")
}

func parseATX(line string) (int, string, bool) {
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++
	}
	if i > 3 {
		return 0, "", false
	}
	start := i
	for i < len(line) && line[i] == '#' {
		i++
	}
	level := i - start
	if level < 1 || level > 6 {
		return 0, "", false
	}
	if i >= len(line) || (line[i] != ' ' && line[i] != '\t') {
		return 0, "", false
	}
	title := stripClosingHashes(line[i:])
	if title == "" {
		return 0, "", false
	}
	return level, title, true
}

func stripClosingHashes(title string) string {
	raw := strings.TrimSpace(title)
	if !strings.HasSuffix(raw, "#") {
		return raw
	}
	without := strings.TrimRight(raw, "#")
	if without == "" {
		return ""
	}
	if !strings.HasSuffix(without, " ") && !strings.HasSuffix(without, "\t") {
		return raw
	}
	return strings.TrimSpace(without)
}

func parseEntry(line string) (string, string, bool) {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i >= len(line) {
		return "", "", false
	}
	switch line[i] {
	case '-', '*', '+':
	default:
		return "", "", false
	}
	i++
	if i >= len(line) || (line[i] != ' ' && line[i] != '\t') {
		return "", "", false
	}
	content := strings.TrimSpace(line[i:])
	sep := strings.IndexAny(content, "：:")
	if sep <= 0 {
		return "", "", false
	}
	label := strings.TrimSpace(content[:sep])
	if label == "" || utf8.RuneCountInString(label) > 40 {
		return "", "", false
	}
	rest := content[sep:]
	_, width := utf8.DecodeRuneInString(rest)
	rest = strings.TrimSpace(rest[width:])
	if rest == "" {
		return "", "", false
	}
	return label, rest, true
}
