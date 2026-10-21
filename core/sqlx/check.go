package sqlx

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// 行数和 SQL 长度。limit 不传时用 DefaultLimit。Schema 里的 maximum / maxLength 必须和这里一致。
const (
	DefaultLimit = 200
	MaxLimit     = 1000
	MaxSQLRunes  = 65536
)

// Check 是第一层过滤，只看这一段文本。
// fmt.Sprintf、变量和 strings.Builder 拼出来的 SQL 不会以完整文本出现在这里，它看不见。
// 这是已知限制。真正拦住写入的是只读连接和执行后的文件哈希，不是这个函数。
func Check(sql string) error {
	if strings.IndexByte(sql, 0) >= 0 {
		return reject(msgBadChar)
	}
	if utf8.RuneCountInString(sql) > MaxSQLRunes {
		return reject(msgTooLong)
	}
	sql = strings.TrimPrefix(sql, "\uFEFF")
	stmts, err := splitStatements(sql)
	if err != nil {
		return err
	}
	switch len(stmts) {
	case 0:
		return reject(msgEmpty)
	case 1:
		return vetStatement(stmts[0])
	default:
		return reject(msgMulti)
	}
}

const (
	msgEmpty      = "SQL 为空"
	msgMulti      = "只允许一条查询"
	msgNotSelect  = "只允许一条 SELECT 或 WITH … SELECT 查询"
	msgWrite      = "只允许只读查询"
	msgPragma     = "不能执行 PRAGMA，表结构请用 schema 工具查询"
	msgAttach     = "不允许附加其他数据库"
	msgFunc       = "不允许调用该函数"
	msgIncomplete = "SQL 不完整"
	msgTooLong    = "SQL 超过长度上限"
	msgBadChar    = "SQL 含有非法字符"
)

// Rejection 是语句扫描拒绝的原因。文本给模型看，不带路径，也不回显 SQL。
type Rejection struct {
	Reason string
}

func (e *Rejection) Error() string {
	if e == nil {
		return msgNotSelect
	}
	return e.Reason
}

func reject(reason string) error {
	return &Rejection{Reason: reason}
}

type tokKind int

const (
	tokEOF tokKind = iota
	tokIdent
	tokSemi
	tokLParen
	tokRParen
	tokOther
)

type token struct {
	kind   tokKind
	text   string
	quoted bool
}

type scanner struct {
	s string
	i int
}

