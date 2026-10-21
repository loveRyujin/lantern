package approval

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	reInsertApproval = regexp.MustCompile(`(?is)insert\s+into\s+approvals\b`)
	reUpdateApproval = regexp.MustCompile(`(?is)update\s+approvals\b`)
	reSetDecision    = regexp.MustCompile(`(?is)\bset\s+decision\b`)
	reTouchDecision  = regexp.MustCompile(`(?is)update\s+approvals\b[^;]*\bdecision\b`)
	reTouchConsumed  = regexp.MustCompile(`(?is)update\s+approvals\b[^;]*\bconsumed_at\b`)
	reTouchState     = regexp.MustCompile(`(?is)update\s+approvals\b[^;]*\bstate\b`)
)

// TestNoDirectApprovalWritesOutsideStateMachine 只扫字符串常量和它们的拼接。
// fmt.Sprintf、变量和 builder 拼出来的 SQL 它看不见。这只是第一道过滤；
// 真正的保证是 #34 的只读连接，再加上文件哈希校验。
func TestNoDirectApprovalWritesOutsideStateMachine(t *testing.T) {
	root := moduleRoot(t)
	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "vendor" || name == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, "_test.go") {
			return nil
		}
		if !strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, ".sql") {
			return nil
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		if approvalWriteAllowed(root, resolved) {
			return nil
		}
		body, err := os.ReadFile(resolved)
		if err != nil {
			return err
		}
		found, err := approvalWrites(name, body)
		if err != nil {
			t.Fatalf("解析 %s: %v", resolved, err)
		}
		for _, hit := range found {
			rel, err := filepath.Rel(root, resolved)
			if err != nil {
				t.Fatal(err)
			}
			hits = append(hits, filepath.ToSlash(rel)+": "+hit)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Fatalf("状态机以外写了确认单:\n%s", strings.Join(hits, "\n"))
	}
}

func TestConcatenatedAndRawApprovalWritesAreFound(t *testing.T) {
	root := moduleRoot(t)
	dir, err := filepath.EvalSymlinks(filepath.Join(root, "core", "approval", "testdata", "writebypass"))
	if err != nil {
		t.Fatal(err)
	}
	bad, err := os.ReadFile(filepath.Join(dir, "bad.go"))
	if err != nil {
		t.Fatal(err)
	}
	hits, err := approvalWrites("bad.go", bad)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(hits, "\n")
	if !reInsertApproval.MatchString(joined) || !reTouchDecision.MatchString(joined) {
		t.Fatalf("拼接和多行原始 SQL 应被扫出，实际:\n%s", joined)
	}
	decoy, err := os.ReadFile(filepath.Join(dir, "decoy.go"))
	if err != nil {
		t.Fatal(err)
	}
	hits, err = approvalWrites("decoy.go", decoy)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("注释和半截字符串不应算写入: %v", hits)
	}
}

func TestApprovalWriteSQLUsesVersionCAS(t *testing.T) {
	root := moduleRoot(t)
	dir, err := filepath.EvalSymlinks(filepath.Join(root, "core", "approval"))
	if err != nil {
		t.Fatal(err)
	}
	updates := 0
	inserts := 0
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hits, err := approvalWrites(d.Name(), body)
		if err != nil {
			t.Fatalf("解析 %s: %v", path, err)
		}
		for _, hit := range hits {
			switch {
			case reUpdateApproval.MatchString(hit):
				updates++
				if !strings.Contains(strings.ToLower(hit), "version") || !strings.Contains(hit, "state = ?") {
					t.Fatalf("状态更新没有同时卡住 state 和 version:\n%s", hit)
				}
			case reInsertApproval.MatchString(hit):
				inserts++
			}
		}
		if reUpdateApproval.Match(body) && !bytesContains(body, "RowsAffected") {
			t.Fatalf("%s 更新了确认单却没有检查 RowsAffected", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updates != 1 || inserts != 1 {
		t.Fatalf("状态机里的写入应各有一处，更新 %d，插入 %d", updates, inserts)
	}
	machine, err := os.ReadFile(filepath.Join(dir, "machine.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(machine)
	if !strings.Contains(text, "RowsAffected") || !strings.Contains(text, "n != 1") {
		t.Fatal("条件更新没有要求恰好一行生效")
	}
	migration, err := filepath.EvalSymlinks(filepath.Join(root, "core", "workspace", "migrations", "runs", "003_approval_state.sql"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(migration)
	if err != nil {
		t.Fatal(err)
	}
	sqlText := string(body)
	if reInsertApproval.MatchString(sqlText) || reSetDecision.MatchString(sqlText) {
		t.Fatalf("回填迁移不能插入确认单，也不能 SET decision:\n%s", sqlText)
	}
}

func approvalWriteAllowed(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if rel == "core/workspace/migrations/runs/003_approval_state.sql" {
		return true
	}
	return strings.HasPrefix(rel, "core/approval/") && !strings.Contains(rel, "/testdata/") && !strings.HasSuffix(rel, "_test.go")
}

func approvalWrites(name string, body []byte) ([]string, error) {
	var texts []string
	if strings.HasSuffix(name, ".sql") {
		texts = []string{string(body)}
	} else {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, body, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			expr, ok := n.(ast.Expr)
			if !ok {
				return true
			}
			folded, ok := foldString(expr)
			if ok {
				texts = append(texts, folded)
			}
			return true
		})
	}
	var hits []string
	for _, text := range texts {
		if reInsertApproval.MatchString(text) || reTouchDecision.MatchString(text) || reTouchConsumed.MatchString(text) || reTouchState.MatchString(text) {
			hits = append(hits, text)
		}
	}
	return hits, nil
}

func foldString(expr ast.Expr) (string, bool) {
	switch n := expr.(type) {
	case *ast.BasicLit:
		if n.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(n.Value)
		if err != nil {
			return "", false
		}
		return s, true
	case *ast.BinaryExpr:
		if n.Op != token.ADD {
			return "", false
		}
		left, okL := foldString(n.X)
		right, okR := foldString(n.Y)
		if !okL || !okR {
			return "", false
		}
		return left + right, true
	case *ast.ParenExpr:
		return foldString(n.X)
	default:
		return "", false
	}
}

func bytesContains(body []byte, sub string) bool {
	return strings.Contains(string(body), sub)
}
