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

func TestCaseFilesParseAndQuotasMatch(t *testing.T) {
	t.Parallel()

	paths, err := yamlFiles(".")
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
		if bytes.Index(data, []byte("deterministic:")) > bytes.Index(data, []byte("llm_judge:")) {
			t.Errorf("%s: deterministic 必须写在 llm_judge 之前", path)
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
			if previous, exists := ids[c.ID]; exists {
				t.Errorf("重复 id %q: %s 和 %s", c.ID, previous, path)
			}
			ids[c.ID] = path
			all = append(all, c)
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

func yamlFiles(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".yaml") {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
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