func splitStatements(sql string) ([][]token, error) {
	sc := &scanner{s: sql}
	var stmts [][]token
	var cur []token
	for {
		tok, err := sc.nextToken()
		if err != nil {
			return nil, err
		}
		if tok.kind == tokEOF {
			break
		}
		if tok.kind == tokSemi {
			if len(cur) > 0 {
				stmts = append(stmts, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, tok)
	}
	if len(cur) > 0 {
		stmts = append(stmts, cur)
	}
	return stmts, nil
}

func (sc *scanner) nextToken() (token, error) {
	for {
		r, ok := sc.peek()
		if !ok {
			return token{kind: tokEOF}, nil
		}
		if unicode.IsSpace(r) {
			sc.bump()
			continue
		}
		if r == '-' && sc.peekAt(1) == '-' {
			sc.bump()
			sc.bump()
			for {
				n, ok := sc.peek()
				if !ok || n == '\n' {
					break
				}
				sc.bump()
			}
			continue
		}
		if r == '/' && sc.peekAt(1) == '*' {
			sc.bump()
			sc.bump()
			closed := false
			for {
				n, ok := sc.peek()
				if !ok {
					break
				}
				sc.bump()
				if n == '*' {
					nxt, nok := sc.peek()
					if nok && nxt == '/' {
						sc.bump()
						closed = true
						break
					}
				}
			}
			if !closed {
				return token{}, reject(msgIncomplete)
			}
			continue
		}
		break
	}
	r, ok := sc.peek()
	if !ok {
		return token{kind: tokEOF}, nil
	}
	switch r {
	case ';':
		sc.bump()
		return token{kind: tokSemi, text: ";"}, nil
	case '(':
		sc.bump()
		return token{kind: tokLParen, text: "("}, nil
	case ')':
		sc.bump()
		return token{kind: tokRParen, text: ")"}, nil
	case '\'':
		if err := sc.skipQuoted('\'', '\''); err != nil {
			return token{}, err
		}
		return token{kind: tokOther, text: "string"}, nil
	case '"':
		text, err := sc.readQuoted('"', '"')
		if err != nil {
			return token{}, err
		}
		return token{kind: tokIdent, text: text, quoted: true}, nil
	case '`':
		text, err := sc.readQuoted('`', '`')
		if err != nil {
			return token{}, err
		}
		return token{kind: tokIdent, text: text, quoted: true}, nil
	case '[':
		text, err := sc.readBracket()
		if err != nil {
			return token{}, err
		}
		return token{kind: tokIdent, text: text, quoted: true}, nil
	}
	if isIdentStart(r) {
		return sc.readIdent(), nil
	}
	sc.bump()
	return token{kind: tokOther, text: string(r)}, nil
}

func (sc *scanner) readIdent() token {
	start := sc.i
	sc.bump()
	for {
		r, ok := sc.peek()
		if !ok || !isIdentCont(r) {
			break
		}
		sc.bump()
	}
	return token{kind: tokIdent, text: sc.s[start:sc.i]}
}

func (sc *scanner) skipQuoted(quote, esc rune) error {
	_, err := sc.readQuoted(quote, esc)
	return err
}

func (sc *scanner) readQuoted(quote, esc rune) (string, error) {
	sc.bump()
	var b strings.Builder
	for {
		r, ok := sc.peek()
		if !ok {
			return "", reject(msgIncomplete)
		}
		sc.bump()
		if r != quote {
			b.WriteRune(r)
			continue
		}
		if nxt, ok := sc.peek(); ok && nxt == esc && quote == esc {
			sc.bump()
			b.WriteRune(quote)
			continue
		}
		return b.String(), nil
	}
}

func (sc *scanner) readBracket() (string, error) {
	sc.bump()
	var b strings.Builder
	for {
		r, ok := sc.peek()
		if !ok {
			return "", reject(msgIncomplete)
		}
		sc.bump()
		if r != ']' {
			b.WriteRune(r)
			continue
		}
		if nxt, ok := sc.peek(); ok && nxt == ']' {
			sc.bump()
			b.WriteRune(']')
			continue
		}
		return b.String(), nil
	}
}

func (sc *scanner) peek() (rune, bool) {
	if sc.i >= len(sc.s) {
		return 0, false
	}
	r, _ := utf8.DecodeRuneInString(sc.s[sc.i:])
	return r, true
}

func (sc *scanner) peekAt(n int) rune {
	i := sc.i
	var r rune
	for range n + 1 {
		if i >= len(sc.s) {
			return 0
		}
		var w int
		r, w = utf8.DecodeRuneInString(sc.s[i:])
		i += w
	}
	return r
}

func (sc *scanner) bump() {
	if sc.i >= len(sc.s) {
		return
	}
	_, w := utf8.DecodeRuneInString(sc.s[sc.i:])
	sc.i += w
}

func isIdentStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isIdentCont(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) || r == '$'
}

func vetStatement(toks []token) error {
	if len(toks) == 0 {
		return reject(msgEmpty)
	}
	if msg := findForbidden(toks); msg != "" {
		return reject(msg)
	}
	if lastStarter(toks) != "SELECT" {
		return reject(msgNotSelect)
	}
	return nil
}

func findForbidden(toks []token) string {
	best := 0
	msg := ""
	consider := func(pri int, reason string) {
		if pri > best {
			best = pri
			msg = reason
		}
	}
	for i, tok := range toks {
		if tok.kind != tokIdent || tok.quoted {
			continue
		}
		upper := strings.ToUpper(tok.text)
		lower := strings.ToLower(tok.text)
		switch {
		case upper == "PRAGMA" || strings.HasPrefix(lower, "pragma_"):
			consider(4, msgPragma)
		case upper == "ATTACH" || upper == "DETACH":
			consider(3, msgAttach)
		case bannedFunc(lower):
			consider(2, msgFunc)
		case upper == "REPLACE" && nextIsParen(toks, i):
		case upper == "EXPLAIN":
			consider(2, msgNotSelect)
		case writeKeyword(upper):
			consider(2, msgWrite)
		}
	}
	return msg
}

func bannedFunc(lower string) bool {
	switch lower {
	case "load_extension", "writefile", "readfile", "sqlite3_load_extension":
		return true
	default:
		return false
	}
}

func writeKeyword(upper string) bool {
	switch upper {
	case "INSERT", "UPDATE", "DELETE", "REPLACE", "DROP", "CREATE", "ALTER",
		"REINDEX", "VACUUM", "ANALYZE", "BEGIN", "COMMIT", "ROLLBACK",
		"SAVEPOINT", "RELEASE":
		return true
	default:
		return false
	}
}

func nextIsParen(toks []token, i int) bool {
	return i+1 < len(toks) && toks[i+1].kind == tokLParen
}

func lastStarter(toks []token) string {
	depth := 0
	last := ""
	for i, tok := range toks {
		switch tok.kind {
		case tokLParen:
			depth++
			continue
		case tokRParen:
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth != 0 || tok.kind != tokIdent || tok.quoted {
			continue
		}
		upper := strings.ToUpper(tok.text)
		if !starter(upper) {
			continue
		}
		// replace(...) 是函数，不是 REPLACE INTO。
		if upper == "REPLACE" && nextIsParen(toks, i) {
			continue
		}
		last = upper
	}
	return last
}

func starter(upper string) bool {
	switch upper {
	case "SELECT", "WITH", "VALUES", "INSERT", "UPDATE", "DELETE", "REPLACE",
		"EXPLAIN", "PRAGMA", "ATTACH", "DETACH", "CREATE", "DROP", "ALTER",
		"VACUUM", "REINDEX", "ANALYZE", "BEGIN":
		return true
	default:
		return false
	}
}
