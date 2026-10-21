package seed_test

import (
	"bytes"
	"fmt"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

const seedPkgPath = "github.com/loveRyujin/lantern/internal/seed"

// TestRecordExportApprovalOnlySeedCallers 用类型信息找 RecordExportApproval 的引用。
// 目标是 internal/seed 里的那个 *types.Func，不是同名的别的函数。
// 非测试代码里，除了 internal/seed，不能引用它。
func TestRecordExportApprovalOnlySeedCallers(t *testing.T) {
	root := lanternRoot(t)

	t.Run("repo", func(t *testing.T) {
		sites := recordExportApprovalUses(t, root)
		var outside []string
		inside := 0
		for _, site := range sites {
			if site.seedPkg {
				inside++
				continue
			}
			outside = append(outside, site.String())
		}
		if len(outside) > 0 {
			t.Fatalf("RecordExportApproval used outside internal/seed:\n%s", strings.Join(outside, "\n"))
		}
		if inside == 0 {
			t.Fatal("internal/seed 里没有引用 RecordExportApproval，扫描可能没有生效")
		}
	})

	t.Run("testdata", func(t *testing.T) {
		dir := filepath.Join(root, "internal", "seed", "testdata", "outsidecall")
		assertOutsideUses(t, recordExportApprovalUses(t, dir), []string{
			"alias.go", "assigned.go", "direct.go",
		})
	})

	// 三段内联源码：直接调用、赋给变量、包起别名后再调用。
	// 另外一段把函数值当参数传出去。扫描器对这几处都要报出来。
	t.Run("inline", func(t *testing.T) {
		sites := recordExportApprovalUses(t, writeInlineModule(t, root, map[string]string{
			"direct.go":   inlineDirect,
			"assigned.go": inlineAssigned,
			"alias.go":    inlineAlias,
			"param.go":    inlineParam,
		}))
		assertOutsideUses(t, sites, []string{
			"alias.go", "assigned.go", "direct.go", "param.go",
		})
	})
}

func assertOutsideUses(t *testing.T, sites []approvalUse, want []string) {
	t.Helper()
	got := map[string]int{}
	for _, site := range sites {
		if site.seedPkg {
			continue
		}
		got[site.rel]++
	}
	var files []string
	for rel, n := range got {
		if n != 1 {
			t.Fatalf("%s uses = %d, want 1", rel, n)
		}
		files = append(files, rel)
	}
	slices.Sort(files)
	if !slices.Equal(files, want) {
		t.Fatalf("uses = %v, want %v", files, want)
	}
}

// 内联反例都在 internal/seed 以外。同名的本地函数不算引用。
const inlineDirect = `package outside

import (
	"context"

	"github.com/loveRyujin/lantern/core/workspace"
	"github.com/loveRyujin/lantern/internal/seed"
)

func direct(ctx context.Context, h *workspace.Handle) error {
	return seed.RecordExportApproval(ctx, h, "id", "user_contacts")
}

func RecordExportApproval() {}

func callLocal() {
	RecordExportApproval()
}
`

const inlineAssigned = `package outside

import (
	"context"

	"github.com/loveRyujin/lantern/core/workspace"
	"github.com/loveRyujin/lantern/internal/seed"
)

func assigned(ctx context.Context, h *workspace.Handle) error {
	fn := seed.RecordExportApproval
	return fn(ctx, h, "id", "user_contacts")
}
`

const inlineAlias = `package outside

import (
	"context"

	"github.com/loveRyujin/lantern/core/workspace"
	alias "github.com/loveRyujin/lantern/internal/seed"
)

func viaAlias(ctx context.Context, h *workspace.Handle) error {
	return alias.RecordExportApproval(ctx, h, "id", "user_contacts")
}
`

const inlineParam = `package outside

import (
	"context"

	"github.com/loveRyujin/lantern/core/workspace"
	"github.com/loveRyujin/lantern/internal/seed"
)

func accept(fn func(context.Context, *workspace.Handle, string, string) error) {
	_ = fn
}

func passed() {
	accept(seed.RecordExportApproval)
}
`

func writeInlineModule(t *testing.T, root string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	fixture := filepath.Join(root, "internal", "seed", "testdata", "outsidecall")
	mod, err := os.ReadFile(filepath.Join(fixture, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(mod),
		"module github.com/loveRyujin/lantern/outsidecall",
		"module github.com/loveRyujin/lantern/outsidecallinline", 1)
	text = strings.Replace(text,
		"replace github.com/loveRyujin/lantern => ../../../..",
		"replace github.com/loveRyujin/lantern => "+root, 1)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := os.ReadFile(filepath.Join(fixture, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// approvalUse 是 types.Info.Uses 里指向种子 RecordExportApproval 的一处标识符。
type approvalUse struct {
	pkgPath string
	rel     string
	line    int
	seedPkg bool
}

func (u approvalUse) String() string {
	return fmt.Sprintf("%s:%d (%s)", u.rel, u.line, u.pkgPath)
}

// recordExportApprovalUses 加载 dir 模块里的非测试包，用 types.Info.Uses
// 找出对 internal/seed.RecordExportApproval 这个 *types.Func 的全部引用。
// 不限于函数调用：赋给变量、当参数传出去、包起别名后再调用，都是同一对象的 Use。
func recordExportApprovalUses(t *testing.T, dir string) []approvalUse {
	t.Helper()
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedCompiledGoFiles |
			packages.NeedImports |
			packages.NeedDeps |
			packages.NeedTypes |
			packages.NeedSyntax |
			packages.NeedTypesInfo |
			packages.NeedModule,
		Dir:   dir,
		Env:   envWithCGODisabled(),
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("packages.Load 没有返回包")
	}
	if msg := packageErrors(pkgs); msg != "" {
		t.Fatalf("类型检查失败:\n%s", msg)
	}

	var target *types.Func
	packages.Visit(pkgs, func(p *packages.Package) bool {
		if p.Types == nil || p.Types.Path() != seedPkgPath {
			return true
		}
		fn, _ := p.Types.Scope().Lookup("RecordExportApproval").(*types.Func)
		if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != seedPkgPath {
			return true
		}
		target = fn
		return true
	}, nil)
	if target == nil {
		t.Fatal("没找到 internal/seed.RecordExportApproval")
	}

	var sites []approvalUse
	seen := map[string]bool{}
	packages.Visit(pkgs, func(p *packages.Package) bool {
		if p.TypesInfo == nil {
			return true
		}
		base := dir
		if p.Module != nil && p.Module.Dir != "" {
			base = p.Module.Dir
		}
		for ident, obj := range p.TypesInfo.Uses {
			fn, ok := obj.(*types.Func)
			if !ok || fn != target {
				continue
			}
			pos := p.Fset.Position(ident.Pos())
			if strings.HasSuffix(pos.Filename, "_test.go") {
				continue
			}
			key := pos.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			rel, err := filepath.Rel(base, pos.Filename)
			if err != nil {
				t.Fatal(err)
			}
			rel = filepath.ToSlash(rel)
			path := p.PkgPath
			sites = append(sites, approvalUse{
				pkgPath: path,
				rel:     rel,
				line:    pos.Line,
				seedPkg: path == seedPkgPath || strings.HasPrefix(path, seedPkgPath+"/"),
			})
		}
		return true
	}, nil)
	return sites
}

func packageErrors(pkgs []*packages.Package) string {
	var b strings.Builder
	packages.Visit(pkgs, func(p *packages.Package) bool {
		for _, err := range p.Errors {
			fmt.Fprintf(&b, "%s: %v\n", p.PkgPath, err)
		}
		return true
	}, nil)
	return b.String()
}

func lanternRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && bytes.HasPrefix(data, []byte("module github.com/loveRyujin/lantern\n")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("找不到 lantern 模块")
		}
		dir = parent
	}
}

func envWithCGODisabled() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, "CGO_ENABLED=") {
			continue
		}
		out = append(out, e)
	}
	return append(out, "CGO_ENABLED=0")
}
