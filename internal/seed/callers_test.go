package seed_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecordExportApprovalOnlySeedCallers 扫描仓库里所有非 _test.go 文件，
// 确认除了 internal/seed 之外，没有地方调用 RecordExportApproval。
func TestRecordExportApprovalOnlySeedCallers(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var outside []string
	inside := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "vendor" {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("解析 %s: %w", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		allowed := strings.HasPrefix(rel, "internal/seed/")
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !callsRecordExportApproval(call.Fun) {
				return true
			}
			if allowed {
				inside++
				return true
			}
			pos := fset.Position(call.Lparen)
			outside = append(outside, fmt.Sprintf("%s:%d", rel, pos.Line))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(outside) > 0 {
		t.Fatalf("RecordExportApproval called outside internal/seed:\n%s", strings.Join(outside, "\n"))
	}
	if inside == 0 {
		t.Fatal("internal/seed 里没有调用 RecordExportApproval，扫描可能没有生效")
	}
}

func callsRecordExportApproval(fun ast.Expr) bool {
	for {
		paren, ok := fun.(*ast.ParenExpr)
		if !ok {
			break
		}
		fun = paren.X
	}
	switch fun := fun.(type) {
	case *ast.Ident:
		return fun.Name == "RecordExportApproval"
	case *ast.SelectorExpr:
		return fun.Sel.Name == "RecordExportApproval"
	default:
		return false
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("找不到 go.mod")
		}
		dir = parent
	}
}
