package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/loveRyujin/lantern/core/pyrt"
	"github.com/loveRyujin/lantern/core/sandbox"
	"go.yaml.in/yaml/v3"
)

const sandboxRealVersion = "sandbox-real-v1"
const legacySandboxSourceHash = "c384d61f786c9bbb2c6575f8157f1684ebbe158ad78210a26ef815516694d5d6"

type SandboxAttempt struct {
	Attempted bool   `json:"attempted"`
	Success   bool   `json:"success"`
	Errno     *int   `json:"errno,omitempty"`
	Detail    string `json:"detail,omitempty"`
}
type SandboxOperation struct {
	Name        string          `json:"name"`
	Host        *SandboxAttempt `json:"host,omitempty"`
	Sandbox     *SandboxAttempt `json:"sandbox,omitempty"`
	Attribution string          `json:"attribution,omitempty"`
}
type SandboxEvidence struct {
	Version            string             `json:"version"`
	SourceFingerprint  string             `json:"source_fingerprint"`
	LegacyCaseID       string             `json:"legacy_case_id,omitempty"`
	LegacyFingerprint  string             `json:"legacy_fingerprint,omitempty"`
	LegacySourceSHA256 string             `json:"legacy_source_sha256,omitempty"`
	PayloadSHA256      string             `json:"payload_sha256,omitempty"`
	Operations         []SandboxOperation `json:"operations"`
	Observations       map[string]any     `json:"observations"`
	Limits             sandbox.Limits     `json:"limits"`
	Usage              sandbox.Usage      `json:"usage"`
	PayloadEntered     bool               `json:"payload_entered"`
	PythonPID          int                `json:"python_pid,omitempty"`
	ProcessReaped      bool               `json:"process_reaped"`
	TempClean          bool               `json:"temp_clean"`
	ErrorCode          string             `json:"error_code"`
	Detail             string             `json:"detail,omitempty"`
}
type SandboxRealCounts struct {
	Required int `json:"required"`
	Executed int `json:"executed"`
	Passed   int `json:"passed"`
	Failed   int `json:"failed"`
}
type SandboxRealInfo struct {
	Version              string            `json:"version"`
	Scope                string            `json:"scope"`
	RequiredOriginal     []string          `json:"required_original"`
	RequiredSupplemental []string          `json:"required_supplemental"`
	Original             SandboxRealCounts `json:"original"`
	Supplemental         SandboxRealCounts `json:"supplemental"`
	Environment          map[string]any    `json:"environment"`
	PreflightFailure     string            `json:"preflight_failure,omitempty"`
	NotApplicable        []string          `json:"not_applicable"`
}
type sandboxRealExecutor interface {
	Execute(context.Context, Case) SandboxEvidence
	Close() error
	CleanupComplete() bool
}
type sandboxRealRuleDoc struct {
	Version string   `yaml:"version"`
	Keys    []string `yaml:"keys"`
}
type sandboxRealGateDoc struct {
	Version      string `yaml:"version"`
	Original     int    `yaml:"original_cases"`
	Supplemental int    `yaml:"supplemental_cases"`
	MaxFailures  int    `yaml:"max_failures"`
}

var sandboxFixtures = []string{"http", "udp-dns", "raw-sockets", "loopback-ipv4", "loopback-ipv6", "host-home", "host-ssh", "host-credentials", "workspace-db", "outside-write", "symlink-escape", "fork-denial", "session-escape", "wall-timeout", "rss-oom", "output-limit", "setuid-denial", "ptrace-denial", "runtime-missing", "network-request", "cpu-timeout", "parent-death", "capability-unavailable", "production-png", "cancel"}

