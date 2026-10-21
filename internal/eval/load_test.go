package eval_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/internal/eval"
)

func TestLoadSuiteYAMLAndJSONL(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "v1")
	writeFile(t, filepath.Join(root, "b.yaml"), `
cases:
  - id: b
    category: tool_call.mock
    input: {task: b}
    expected_behavior: 调用搜索
    judging_criteria:
      deterministic: ["tool_call:search_docs"]
      llm_judge: {required: false}
`)
	writeFile(t, filepath.Join(root, "a.jsonl"), `{"id":"a","category":"document_poisoning","input":{"task":"a"},"expected":"不泄露","rule":"sensitive_plaintext_count:0"}`+"\n\n")
	writeFile(t, filepath.Join(root, "rules.yaml"), "rules: [tool_call]\nnot a case\n")
	writeFile(t, filepath.Join(root, "gates.yaml"), "version: v1\nnot a case\n")
	writeFile(t, filepath.Join(root, "notes.md"), "ignore\n")

	suite, err := eval.LoadSuite(root, filepath.Join(root, "rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if suite.Version != "v1" {
		t.Fatalf("版本 = %s", suite.Version)
	}
	if len(suite.Cases) != 2 || suite.Cases[0].ID != "a" || suite.Cases[1].ID != "b" {
		t.Fatalf("用例顺序 = %+v", idsOf(suite))
	}
	if suite.Cases[0].Expected != "不泄露" || suite.Cases[0].Rules[0] != "sensitive_plaintext_count:0" {
		t.Fatalf("JSONL 别名没有读入: %+v", suite.Cases[0])
	}
	if suite.Cases[1].Judge.Required {
		t.Fatal("required false 被读成 true")
	}
}

func TestLoadSuiteRejectsDuplicateAndMissingFields(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "one.yaml"), "id: same\ncategory: tools\ninput: {task: a}\nexpected_behavior: x\nrule: \"tool_call:search_docs\"\n")
	writeFile(t, filepath.Join(root, "two.jsonl"), `{"id":"same","category":"tools","input":{"task":"b"},"expected_behavior":"x","rule":"tool_call:search_docs"}`+"\n")
	if _, err := eval.LoadSuite(root, ""); err == nil {
		t.Fatal("重复 id 应失败")
	}

	bad := t.TempDir()
	writeFile(t, filepath.Join(bad, "bad.yaml"), "id: x\ncategory: tools\ninput: {task: a}\nrule: \"tool_call:search_docs\"\n")
	_, err := eval.LoadSuite(bad, "")
	if err == nil || !strings.Contains(err.Error(), "expected_behavior") {
		t.Fatalf("缺少 expected_behavior 应失败，err = %v", err)
	}
}

func TestLoadSuiteInfersSandboxPlatform(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "sandbox", "linux", "escape.yaml"), `
id: sb
category: sandbox
input: {task: curl}
expected_behavior: 拦截
rule: "sandbox_blocked:true"
`)
	suite, err := eval.LoadSuite(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(suite.Cases) != 1 || suite.Cases[0].Platform != "linux" {
		t.Fatalf("平台 = %+v", suite.Cases)
	}

	conflict := t.TempDir()
	writeFile(t, filepath.Join(conflict, "sandbox", "darwin", "bad.yaml"), `
id: sb
category: sandbox
platform: linux
input: {task: curl}
expected_behavior: 拦截
rule: "sandbox_blocked:true"
`)
	if _, err := eval.LoadSuite(conflict, ""); err == nil {
		t.Fatal("路径平台与字段不一致应失败")
	}
}

func idsOf(suite eval.Suite) []string {
	out := make([]string, len(suite.Cases))
	for i, item := range suite.Cases {
		out[i] = item.ID
	}
	return out
}
