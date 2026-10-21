package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/internal/eval"
)

func TestCLIUnknownRuleExitsNonZero(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rules.yaml"), []byte("rules: [tool_call]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "gates.yaml"), []byte("version: v1\ngates: {}\nnon_gate: [tool_call]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := []byte("id: BAD\ncategory: tool_call.mock\ninput: {task: x}\nexpected_behavior: x\nrule: \"not_a_rule:1\"\n")
	if err := os.WriteFile(filepath.Join(root, "case.yaml"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--root", root, "--out", out}, &stdout, &stderr)
	if code != eval.ExitFailed {
		t.Fatalf("退出码 = %d\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "评测未通过") {
		t.Fatalf("stderr = %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), ".json") || !strings.Contains(stdout.String(), ".md") {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestCLIMissingCatalogExitsNonZero(t *testing.T) {
	root := t.TempDir()
	body := []byte("id: A\ncategory: tools\ninput: {task: a}\nexpected_behavior: a\nrule: \"tool_call:search_docs\"\n")
	if err := os.WriteFile(filepath.Join(root, "case.yaml"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"--root", root, "--out", t.TempDir()}, &stdout, &stderr)
	if code == eval.ExitOK {
		t.Fatalf("缺少规则清单时不应成功\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "规则清单") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestCLILiveModeExitsWithoutReport(t *testing.T) {
	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--mode", "live", "--out", out}, &stdout, &stderr)
	if code != eval.ExitUsage {
		t.Fatalf("退出码 = %d\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "未接入") {
		t.Fatalf("stderr = %s", stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "grok") || strings.Contains(stdout.String()+stderr.String(), "ollama") {
		t.Fatalf("live 报错不应提到未使用的模型\n%s\n%s", stdout.String(), stderr.String())
	}
	matches, err := filepath.Glob(filepath.Join(out, "eval-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("live 不应写报告: %v", matches)
	}
}

func TestCLIRejectsBadMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--mode", "sometimes"}, &stdout, &stderr)
	if code != eval.ExitUsage {
		t.Fatalf("退出码 = %d\n%s", code, stderr.String())
	}
}
