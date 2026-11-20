package session

import (
	"testing"

	"github.com/loveRyujin/lantern/core/pyrt"
	"github.com/loveRyujin/lantern/core/pythonx"
	"github.com/loveRyujin/lantern/core/sandbox"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
	"github.com/loveRyujin/lantern/internal/seed"
)

func seededRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if _, err := seed.Run(t.Context(), seed.Options{Root: root, Seed: seed.DefaultSeed}); err != nil {
		t.Fatal(err)
	}
	return root
}

func errCode(err error) string {
	c, _ := tools.CodeOf(err)
	return c
}

// 记录里放 Session 并不持有的产物 ID：读了 Session 会得到 artifact_not_found，
// 所以超限和非 PNG 返回各自的分类码就说明没有去取字节。
func TestFetchRejectsOversizeAndUnsupportedBeforeReading(t *testing.T) {
	h, err := workspace.Open(t.Context(), seededRoot(t), "星禾")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	an := &Analysis{Runtime: pyrt.Runtime{}, MaxChartBytes: 100}
	defer an.Close()
	if _, err := an.open(t.Context(), h, "run-a"); err != nil {
		t.Fatal(err)
	}
	an.mu.Lock()
	an.runs["run-a"].charts = []Chart{
		{ArtifactID: "big", Name: "big.png", Kind: sandbox.KindPNG, Bytes: 101},
		{ArtifactID: "csv", Name: "x.csv", Kind: sandbox.ArtifactKind("csv"), Bytes: 1},
		{ArtifactID: "ok", Name: "ok.png", Kind: sandbox.KindPNG, Bytes: 10},
	}
	an.mu.Unlock()
	if _, err := an.Fetch("星禾", "run-a", "big"); errCode(err) != CodeChartTooLarge {
		t.Fatalf("超限 err=%v", err)
	}
	if _, err := an.Fetch("星禾", "run-a", "csv"); errCode(err) != CodeChartUnsupported {
		t.Fatalf("非 PNG err=%v", err)
	}
	if _, err := an.Fetch("星禾", "run-a", "ok"); errCode(err) != pythonx.CodeArtifact {
		t.Fatalf("对照组应读到 Session 并得到 artifact_not_found，err=%v", err)
	}
	an.MaxChartBytes = int(sandbox.MaxArtifactBytes) * 4
	if an.maxBytes() != int(sandbox.MaxArtifactBytes) {
		t.Fatalf("界面上限没有收回到沙箱上限：%d", an.maxBytes())
	}
}

func TestAskLeavesNoRunWhenAnalysisCannotOpen(t *testing.T) {
	root := seededRoot(t)
	an := &Analysis{}
	if err := an.Close(); err != nil {
		t.Fatal(err)
	}
	r := &LoopRunner{Root: root, Waiter: NewWaiter(), Analysis: an, Demo: true}
	if _, err := r.Ask(t.Context(), Ask{RunID: "run-x", WorkspaceID: "星禾", Prompt: "画图"}); err == nil {
		t.Fatal("分析已关闭时 Ask 应失败")
	}
	h, err := workspace.Open(t.Context(), root, "星禾")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if exists, err := runExists(t.Context(), h.Runs, "run-x"); err != nil || exists {
		t.Fatalf("工具准备失败后留下了运行行 exists=%v err=%v", exists, err)
	}
}
