package sqlx

import "strings"

// wrapResultCells 给最终结果的每一列套上 lantern_cell。
// 子查询和 WITH 里的列是中间值，不套。SELECT * FROM (子查询) 时，子查询的列就是最终结果，要套。
// 套不上就原样返回，连接上的 MaxValueBytes 仍然有效。
func wrapResultCells(sql string) string {
	toks, err := lexSQL(sql)
	if err != nil || len(toks) == 0 {
		return sql
	}
	edits := wrapQuery(sql, toks, skipWith(toks, 0), len(toks))
	return applyEdits(sql, edits)
}

func lexSQL(sql string) ([]token, error) {
	sc := &scanner{s: sql}
	var toks []token
	for {
		tok, err := sc.nextToken()
		if err != nil {
			return nil, err
		}
		if tok.kind == tokEOF || tok.kind == tokSemi {
			break
		}
		toks = append(toks, tok)
	}
	return toks, nil
}

type sqlEdit struct {
	start int
	end   int
	repl  string
}

func applyEdits(sql string, edits []sqlEdit) string {
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		if e.start < 0 || e.end > len(sql) || e.start > e.end {
			return sql
		}
		sql = sql[:e.start] + e.repl + sql[e.end:]
	}
	return sql
}

func skipWith(toks []token, i int) int {
	if !isWord(toks, i, "WITH") {
		return i
	}
	i++
	if isWord(toks, i, "RECURSIVE") {
		i++
	}
	for i < len(toks) {
		i++ // 名字
		if i < len(toks) && toks[i].kind == tokLParen {
			i = skipParen(toks, i)
		}
		if isWord(toks, i, "AS") {
			i++
		}
		if i < len(toks) && toks[i].kind == tokLParen {
			i = skipParen(toks, i)
		}
		if i < len(toks) && toks[i].text == "," {
			i++
			continue
		}
		break
	}
	return i
}

func skipParen(toks []token, i int) int {
	depth := 0
	for i < len(toks) {
		switch toks[i].kind {
		case tokLParen:
			depth++
		case tokRParen:
			depth--
			i++
			if depth == 0 {
				return i
			}
			continue
		}
		i++
	}
	return i
}

func wrapQuery(sql string, toks []token, i, end int) []sqlEdit {
	var edits []sqlEdit
	wrapQueryInto(sql, toks, i, end, &edits)
	return edits
}

func wrapQueryInto(sql string, toks []token, i, end int, edits *[]sqlEdit) int {
	for i < end && toks[i].kind == tokLParen {
		closeAt := skipParen(toks, i)
		if closeAt > end {
			closeAt = end
		}
		innerEnd := closeAt - 1
		if innerEnd < i+1 {
			innerEnd = i + 1
		}
		wrapQueryInto(sql, toks, i+1, innerEnd, edits)
		i = closeAt
		i = skipCompound(sql, toks, i, end, edits)
		return i
	}
	i = wrapSelect(sql, toks, i, end, edits)
	return skipCompound(sql, toks, i, end, edits)
}

func skipCompound(sql string, toks []token, i, end int, edits *[]sqlEdit) int {
	for i < end && isCompound(toks, i) {
		i++
		if isWord(toks, i, "ALL") {
			i++
		}
		i = wrapQueryInto(sql, toks, i, end, edits)
	}
	return i
}

func wrapSelect(sql string, toks []token, i, end int, edits *[]sqlEdit) int {
	if !isWord(toks, i, "SELECT") {
		return i
	}
	i++
	if isWord(toks, i, "DISTINCT") || isWord(toks, i, "ALL") {
		i++
	}
	type item struct {
		start int
		end   int
		star  bool
	}
	var items []item
	for i < end && !isClauseWord(toks, i) {
		if toks[i].kind == tokRParen {
			break
		}
		itemStart := i
		depth := 0
		for i < end {
			if depth == 0 && i > itemStart && isClauseWord(toks, i) {
				break
			}
			switch toks[i].kind {
			case tokLParen:
				depth++
			case tokRParen:
				if depth == 0 {
					goto itemDone
				}
				depth--
			default:
				if depth == 0 && toks[i].text == "," {
					goto itemDone
				}
			}
			i++
		}
	itemDone:
		if i > itemStart {
			items = append(items, item{start: itemStart, end: i, star: isStar(toks, itemStart, i)})
		}
		if i < end && toks[i].text == "," {
			i++
		}
	}
	if len(items) == 1 && items[0].star && isWord(toks, i, "FROM") {
		if j := i + 1; j < end && toks[j].kind == tokLParen {
			closeAt := skipParen(toks, j)
			innerEnd := closeAt - 1
			if innerEnd > j+1 {
				wrapQueryInto(sql, toks, j+1, innerEnd, edits)
			}
		}
	} else {
		col := 0
		for _, it := range items {
			if it.star {
				continue
			}
			if e, ok := wrapItem(sql, toks, it.start, it.end, col); ok {
				*edits = append(*edits, e)
			}
			col++
		}
	}
	for i < end {
		if toks[i].kind == tokLParen {
			i = skipParen(toks, i)
			continue
		}
		if isCompound(toks, i) {
			break
		}
		i++
	}
	return i
}

func wrapItem(sql string, toks []token, start, end, col int) (sqlEdit, bool) {
	if start >= end || toks[start].pos < 0 || toks[end-1].end > len(sql) {
		return sqlEdit{}, false
	}
	exprEnd := aliasAt(toks, start, end)
	if exprEnd <= start {
		return sqlEdit{}, false
	}
	expr := sql[toks[start].pos:toks[exprEnd-1].end]
	repl := resultCellName + "(" + itoa(col) + ", " + expr + ")"
	itemEnd := toks[end-1].end
	if exprEnd < end {
		repl += " " + sql[toks[exprEnd].pos:itemEnd]
	} else {
		repl += " AS " + quoteIdent(strings.TrimSpace(expr))
	}
	return sqlEdit{start: toks[start].pos, end: itemEnd, repl: repl}, true
}

func aliasAt(toks []token, start, end int) int {
	depth := 0
	for i := start; i < end; i++ {
		switch toks[i].kind {
		case tokLParen:
			depth++
		case tokRParen:
			if depth > 0 {
				depth--
			}
		}
		if depth == 0 && i+2 == end && isWord(toks, i, "AS") && toks[i+1].kind == tokIdent {
			return i
		}
	}
	return end
}

func isStar(toks []token, start, end int) bool {
	n := end - start
	switch n {
	case 1:
		return toks[start].text == "*"
	case 3:
		return toks[start].kind == tokIdent && toks[start+1].text == "." && toks[start+2].text == "*"
	case 5:
		return toks[start].kind == tokIdent && toks[start+1].text == "." && toks[start+2].kind == tokIdent && toks[start+3].text == "." && toks[start+4].text == "*"
	default:
		return false
	}
}

func isCompound(toks []token, i int) bool {
	return isWord(toks, i, "UNION") || isWord(toks, i, "EXCEPT") || isWord(toks, i, "INTERSECT")
}

func isClauseWord(toks []token, i int) bool {
	return isWord(toks, i, "FROM") || isWord(toks, i, "WHERE") || isWord(toks, i, "GROUP") ||
		isWord(toks, i, "HAVING") || isWord(toks, i, "WINDOW") || isWord(toks, i, "ORDER") ||
		isWord(toks, i, "LIMIT") || isCompound(toks, i)
}

func isWord(toks []token, i int, word string) bool {
	if i < 0 || i >= len(toks) {
		return false
	}
	tok := toks[i]
	return tok.kind == tokIdent && !tok.quoted && strings.EqualFold(tok.text, word)
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