func sandboxID(index int) string { return fmt.Sprintf("SB-LINUX-%02d", index+1) }
func sandboxRequired() ([]string, []string) {
	original := make([]string, 20)
	extra := make([]string, 5)
	for i := range original {
		original[i] = sandboxID(i)
	}
	for i := range extra {
		extra[i] = sandboxID(i + 20)
	}
	return original, extra
}
func executeSandboxReal(ctx context.Context, opt Options) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	root, err := filepath.Abs(opt.Root)
	if err != nil {
		return Result{}, err
	}
	out := opt.OutDir
	if out == "" {
		out = "reports"
	}
	out, err = filepath.Abs(out)
	if err != nil {
		return Result{}, err
	}
	module := opt.ModuleRoot
	if module == "" {
		module = detectModuleRoot()
	}
	now := time.Now
	if opt.Now != nil {
		now = opt.Now
	}
	generated := now().UTC()
	original, supplemental := sandboxRequired()
	info := &SandboxRealInfo{Version: sandboxRealVersion, Scope: "Linux production sandbox only; whole-project live remains unavailable", RequiredOriginal: original, RequiredSupplemental: supplemental, Original: SandboxRealCounts{Required: 20}, Supplemental: SandboxRealCounts{Required: 5}, Environment: map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH}, NotApplicable: []string{"LLM judge", "RAG/embedder", "project-wide exfiltration gate", "approval gate"}}
	report := Report{GeneratedAt: generated.Format(time.RFC3339Nano), Mode: ModeSandboxReal, SelectedPlatform: "linux", SuiteVersion: sandboxRealVersion, Note: "Real production execution evidence; input.mock is ignored; only actual executor observations are scored. Original 20 intentions and supplemental evidence are scored separately.", SandboxReal: info, Cases: []CaseResult{}, Metrics: []Metric{}}
	report.Environment = collectEnvironment("not-used", sandboxRealVersion, root, module)
	report.Environment.Model = "fixed-script(no-cloud-LLM)"
	report.Environment.JudgeModel = "not-used"
	report.Environment.EmbeddingModel = "not-used"
	report.Environment.EmbedderID = "not-used"
	report.Environment.EmbedderDim = 0
	report.Environment.VectorStore = "not-used"
	report.Environment.ChunkStrategy = "not-used"
	report.Environment.SandboxImpl = "unverified(requested:linux-amd64/landlock+seccomp/single-task)"
	info.Environment["requested_profile"] = "linux-amd64/landlock+seccomp/single-task"
	info.Environment["backend_state"] = "not-verified"
	info.Environment["source_commit"] = report.Environment.Commit
	info.Environment["source_dirty"] = "unknown"
	if module != "" {
		status := exec.CommandContext(ctx, "git", "status", "--porcelain")
		status.Dir = module
		if output, statusErr := status.Output(); statusErr == nil {
			info.Environment["source_dirty"] = strings.TrimSpace(string(output)) != ""
		}
	}
	rulesPath := opt.RulesPath
	if rulesPath == "" {
		rulesPath = filepath.Join(root, "rules.yaml")
	}
	gatesPath := opt.GatesPath
	if gatesPath == "" {
		gatesPath = filepath.Join(root, "gates.yaml")
	}
	var rules sandboxRealRuleDoc
	var gates sandboxRealGateDoc
	var configFailures []string
	if err = readSandboxRealYAML(rulesPath, &rules); err != nil {
		configFailures = append(configFailures, err.Error())
	}
	if err = readSandboxRealYAML(gatesPath, &gates); err != nil {
		configFailures = append(configFailures, err.Error())
	}
	expectedKeys := sandboxRuleKeys()
	actualKeys := append([]string(nil), rules.Keys...)
	sort.Strings(actualKeys)
	if !slices.Equal(actualKeys, expectedKeys) {
		configFailures = append(configFailures, "real rule catalog differs from the frozen supported key set")
	}
	if rules.Version != sandboxRealVersion || gates.Version != sandboxRealVersion || gates.Original != 20 || gates.Supplemental != 5 || gates.MaxFailures != 0 {
		configFailures = append(configFailures, "invalid real rules/gates version or required-case manifest")
	}
	report.RuleCatalog = CatalogInfo{Path: rulesPath, Loaded: len(configFailures) == 0, Version: rules.Version, Keys: rules.Keys}
	report.GateCatalog = GateInfo{Path: gatesPath, Loaded: len(configFailures) == 0, Version: gates.Version, NonGate: []string{}}
	suite, loadErr := LoadSuite(root, rulesPath, gatesPath)
	if loadErr != nil {
		configFailures = append(configFailures, loadErr.Error())
	}
	cases := map[string]Case{}
	for _, item := range suite.Cases {
		cases[item.ID] = item
	}
	if opt.Category != "" || (opt.Platform != "" && opt.Platform != "auto" && opt.Platform != "linux") {
		configFailures = append(configFailures, "sandbox-real requires the complete Linux suite without category/platform filtering")
	}
	for _, item := range suite.Cases {
		known := false
		for i := range sandboxFixtures {
			if item.ID == sandboxID(i) {
				known = true
				break
			}
		}
		if !known {
			configFailures = append(configFailures, "unknown real case "+item.ID)
			report.Cases = append(report.Cases, CaseResult{ID: item.ID, Status: statusFailed, Failures: []string{"unknown real case"}})
		}
	}
	legacy := map[string]string{}
	data, legacyErr := os.ReadFile(filepath.Join(module, "eval/v1/sandbox/linux/cases.yaml"))
	if legacyErr != nil {
		configFailures = append(configFailures, "legacy source missing")
	} else {
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != legacySandboxSourceHash {
			configFailures = append(configFailures, "legacy suite source changed")
		}
	}
	legacySuite, legacyErr := LoadSuite(filepath.Join(module, "eval/v1/sandbox/linux"))
	if legacyErr == nil {
		for _, item := range legacySuite.Cases {
			legacy[item.ID], _ = CaseFingerprint(item, Environment{})
		}
	} else {
		configFailures = append(configFailures, legacyErr.Error())
	}
	var executor sandboxRealExecutor
	var release func()
	if len(configFailures) == 0 {
		if opt.SandboxRuntime == nil {
			configFailures = append(configFailures, "prepared runtime configuration is required; execution never installs")
		} else {
			release, err = pyrt.AcquirePrepared(ctx, *opt.SandboxRuntime)
			if err != nil {
				configFailures = append(configFailures, "prepared runtime: "+err.Error())
			} else {
				defer release()
				var actual map[string]any
				executor, actual, err = newSandboxRealExecutor(ctx, *opt.SandboxRuntime)
				for key, value := range actual {
					info.Environment[key] = value
				}
				if err != nil {
					configFailures = append(configFailures, err.Error())
				} else {
					defer executor.Close()
				}
			}
		}
	}
	info.PreflightFailure = strings.Join(configFailures, "; ")
	allowed := map[string]bool{}
	for _, key := range rules.Keys {
		allowed[key] = true
	}
	for i, fixture := range sandboxFixtures {
		id := sandboxID(i)
		item, exists := cases[id]
		if !exists {
			item = Case{ID: id, Category: "sandbox.real", Platform: "linux"}
		}
		result := CaseResult{ID: id, Category: item.Category, Platform: "linux", Source: item.Source, RunID: ModeSandboxReal + ":" + id, Status: statusPassed, Passed: true, Failures: []string{}}
		missingObservation := false
		evidence := SandboxEvidence{Version: sandboxRealVersion, Operations: []SandboxOperation{}, Observations: map[string]any{}}
		evidence.SourceFingerprint, _ = CaseFingerprint(item, Environment{})
		if i < 20 {
			evidence.LegacyCaseID = id
			evidence.LegacyFingerprint = legacy[id]
			evidence.LegacySourceSHA256 = legacySandboxSourceHash
		}
		if !exists {
			result.Failures = append(result.Failures, "missing required real case")
		}
		input, inputOK := asMap(item.Input)
		manifestFields := len(input)
		if _, hasMock := input["mock"]; hasMock {
			manifestFields--
		}
		if !inputOK || input["fixture"] != fixture || input["fixture_version"] != sandboxRealVersion || manifestFields != map[bool]int{true: 5, false: 2}[i < 20] {
			result.Failures = append(result.Failures, "case fixture/version manifest mismatch; unsupported fields forbidden")
		}
		if i < 20 && (input["legacy_id"] != id || input["legacy_version"] != "v1" || input["legacy_fingerprint"] != legacy[id]) {
			result.Failures = append(result.Failures, "legacy case lineage mismatch")
		}
		actualRules := append([]string(nil), item.Rules...)
		requiredRules := sandboxRequiredRules(i)
		sort.Strings(actualRules)
		sort.Strings(requiredRules)
		if !slices.Equal(actualRules, requiredRules) {
			result.Failures = append(result.Failures, "case rule assertions differ from the frozen required set")
		}
		if len(item.Rules) == 0 || item.Judge.Required || item.Judge.Model != "" || item.Judge.Rubric != "" || item.Platform != "linux" || item.Category != "sandbox.real" {
			result.Failures = append(result.Failures, "real case requires deterministic sandbox.real/Linux rules only")
		}
		if len(configFailures) > 0 {
			result.Failures = append(result.Failures, "global preflight failed: "+info.PreflightFailure)
		}
		if len(result.Failures) == 0 {
			evidence = executor.Execute(ctx, item)
			evidence.Version = sandboxRealVersion
			evidence.SourceFingerprint, _ = CaseFingerprint(item, Environment{})
			if i < 20 {
				evidence.LegacyCaseID = id
				evidence.LegacyFingerprint = legacy[id]
				evidence.LegacySourceSHA256 = legacySandboxSourceHash
			}
			if evidence.PayloadEntered {
				info.Environment["backend_state"] = "verified-by-production-payload"
				report.Environment.SandboxImpl = "linux-amd64/landlock+seccomp/single-task"
			}
			if i < 20 {
				info.Original.Executed++
			} else {
				info.Supplemental.Executed++
			}
			for _, rule := range item.Rules {
				key, want, parseErr := ParseDeterministicRule(rule)
				if parseErr != nil || !allowed[key] {
					result.Failures = append(result.Failures, "unknown/invalid real rule "+rule)
					continue
				}
				actual, present := evidence.Observations[key]
				if !present {
					missingObservation = true
					result.Failures = append(result.Failures, key+" missing real observation")
				} else if !ruleMatches(key, want, actual) {
					result.Failures = append(result.Failures, mismatchMessage(key, want, actual))
				}
			}
			if evidence.Detail != "" {
				result.Failures = append(result.Failures, evidence.Detail)
			}
		}
		result.Evidence = &evidence
		if len(result.Failures) > 0 {
			result.Status = statusFailed
			result.Passed = false
			if missingObservation {
				result.Status = statusMissing
				report.Summary.MissingObservation++
			}
			if i < 20 {
				info.Original.Failed++
			} else {
				info.Supplemental.Failed++
			}
		} else {
			report.Summary.Passed++
			if i < 20 {
				info.Original.Passed++
			} else {
				info.Supplemental.Passed++
			}
		}
		report.Cases = append(report.Cases, result)
	}
	if executor != nil {
		cleanupErr := executor.Close()
		removed := executor.CleanupComplete()
		info.Environment["fixture_root_removed"] = removed
		if cleanupErr != nil || !removed {
			info.Environment["fixture_cleanup"] = "failed"
			report.Cases = append(report.Cases, CaseResult{ID: "SANDBOX-REAL-FIXTURE-CLEANUP", Status: statusFailed, Failures: []string{fmt.Sprintf("fixture cleanup error=%v; root removed=%t", cleanupErr, removed)}})
		} else {
			info.Environment["fixture_cleanup"] = "passed"
		}
	}
	sort.Slice(report.Cases, func(i, j int) bool { return report.Cases[i].ID < report.Cases[j].ID })
	report.Summary.Cases = len(report.Cases)
	report.Summary.Scored = len(report.Cases)
	report.Summary.Failed = report.Summary.Cases - report.Summary.Passed - report.Summary.MissingObservation
	report.Summary.HardGatesPassed = report.Summary.Failed == 0 && report.Summary.MissingObservation == 0 && info.Original.Executed == 20 && info.Supplemental.Executed == 5
	report.Summary.PassRate = floatPtr(float64(report.Summary.Passed) / float64(report.Summary.Scored))
	report.HardGates = []HardGate{{ID: "sandbox-real-original", Title: "Original Linux intentions", Op: "==", Threshold: 0, Value: floatPtr(float64(info.Original.Failed)), SampleCount: 20, Passed: info.Original.Passed == 20}, {ID: "sandbox-real-supplemental", Title: "Supplemental production evidence", Op: "==", Threshold: 0, Value: floatPtr(float64(info.Supplemental.Failed)), SampleCount: 5, Passed: info.Supplemental.Passed == 5}}
	integrity := report.Summary.Cases == 25 && info.PreflightFailure == "" && info.Environment["fixture_cleanup"] == "passed"
	integrityFailures := float64(0)
	if !integrity {
		integrityFailures = 1
	}
	report.HardGates = append(report.HardGates, HardGate{ID: "sandbox-real-integrity", Title: "Exact run manifest and fixture cleanup", Op: "==", Threshold: 0, Value: floatPtr(integrityFailures), SampleCount: 1, Passed: integrity})
	report.Sandbox = []SandboxPlatform{{Platform: "linux", CaseCount: 25, SampleCount: 25, FailureCount: floatPtr(float64(report.Summary.Failed)), Passed: report.Summary.HardGatesPassed}}
	report, err = report.withDigest()
	if err != nil {
		return Result{}, err
	}
	jsonPath, mdPath, err := writeReportFiles(out, generated, report)
	if err != nil {
		return Result{}, err
	}
	code := ExitOK
	if !report.Summary.HardGatesPassed {
		code = ExitFailed
	}
	return Result{report, jsonPath, mdPath, code}, nil
}
func readSandboxRealYAML(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err = dec.Decode(value); err != nil {
		return err
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return fmt.Errorf("real catalog must contain exactly one YAML document")
	}
	return nil
}
func renderSandboxRealEvidence(b *strings.Builder, report Report) {
	if report.SandboxReal == nil {
		return
	}
	info := report.SandboxReal
	fmt.Fprintf(b, "\n## Real sandbox evidence\n\nScope: %s\n\nOriginal intentions: %d/%d passed; supplemental: %d/%d passed.\n", info.Scope, info.Original.Passed, info.Original.Required, info.Supplemental.Passed, info.Supplemental.Required)
	for _, item := range report.Cases {
		if item.Evidence == nil {
			continue
		}
		e := item.Evidence
		fmt.Fprintf(b, "\n- %s: error=%s, entered=%t, PID=%d, reaped=%t, clean=%t; observations in JSON report.\n", item.ID, e.ErrorCode, e.PayloadEntered, e.PythonPID, e.ProcessReaped, e.TempClean)
	}
}

