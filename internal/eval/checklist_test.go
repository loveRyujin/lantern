package eval_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	if got := digestOfRealReport(t, res.Report); got != res.Report.Digest {
		t.Fatalf("生产摘要指纹应按真实报告计算，报告 %s，重算 %s", res.Report.Digest, got)
	}
	got, err := frozenPlatformAllDigest(res.Report)
	if err != nil {
		t.Fatal(err)
	}
	if got != platformAllSummaryDigest {
		t.Fatalf("--platform all 冻结摘要指纹 = %s，现有输出是 %s", got, platformAllSummaryDigest)
	}
}

func digestOfRealReport(t *testing.T, report eval.Report) string {
	t.Helper()
	report.GeneratedAt = ""
	report.Digest = ""
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// digestSummary 对齐既有 --platform all 报告的 Summary。后来加上的 scored 不进这道冻结指纹。
type digestSummary struct {
	Cases              int      `json:"cases"`
	Passed             int      `json:"passed"`
	Failed             int      `json:"failed"`
	MissingObservation int      `json:"missing_observation"`
	Skipped            int      `json:"skipped"`
	Filtered           int      `json:"filtered"`
	PassRate           *float64 `json:"pass_rate"`
	HardGatesPassed    bool     `json:"hard_gates_passed"`
}

type digestReport struct {
	GeneratedAt      string                 `json:"generated_at"`
	Digest           string                 `json:"digest"`
	Mode             string                 `json:"mode"`
	Category         string                 `json:"category"`
	SelectedPlatform string                 `json:"selected_platform"`
	SuiteVersion     string                 `json:"suite_version"`
	Note             string                 `json:"note"`
	Environment      eval.Environment       `json:"environment"`
	RuleCatalog      eval.CatalogInfo       `json:"rule_catalog"`
	GateCatalog      eval.GateInfo          `json:"gate_catalog"`
	Summary          digestSummary          `json:"summary"`
	HardGates        []eval.HardGate        `json:"hard_gates"`
	Metrics          []eval.Metric          `json:"metrics"`
	Sandbox          []eval.SandboxPlatform `json:"sandbox"`
	Cases            []eval.CaseResult      `json:"cases"`
}

func frozenPlatformAllDigest(report eval.Report) (string, error) {
	view := digestReport{
		Mode:             report.Mode,
		Category:         report.Category,
		SelectedPlatform: report.SelectedPlatform,
		SuiteVersion:     report.SuiteVersion,
		Note:             report.Note,
		Environment:      report.Environment,
		RuleCatalog:      report.RuleCatalog,
		GateCatalog:      report.GateCatalog,
		Summary: digestSummary{
			Cases:              report.Summary.Cases,
			Passed:             report.Summary.Passed,
			Failed:             report.Summary.Failed,
			MissingObservation: report.Summary.MissingObservation,
			Skipped:            report.Summary.Skipped,
			Filtered:           report.Summary.Filtered,
			PassRate:           report.Summary.PassRate,
			HardGatesPassed:    report.Summary.HardGatesPassed,
		},
		HardGates: report.HardGates,
		Metrics:   report.Metrics,
		Sandbox:   report.Sandbox,
		Cases:     report.Cases,
	}
	// 只在测试里换回既有 --platform all 输出里的环境，用来对照 de135f6e。
	// 生产摘要指纹不替换这些字段。
	view.Environment.OS = "linux"
	view.Environment.Arch = "amd64"
	view.Environment.Platform = "linux"
	view.Environment.GoVersion = "go1.27.1"
	view.Environment.Commit = "a954fd599109281be9b71b6bef89a950741c11bc"
	view.Environment.SuiteRoot = "/workspace/eval/v1"
	view.RuleCatalog.Path = "/workspace/eval/v1/rules.yaml"
	view.GateCatalog.Path = "/workspace/eval/v1/gates.yaml"
	raw, err := json.Marshal(view)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
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
