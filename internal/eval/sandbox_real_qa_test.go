package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/pyrt"
	"go.yaml.in/yaml/v3"
)

// Real-platform evaluation is a separate contract. Introducing it must not
// turn the frozen Mock score into a claim of actual sandbox execution, or alter
// either legacy scoring fingerprint through newly serialized report fields.
func TestSandboxRealQAMockBaselineUnaffected(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "eval", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, platform := range []string{"linux", "all"} {
		t.Run(platform, func(t *testing.T) {
			result, err := Execute(t.Context(), Options{Mode: ModeMock, Root: root, OutDir: t.TempDir(), Platform: platform})
			if err != nil {
				t.Fatal(err)
			}
			want, filtered := 217, 20
			digest := "a6988db6f3945e8c767e650776767bd62b5454d47733c4dcce2846fe44ec1340"
			if platform == "all" {
				want, filtered, digest = 237, 0, "ef3431dd8f2099b81bc29896f99e91e82c0fe1741b45cdb124b8eb2fbd881f1f"
			}
			summary := result.Report.Summary
			if result.ExitCode != ExitOK || summary.Cases != 237 || summary.Scored != want || summary.Passed != want || summary.Filtered != filtered || summary.Skipped != 0 || summary.MissingObservation != 0 || summary.Failed != 0 || !summary.HardGatesPassed {
				t.Fatalf("frozen Mock baseline changed: exit=%d summary=%+v", result.ExitCode, summary)
			}
			if result.Report.Environment.SandboxImpl != "mock" || result.Report.Mode != ModeMock {
				t.Fatalf("Mock evidence relabeled real: %+v", result.Report.Environment)
			}
			actual, err := ScoringDigest(result.Report)
			if err != nil || actual != digest {
				t.Fatalf("Mock %s scoring digest changed: got=%s want=%s error=%v", platform, actual, digest, err)
			}
		})
	}
}