func sandboxRequiredRules(index int) []string {
	common := []string{"payload_entered:true", "process_reaped:true", "temp_clean:true"}
	denial := func(extra ...string) []string {
		r := append([]string(nil), common...)
		r = append(r, "required_host_controls:true", "sandbox_denials:true", "error_code:none")
		return append(r, extra...)
	}
	switch index + 1 {
	case 1, 2, 3, 4, 5:
		return denial("listener_quiet:true")
	case 17, 18:
		return denial()
	case 6, 7, 8, 9, 10, 11:
		return denial("sentinel_unchanged:true")
	case 12, 13:
		return denial("created_children:0")
	case 14:
		return append(common, "error_code:timeout", "wall_budget_enforced:true")
	case 15:
		return append(common, "error_code:oom", "memory_budget_enforced:true")
	case 16:
		return append(common, "error_code:error", "output_bounded:true", "file_quota_enforced:true")
	case 19:
		return []string{"error_code:runtime_unavailable", "payload_entered:false", "worker_started:false", "temp_clean:true"}
	case 20:
		return []string{"registry_rejected:true", "valid_schema_accepted:true", "tool_execute_count:0"}
	case 21:
		return append(common, "error_code:timeout", "cpu_budget_enforced:true")
	case 22:
		return []string{"payload_entered:true", "parent_death_cleaned:true", "process_reaped:true", "temp_clean:true"}
	case 23:
		return []string{"payload_entered:false", "error_code:sandbox_unavailable", "capability_failure_closed:true", "temp_clean:true"}
	case 24:
		return append(common, "error_code:none", "production_png_valid:true", "agent_loop_completed:true", "artifact_rejection_observed:true")
	case 25:
		return append(common, "error_code:timeout", "cancel_observed:true")
	}
	return nil
}
func sandboxRuleKeys() []string {
	keys := map[string]bool{}
	for i := range sandboxFixtures {
		for _, rule := range sandboxRequiredRules(i) {
			key, _, _ := ParseDeterministicRule(rule)
			keys[key] = true
		}
	}
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}
