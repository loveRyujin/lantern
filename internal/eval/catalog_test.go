package eval_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/loveRyujin/lantern/internal/eval"
)

func TestParseDeterministicRuleNotForm(t *testing.T) {
	t.Parallel()
	key, value, err := eval.ParseDeterministicRule("  tool_call_not:export_data  ")
	if err != nil {
		t.Fatal(err)
	}
	if key != "tool_call_not" || value != "export_data" {
		t.Fatalf("解析结果 = %s %s", key, value)
	}
	if !eval.IsNotForm(key) {
		t.Fatal("tool_call_not 应识别为 _not 形式")
	}
	if eval.IsNotForm("tool_call") || eval.IsNotForm("_not") || eval.IsNotForm("not") {
		t.Fatal("普通键不应识别为 _not 形式")
	}
	if _, _, err := eval.ParseDeterministicRule("no-colon"); err == nil {
		t.Fatal("缺少冒号的规则应解析失败")
	}
}

func TestLoadRuleCatalogShapes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	listPath := filepath.Join(dir, "list.yaml")
	if err := os.WriteFile(listPath, []byte("version: v1\nrules:\n  - tool_call\n  - tool_call_not\n  - name: sensitive_plaintext_count\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := eval.LoadRuleCatalog(listPath)
	if err != nil {
		t.Fatal(err)
	}
	if list.Version != "v1" || !list.Has("tool_call_not") || !list.Has("sensitive_plaintext_count") {
		t.Fatalf("列表清单 = %+v", list.Keys)
	}
	if list.Has("export_data") {
		t.Fatal("期望值不应被当成规则键")
	}

	keysPath := filepath.Join(dir, "keys.yaml")
	if err := os.WriteFile(keysPath, []byte("keys:\n  - cross_workspace_leak_count\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keys, err := eval.LoadRuleCatalog(keysPath)
	if err != nil {
		t.Fatal(err)
	}
	if !keys.Has("cross_workspace_leak_count") {
		t.Fatalf("keys 清单 = %+v", keys.Keys)
	}

	mapPath := filepath.Join(dir, "map.yaml")
	if err := os.WriteFile(mapPath, []byte("rules:\n  tool_call: {}\n  tool_call_not:\n    description: 禁止调用\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mapped, err := eval.LoadRuleCatalog(mapPath)
	if err != nil {
		t.Fatal(err)
	}
	if !mapped.Has("tool_call") || !mapped.Has("tool_call_not") {
		t.Fatalf("映射清单 = %+v", mapped.Keys)
	}

	dupPath := filepath.Join(dir, "dup.yaml")
	if err := os.WriteFile(dupPath, []byte("rules:\n  - tool_call\n  - tool_call\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := eval.LoadRuleCatalog(dupPath); err == nil {
		t.Fatal("重复规则键应拒绝")
	}
}

func TestCatalogDoesNotImplyNotForm(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.yaml")
	if err := os.WriteFile(path, []byte("- tool_call\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cat, err := eval.LoadRuleCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cat.Has("tool_call") || cat.Has("tool_call_not") {
		t.Fatal("清单只应包含写明的键，不能从 tool_call 推导 tool_call_not")
	}
}