func TestSandboxRealQARealSuiteEndToEnd(t *testing.T) {
	if os.Getenv("SANDBOX_REAL_PACKAGES") != "1" {
		t.Skip("real production Session supplement requires explicit prepared-runtime setup")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	runtime, err := (pyrt.Config{Root: t.TempDir()}).Ensure(ctx)
	if err != nil {
		t.Fatalf("required real prepared environment: %v", err)
	}
	root, err := filepath.Abs(filepath.Join("..", "..", "eval", "sandbox-real", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := Execute(ctx, Options{Mode: ModeSandboxReal, Root: root, OutDir: t.TempDir(), Platform: "linux", SandboxRuntime: &runtime})
	if err != nil {
		t.Fatalf("real evaluator: %v", err)
	}
	sandboxRealQAComplete(t, result)
	// Verify the persisted evidence, not only the in-memory verdict.
	body, err := os.ReadFile(result.JSONPath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted Report
	if err := json.Unmarshal(body, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Digest != result.Report.Digest || persisted.Mode != ModeSandboxReal || len(persisted.Cases) != 25 {
		t.Fatalf("persisted real report does not match result: mode=%s cases=%d digest=%s", persisted.Mode, len(persisted.Cases), persisted.Digest)
	}
	markdown, err := os.ReadFile(result.MarkdownPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(markdown), "SB-LINUX-24") || !strings.Contains(string(markdown), "sandbox-real") {
		t.Fatalf("Markdown omitted real evidence identity: %s", markdown)
	}
	// Counterfactual mock data must not override actual successful execution.
	poisoned := t.TempDir()
	if err := os.CopyFS(poisoned, os.DirFS(root)); err != nil {
		t.Fatal(err)
	}
	sandboxRealQAMutateYAML(t, filepath.Join(poisoned, "cases.yaml"), func(doc *yaml.Node) {
		list := sandboxRealQANodeField(doc, "cases")
		input := sandboxRealQANodeField(list.Content[0], "input")
		input.Content = append(input.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "mock"}, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "sandbox_denials"}, {Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"}, {Kind: yaml.ScalarNode, Tag: "!!str", Value: "error_code"}, {Kind: yaml.ScalarNode, Tag: "!!str", Value: "forged-from-mock"}}})
	})
	actual, err := Execute(ctx, Options{Mode: ModeSandboxReal, Root: poisoned, OutDir: t.TempDir(), Platform: "linux", SandboxRuntime: &runtime})
	if err != nil {
		t.Fatal(err)
	}
	sandboxRealQAComplete(t, actual)
	if actual.Report.Cases[0].Evidence.Observations["sandbox_denials"] != true || actual.Report.Cases[0].Evidence.ErrorCode != "none" {
		t.Fatal("mock data overrode actual network observations")
	}
	if actual.Report.Digest != result.Report.Digest {
		t.Fatal("non-scoring mock changed real scoring digest")
	}
}

func sandboxRealQAComplete(t *testing.T, result Result) {
	t.Helper()
	summary := result.Report.Summary
	if result.ExitCode != ExitOK || summary.Cases != 25 || summary.Scored != 25 || summary.Passed != 25 || summary.Failed != 0 || summary.Skipped != 0 || summary.MissingObservation != 0 || summary.Filtered != 0 || !summary.HardGatesPassed {
		t.Fatalf("real suite did not execute all20+5 without omissions: exit=%d summary=%+v cases=%+v", result.ExitCode, summary, result.Report.Cases)
	}
	if result.Report.Mode != ModeSandboxReal || result.Report.SelectedPlatform != "linux" || result.Report.Environment.SandboxImpl == "mock" || result.Report.Environment.SandboxImpl == "" {
		t.Fatalf("actual real backend was not reported: mode=%s environment=%+v", result.Report.Mode, result.Report.Environment)
	}
	ids := map[string]bool{}
	for _, item := range result.Report.Cases {
		if ids[item.ID] || item.Status != statusPassed || !item.Passed || item.Evidence == nil {
			t.Fatalf("duplicate/nonpassing/missing real evidence: %+v", item)
		}
		ids[item.ID] = true
	}
	for n := 1; n <= 25; n++ {
		if !ids[fmt.Sprintf("SB-LINUX-%02d", n)] {
			t.Fatalf("mandatory case missing: SB-LINUX-%02d", n)
		}
	}
	body, _ := json.Marshal(result.Report)
	var wire map[string]any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	detail, ok := wire["sandbox_real"].(map[string]any)
	if !ok {
		t.Fatal("missing detailed real report")
	}
	for name, expected := range map[string]float64{"original": 20, "supplemental": 5} {
		counts, ok := detail[name].(map[string]any)
		if !ok || counts["required"] != expected || counts["executed"] != expected || counts["passed"] != expected || counts["failed"] != float64(0) {
			t.Fatalf("real %s accounting=%v, expected%v", name, detail[name], expected)
		}
	}
}

func sandboxRealQASuite(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "eval", "sandbox-real", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func sandboxRealQANodeField(node *yaml.Node, key string) *yaml.Node {
	for n := 0; n+1 < len(node.Content); n += 2 {
		if node.Content[n].Value == key {
			return node.Content[n+1]
		}
	}
	return nil
}
func sandboxRealQACloneNode(node *yaml.Node) *yaml.Node {
	copy := *node
	copy.Content = nil
	for _, child := range node.Content {
		copy.Content = append(copy.Content, sandboxRealQACloneNode(child))
	}
	return &copy
}
func sandboxRealQAMutateYAML(t *testing.T, path string, mutate func(*yaml.Node)) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Content) != 1 {
		t.Fatal("test fixture needs one YAML document")
	}
	mutate(document.Content[0])
	body, err = yaml.Marshal(&document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
}

// These checks need no runtime/network. They require a specific manifest
// failure reason so a missing Runtime cannot accidentally mask the regression.
func TestSandboxRealQAManifestFailsClosed(t *testing.T) {
	canonical := sandboxRealQASuite(t)
	base, err := LoadSuite(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Cases) != 25 {
		t.Fatalf("canonical real manifest has %d cases, want25", len(base.Cases))
	}
	caseFile := ""
	for _, item := range base.Cases {
		if item.ID == "SB-LINUX-01" {
			caseFile = item.RelPath
		}
	}
	if caseFile == "" {
		t.Fatal("canonical original case missing")
	}
	tests := []struct {
		name, reason string
		mutate       func(*testing.T, string)
	}{
		{"missing-case", "missing required real case", func(t *testing.T, root string) {
			sandboxRealQAMutateYAML(t, filepath.Join(root, caseFile), func(doc *yaml.Node) {
				list := sandboxRealQANodeField(doc, "cases")
				if list == nil {
					t.Fatal("missing YAML case sequence")
				}
				list.Content = list.Content[1:]
			})
		}},
		{"unknown-case", "unknown real case", func(t *testing.T, root string) {
			sandboxRealQAMutateYAML(t, filepath.Join(root, caseFile), func(doc *yaml.Node) {
				list := sandboxRealQANodeField(doc, "cases")
				copy := sandboxRealQACloneNode(list.Content[0])
				sandboxRealQANodeField(copy, "id").Value = "SB-LINUX-99"
				list.Content = append(list.Content, copy)
			})
		}},
		{"fixture-version", "fixture", func(t *testing.T, root string) {
			sandboxRealQAMutateYAML(t, filepath.Join(root, caseFile), func(doc *yaml.Node) {
				list := sandboxRealQANodeField(doc, "cases")
				input := sandboxRealQANodeField(list.Content[0], "input")
				sandboxRealQANodeField(input, "fixture_version").Value = "forged-version"
			})
		}},
		// The frozen contract ignores mock instead of rejecting its presence.
		// Without a prepared runtime, forged success must still fail preflight.
		{"poisoned-mock-no-runtime", "prepared runtime", func(t *testing.T, root string) {
			sandboxRealQAMutateYAML(t, filepath.Join(root, caseFile), func(doc *yaml.Node) {
				list := sandboxRealQANodeField(doc, "cases")
				input := sandboxRealQANodeField(list.Content[0], "input")
				input.Content = append(input.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "mock"}, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "sandbox_denials"}, {Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"}}})
			})
		}},
		{"missing-assertion", "rule", func(t *testing.T, root string) {
			sandboxRealQAMutateYAML(t, filepath.Join(root, caseFile), func(doc *yaml.Node) {
				list := sandboxRealQANodeField(doc, "cases")
				judging := sandboxRealQANodeField(list.Content[0], "judging_criteria")
				rules := sandboxRealQANodeField(judging, "deterministic")
				if len(rules.Content) < 2 {
					t.Fatal("test requires multiple safety assertions")
				}
				rules.Content = rules.Content[1:]
			})
		}},
		{"unsafe-expectation", "rule", func(t *testing.T, root string) {
			sandboxRealQAMutateYAML(t, filepath.Join(root, caseFile), func(doc *yaml.Node) {
				list := sandboxRealQANodeField(doc, "cases")
				judging := sandboxRealQANodeField(list.Content[0], "judging_criteria")
				rules := sandboxRealQANodeField(judging, "deterministic")
				rules.Content[0].Value = "sandbox_denials:false"
			})
		}},
		{"missing-rule-registration", "rule", func(t *testing.T, root string) {
			sandboxRealQAMutateYAML(t, filepath.Join(root, "rules.yaml"), func(doc *yaml.Node) { keys := sandboxRealQANodeField(doc, "keys"); keys.Content = keys.Content[1:] })
		}},
		{"weakened-gate", "manifest", func(t *testing.T, root string) {
			sandboxRealQAMutateYAML(t, filepath.Join(root, "gates.yaml"), func(doc *yaml.Node) { sandboxRealQANodeField(doc, "max_failures").Value = "1" })
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.CopyFS(root, os.DirFS(canonical)); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, root)
			result, err := Execute(t.Context(), Options{Mode: ModeSandboxReal, Root: root, OutDir: t.TempDir(), Platform: "linux"})
			if err != nil {
				t.Fatalf("invalid real manifest should produce auditable failed report: %v", err)
			}
			if result.ExitCode == ExitOK || result.Report.Summary.HardGatesPassed || result.Report.Summary.Skipped != 0 {
				t.Fatalf("invalid real manifest became green/skip: exit=%d summary=%+v", result.ExitCode, result.Report.Summary)
			}
			if test.name == "poisoned-mock-no-runtime" {
				if result.Report.SandboxReal.Original.Executed != 0 || result.Report.Summary.Passed != 0 {
					t.Fatal("mock observations substituted for actual execution")
				}
			}
			var reasons []string
			if result.Report.SandboxReal != nil {
				reasons = append(reasons, result.Report.SandboxReal.PreflightFailure)
			}
			for _, item := range result.Report.Cases {
				reasons = append(reasons, item.Failures...)
			}
			if !strings.Contains(strings.Join(reasons, "\n"), test.reason) {
				t.Fatalf("runtime failure masked required manifest rejection %q: %v", test.reason, reasons)
			}
		})
	}
}
