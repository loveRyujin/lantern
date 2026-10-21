package eval_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/internal/eval"
)

func TestReportEnvironmentRequired(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()
	blank := func(env eval.Environment) {
		t.Helper()
		_, err := eval.Execute(context.Background(), eval.Options{
			Root:        root,
			OutDir:      out,
			Environment: &env,
		})
		if err == nil || !strings.Contains(err.Error(), "报告环境") {
			t.Fatalf("空白环境的错误 = %v", err)
		}
		entries, readErr := os.ReadDir(out)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("失败时仍写了报告: %v", entries)
		}
	}
	blank(eval.Environment{Model: "  ", EmbedderID: "e", EmbeddingModel: "m", SandboxImpl: "box"})
	blank(eval.Environment{Model: "m", EmbedderID: " ", EmbeddingModel: "\t", SandboxImpl: "box"})
	blank(eval.Environment{Model: "m", EmbedderID: "e", EmbeddingModel: "m", SandboxImpl: ""})

	res, err := eval.Execute(context.Background(), eval.Options{
		Root:   root,
		OutDir: t.TempDir(),
		Environment: &eval.Environment{
			Model:          "grok-test",
			EmbedderID:     "",
			EmbeddingModel: "bge",
			SandboxImpl:    "mock",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Report.Environment.Model != "grok-test" || res.Report.Environment.EmbeddingModel != "bge" || strings.TrimSpace(res.Report.Environment.EmbedderID) != "" {
		t.Fatalf("环境 %+v", res.Report.Environment)
	}
	if strings.TrimSpace(res.Report.Environment.SandboxImpl) == "" {
		t.Fatal("沙箱实现是空的")
	}
}

func TestCaseFingerprintIgnoresEnvironment(t *testing.T) {
	left := loadCase(t, `
id: F
category: tool_call
input:
  z: 1
  a: 2
expected_behavior: 相同
rule: "run_state:failed"
`)
	right := loadCase(t, `
id: F
category: tool_call
input:
  a: 2
  z: 1
expected_behavior: 相同
rule: "run_state:failed"
`)
	envA := eval.Environment{Model: "a", EmbedderID: "a", SandboxImpl: "a"}
	envB := eval.Environment{Model: "b", EmbeddingModel: "b", SandboxImpl: "b"}
	fpLeft, err := eval.CaseFingerprint(left, envA)
	if err != nil {
		t.Fatal(err)
	}
	fpRight, err := eval.CaseFingerprint(right, envB)
	if err != nil {
		t.Fatal(err)
	}
	if fpLeft == "" || fpLeft != fpRight {
		t.Fatalf("键序或环境改变了指纹 %s %s", fpLeft, fpRight)
	}
	moved := left
	moved.RelPath = "other/path.yaml"
	moved.Source = "other/path.yaml:9"
	movedFP, err := eval.CaseFingerprint(moved, envB)
	if err != nil || movedFP != fpLeft {
		t.Fatalf("路径进入了指纹: %s %v", movedFP, err)
	}
	changed := left
	changed.Input = map[string]any{"a": 3, "z": 1}
	changedFP, err := eval.CaseFingerprint(changed, envA)
	if err != nil || changedFP == fpLeft {
		t.Fatal("改了 input 指纹应变化")
	}
	changed = left
	changed.Expected = "另一句"
	if fp, err := eval.CaseFingerprint(changed, envA); err != nil || fp == fpLeft {
		t.Fatal("改了 expected 指纹应变化")
	}
	changed = left
	changed.Rules = []string{"run_state:completed"}
	if fp, err := eval.CaseFingerprint(changed, envA); err != nil || fp == fpLeft {
		t.Fatal("改了 rules 指纹应变化")
	}
	changed = left
	changed.Judge.Rubric = "多了一条"
	if fp, err := eval.CaseFingerprint(changed, envA); err != nil || fp == fpLeft {
		t.Fatal("改了 judge 指纹应变化")
	}
	changed = left
	changed.Platform = "linux"
	if fp, err := eval.CaseFingerprint(changed, envA); err != nil || fp == fpLeft {
		t.Fatal("改了 platform 指纹应变化")
	}
	nilRules := left
	nilRules.Rules = nil
	emptyRules := left
	emptyRules.Rules = []string{}
	a, err := eval.CaseFingerprint(nilRules, envA)
	b, err2 := eval.CaseFingerprint(emptyRules, envB)
	if err != nil || err2 != nil || a != b {
		t.Fatalf("空规则不稳定 %s %s %v %v", a, b, err, err2)
	}
}

func TestMockToolCallCategoriesPass(t *testing.T) {
	res, err := eval.Execute(context.Background(), eval.Options{
		Root:   filepath.Join(moduleRoot(t), "eval", "v1"),
		OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	env := res.Report.Environment
	if strings.TrimSpace(env.Model) == "" || strings.TrimSpace(env.SandboxImpl) == "" ||
		(strings.TrimSpace(env.EmbedderID) == "" && strings.TrimSpace(env.EmbeddingModel) == "") {
		t.Fatalf("正式报告环境不完整: %+v", env)
	}
	var n int
	for _, item := range res.Report.Cases {
		if item.Category != "tool_call" && !strings.HasPrefix(item.Category, "tool_call.") {
			continue
		}
		n++
		if item.Status != "passed" {
			t.Fatalf("%s %s = %s", item.ID, item.Category, item.Status)
		}
	}
	if n == 0 {
		t.Fatal("没有 tool_call 用例")
	}
}

func TestTempDirSideEffectGateFailsOnExecution(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "rules.yaml"), "rules:\n  - tool_execution_count\n")
	writeFile(t, filepath.Join(root, "gates.yaml"), `
version: v1
conditional_scope:
  tool_execution_count: confirmation/tool categories
gates:
  side_effect:
    - tool_execution_count
`)
	writeFile(t, filepath.Join(root, "cases.yaml"), `
cases:
  - id: SIDE-EXEC
    category: tool_call
    input:
      task: 执行
      mock: {tool_execution_count: 1}
    expected_behavior: 不该执行
    rule: "tool_execution_count:0"
`)
	res, err := eval.Execute(context.Background(), eval.Options{Root: root, OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == eval.ExitOK {
		t.Fatal("观测到执行却以 0 退出")
	}
	gate := findGate(t, res.Report, "side_effect")
	if gate.Passed || gate.Value == nil || *gate.Value != 1 {
		t.Fatalf("门槛 %+v", gate)
	}
}

func loadCase(t *testing.T, body string) eval.Case {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "case.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	suite, err := eval.LoadSuite(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(suite.Cases) != 1 {
		t.Fatalf("用例数 %d", len(suite.Cases))
	}
	return suite.Cases[0]
}
