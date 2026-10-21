package evalv1_test

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type caseFile struct {
	Cases []evalCase `yaml:"cases"`
}

type evalCase struct {
	ID               string         `yaml:"id"`
	Category         string         `yaml:"category"`
	Input            map[string]any `yaml:"input"`
	ExpectedBehavior string         `yaml:"expected_behavior"`
	JudgingCriteria  judging        `yaml:"judging_criteria"`
}

type judging struct {
	Deterministic []string `yaml:"deterministic"`
	LLMJudge      llmJudge `yaml:"llm_judge"`
}

type llmJudge struct {
	Required bool   `yaml:"required"`
	Model    string `yaml:"model,omitempty"`
	Rubric   string `yaml:"rubric,omitempty"`
}

type ruleFile struct {
	Rules []ruleDefinition `yaml:"rules"`
}

type ruleDefinition struct {
	Name        string          `yaml:"name"`
	Description string          `yaml:"description"`
	Parameters  []ruleParameter `yaml:"parameters"`
	Kind        string          `yaml:"kind"`
}

type ruleParameter struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"`
	Description string `yaml:"description"`
}

func TestCaseFilesParseAndQuotasMatch(t *testing.T) {
	t.Parallel()

	rules := loadRuleCatalog(t, "rules.yaml")
	usedRules := make(map[string]bool, len(rules))

	paths, err := caseYAMLFiles(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 {
		t.Fatalf("YAML 文件数 = %d，期望 3", len(paths))
	}

	var all []evalCase
	ids := make(map[string]string)
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("读取 %s: %v", path, readErr)
		}

		var file caseFile
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if decodeErr := decoder.Decode(&file); decodeErr != nil {
			t.Fatalf("解析 %s: %v", path, decodeErr)
		}
		if len(file.Cases) == 0 {
			t.Errorf("%s: cases 不能为空", path)
		}

		for i, c := range file.Cases {
			validateCase(t, path, i, c)
			validateRuleReferences(t, path, i, c, rules, usedRules)
			if previous, exists := ids[c.ID]; exists {
				t.Errorf("重复 id %q: %s 和 %s", c.ID, previous, path)
			}
			ids[c.ID] = path
			all = append(all, c)
		}
	}

	for name := range rules {
		if !usedRules[name] {
			t.Errorf("rules.yaml 中的规则 %q 未被任何用例使用", name)
		}
	}

	assertQuota(t, all, "document_poisoning", 10)
	assertQuota(t, all, "tool_output", 8)
	assertQuota(t, all, "user_inducement", 6)
	assertQuota(t, all, "code_egress_external", 3)
	assertQuota(t, all, "code_egress_loopback", 3)
	assertQuota(t, all, "benign_control", 5)
	assertQuota(t, all, "tool_call.live", 25)
	assertQuota(t, all, "tool_call.mock", 15)
	assertPrefixQuota(t, all, "workspace_isolation.", 8)

	if got := len(all); got != 83 {
		t.Fatalf("用例总数 = %d，期望 83", got)
	}
}

func caseYAMLFiles(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// rules.yaml 是规则清单，gates.yaml 是硬门槛，都不是用例文件。
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			return nil
		}
		switch entry.Name() {
		case "rules.yaml", "gates.yaml":
			return nil
		default:
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}

func loadRuleCatalog(t *testing.T, path string) map[string]ruleDefinition {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}

	var file ruleFile
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}
	if len(file.Rules) == 0 {
		t.Fatalf("%s: rules 不能为空", path)
	}

	rules := make(map[string]ruleDefinition, len(file.Rules))
	for i, rule := range file.Rules {
		where := fmt.Sprintf("%s rules[%d]", path, i)
		if rule.Name == "" || rule.Description == "" || len(rule.Parameters) == 0 {
			t.Errorf("%s: name、description、parameters 都必须非空", where)
		}
		if strings.Contains(rule.Description, "\n") {
			t.Errorf("%s: description 必须为单行", where)
		}
		if rule.Kind != "deterministic" && rule.Kind != "llm_judge" {
			t.Errorf("%s: kind %q 无效", where, rule.Kind)
		}
		for parameterIndex, parameter := range rule.Parameters {
			if parameter.Name == "" || parameter.Type == "" || parameter.Description == "" {
				t.Errorf("%s parameters[%d]: name、type、description 都必须非空", where, parameterIndex)
			}
		}
		if _, exists := rules[rule.Name]; exists {
			t.Errorf("%s: 重复规则名 %q", where, rule.Name)
		}
		rules[rule.Name] = rule
	}
	return rules
}

func validateRuleReferences(
	t *testing.T,
	path string,
	index int,
	c evalCase,
	rules map[string]ruleDefinition,
	usedRules map[string]bool,
) {
	t.Helper()
	where := fmt.Sprintf("%s cases[%d]", path, index)
	for _, assertion := range c.JudgingCriteria.Deterministic {
		name, value, found := strings.Cut(assertion, ":")
		if !found || name == "" || value == "" {
			t.Errorf("%s: 判定规则 %q 必须使用 name:value 格式", where, assertion)
			continue
		}
		rule, exists := rules[name]
		if !exists {
			t.Errorf("%s: 未知判定规则 %q", where, name)
			continue
		}
		if rule.Kind != "deterministic" {
			t.Errorf("%s: 规则 %q 的 kind = %q，期望 deterministic", where, name, rule.Kind)
		}
		usedRules[name] = true
	}

	rule, exists := rules["llm_judge"]
	if !exists {
		t.Errorf("%s: rules.yaml 缺少 llm_judge 规则", where)
		return
	}
	if rule.Kind != "llm_judge" {
		t.Errorf("%s: llm_judge 的 kind = %q", where, rule.Kind)
	}
	usedRules["llm_judge"] = true
}

func validateCase(t *testing.T, path string, index int, c evalCase) {
	t.Helper()
	where := fmt.Sprintf("%s cases[%d]", path, index)
	if c.ID == "" || c.Category == "" || len(c.Input) == 0 || c.ExpectedBehavior == "" {
		t.Errorf("%s: id、category、input、expected_behavior 都必须非空", where)
	}
	if len(c.JudgingCriteria.Deterministic) == 0 {
		t.Errorf("%s: 至少需要一条 deterministic 判定规则", where)
	}
	if c.JudgingCriteria.LLMJudge.Required {
		if c.JudgingCriteria.LLMJudge.Model != "gemini-4-argon" {
			t.Errorf("%s: LLM judge 模型 = %q，期望 gemini-4-argon", where, c.JudgingCriteria.LLMJudge.Model)
		}
		if c.JudgingCriteria.LLMJudge.Rubric == "" {
			t.Errorf("%s: 需要 LLM judge 时 rubric 不能为空", where)
		}
	} else if c.JudgingCriteria.LLMJudge.Model != "" || c.JudgingCriteria.LLMJudge.Rubric != "" {
		t.Errorf("%s: 不需要 LLM judge 时不能设置 model 或 rubric", where)
	}
}

func assertQuota(t *testing.T, cases []evalCase, category string, want int) {
	t.Helper()
	got := 0
	for _, c := range cases {
		if c.Category == category {
			got++
		}
	}
	if got != want {
		t.Errorf("%s 配额 = %d，期望 %d", category, got, want)
	}
}

func assertPrefixQuota(t *testing.T, cases []evalCase, prefix string, want int) {
	t.Helper()
	got := 0
	for _, c := range cases {
		if strings.HasPrefix(c.Category, prefix) {
			got++
		}
	}
	if got != want {
		t.Errorf("%s* 配额 = %d，期望 %d", prefix, got, want)
	}
}
