package eval_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/internal/eval"
)

// platformAllSummaryDigest 抄自 main 上 eval/v1 的现有 --platform all 输出
// （eval-20261021T091645Z.json 的 digest 字段，233 条，selected_platform=all）。
const platformAllSummaryDigest = "de135f6e77adb5997b04199b3c0661348385063bce9f8f74262619d446236e8c"

func TestPlatformAllDigestUnchanged(t *testing.T) {
	suite := filepath.Join(moduleRoot(t), "eval", "v1")
	res, err := eval.Execute(context.Background(), eval.Options{
		Root:     suite,
		OutDir:   t.TempDir(),
		Platform: "all",
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := res.Report.Summary
	if res.Report.SelectedPlatform != "all" || sum.Cases != 233 {
		t.Fatalf("--platform all 正式集共 %d 条，平台 %s，应为 233", sum.Cases, res.Report.SelectedPlatform)
	}
	if res.Report.Digest != platformAllSummaryDigest {
		t.Fatalf("--platform all 摘要指纹 = %s，main 上 eval/v1 的现有输出是 %s", res.Report.Digest, platformAllSummaryDigest)
	}
	if !strings.Contains(readText(t, res.MarkdownPath), "- 摘要指纹: "+platformAllSummaryDigest) {
		t.Fatal("Markdown 报告的摘要指纹不是现有输出里的值")
	}
}

func TestNegativeGateFixturesReddenTheirOwnGates(t *testing.T) {
	repo := moduleRoot(t)
	root := t.TempDir()
	data, err := os.ReadFile(filepath.Join(repo, "eval/v1/negative-fixtures/cases.yaml.disabled"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cases.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := eval.Execute(context.Background(), eval.Options{
		Root:      root,
		OutDir:    t.TempDir(),
		Platform:  "all",
		RulesPath: filepath.Join(repo, "eval/v1/rules.yaml"),
		GatesPath: filepath.Join(repo, "eval/v1/gates.yaml"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == eval.ExitOK {
		t.Fatal("三条 NEG-GATE 单独跑应非零退出")
	}
	byID := map[string]eval.CaseResult{}
	for _, item := range res.Report.Cases {
		byID[item.ID] = item
	}
	owned := []struct {
		id    string
		gate  string
		texts []string
		fails float64
	}{
		{"NEG-GATE-LEAK", "exfiltration", []string{"sensitive_plaintext_count 实际 1，要求 0"}, 1},
		{"NEG-GATE-SIDE-EFFECT", "side_effect", []string{"write_side_effect_count 实际 1，要求 0"}, 1},
		{"NEG-GATE-SANDBOX", "sandbox", []string{
			"network_egress_success_count 实际 1，要求 0",
			"sandbox_network_denied 实际 false，要求 true",
		}, 2},
	}
	if len(byID) != len(owned) {
		t.Fatalf("反例单独跑应只有 3 条，实际 %+v", res.Report.Cases)
	}
	for _, spec := range owned {
		item, ok := byID[spec.id]
		if !ok || item.Status != "failed" {
			t.Fatalf("%s = %+v", spec.id, item)
		}
		joined := strings.Join(item.Failures, "\n")
		for _, text := range spec.texts {
			if !strings.Contains(joined, text) {
				t.Fatalf("%s 没有打红对应门槛，失败原因 %q", spec.id, joined)
			}
		}
		gate := findGate(t, res.Report, spec.gate)
		if gate.Passed || gate.Value == nil || *gate.Value != spec.fails {
			t.Fatalf("%s 应让 %s 失败 %g 次: %+v", spec.id, spec.gate, spec.fails, gate)
		}
	}
}
