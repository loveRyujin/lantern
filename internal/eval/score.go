package eval

import (
	"context"
	"fmt"
	"strings"
)

type gateObs struct {
	key      string
	expected string
	actual   any
	present  bool
	mismatch bool
}

type caseScore struct {
	result       CaseResult
	observations []gateObs
	countGates   bool
}

type gateTally struct {
	samples  map[string]int
	failures map[string]int
	sandbox  []sandboxSample
}

type sandboxSample struct {
	platform string
	failed   bool
}

func newGateTally() gateTally {
	return gateTally{
		samples:  map[string]int{},
		failures: map[string]int{},
	}
}

func scoreSuite(ctx context.Context, suite Suite, catalog RuleCatalog, gates GateSet, opt Options, judge Judge, model string) Report {
	selected := resolvePlatform(opt.Platform)
	results := make([]CaseResult, 0, len(suite.Cases))
	tally := newGateTally()
	for _, item := range suite.Cases {
		scored := scoreCase(ctx, item, catalog, opt, judge, model, selected)
		results = append(results, scored.result)
		if scored.countGates {
			addGateSamples(&tally, gates, item.Category, item.Platform, selected, scored.observations)
		}
	}
	if results == nil {
		results = []CaseResult{}
	}
	hard := buildHardGates(gates, tally)
	passed, failed, missing, skipped, filtered := 0, 0, 0, 0, 0
	for _, item := range results {
		switch item.Status {
		case statusPassed:
			passed++
		case statusFailed:
			failed++
		case statusMissing:
			missing++
		case statusSkipped:
			skipped++
		case statusFiltered:
			filtered++
		}
	}
	allHard := true
	for _, gate := range hard {
		if !gate.Passed {
			allHard = false
			break
		}
	}
	denom := passed + failed + missing
	summary := Summary{
		Cases:              len(results),
		Scored:             denom,
		Passed:             passed,
		Failed:             failed,
		MissingObservation: missing,
		Skipped:            skipped,
		Filtered:           filtered,
		HardGatesPassed:    allHard,
	}
	if denom > 0 {
		summary.PassRate = floatPtr(float64(passed) / float64(denom))
	}
	return Report{
		Mode:             modeOrDefault(opt.Mode),
		Category:         opt.Category,
		SelectedPlatform: selected,
		Note:             skeletonNote,
		Summary:          summary,
		HardGates:        hard,
		Metrics:          buildMetrics(hard),
		Sandbox:          sandboxReports(tally.sandbox, hasGate(gates, "sandbox")),
		Cases:            results,
	}
}

func scoreCase(ctx context.Context, item Case, catalog RuleCatalog, opt Options, judge Judge, model, selected string) caseScore {
	res := CaseResult{
		ID:       item.ID,
		Category: item.Category,
		Platform: item.Platform,
		Source:   item.Source,
		Failures: []string{},
		Status:   statusPassed,
		Passed:   true,
	}
	selectedCase := caseSelected(item, opt.Category, selected)
	mock, hasMock := mockState(item.Input)
	var msgs []string
	var observations []gateObs
	rulesOK := true
	mismatched := false
	missingObs := false
	for _, rule := range item.Rules {
		key, want, err := ParseDeterministicRule(rule)
		if err != nil {
			msgs = append(msgs, err.Error())
			rulesOK = false
			continue
		}
		if !catalog.Has(key) {
			msgs = append(msgs, fmt.Sprintf("未知判定规则 %q，不在规则清单中", rule))
			rulesOK = false
			continue
		}
		if !selectedCase || !hasMock {
			continue
		}
		actual, present := observation(mock, key)
		obs := gateObs{key: key, expected: want, actual: actual, present: present}
		if !present {
			msgs = append(msgs, fmt.Sprintf("%s 缺少观测", key))
			missingObs = true
		} else if !ruleMatches(key, want, actual) {
			msgs = append(msgs, mismatchMessage(key, want, actual))
			obs.mismatch = true
			mismatched = true
		}
		observations = append(observations, obs)
	}
	judgeErr := judgeConfigError(item.Judge, model)
	if judgeErr != "" {
		msgs = append(msgs, judgeErr)
	}
	if !selectedCase {
		if !rulesOK || judgeErr != "" {
			res.Failures = msgs
			res.Status = statusFailed
			res.Passed = false
			res.RunID = runID(opt.Mode, item.ID)
			return finishScore(res, nil, false)
		}
		res.Status = statusFiltered
		res.Passed = false
		res.Failures = []string{filterReason(item, opt.Category, selected)}
		return finishScore(res, nil, false)
	}
	res.RunID = runID(opt.Mode, item.ID)
	if !rulesOK || judgeErr != "" {
		res.Failures = msgs
		res.Status = statusFailed
		res.Passed = false
		return finishScore(res, observations, hasMock)
	}
	if !hasMock {
		res.Status = statusSkipped
		res.Passed = false
		res.Failures = []string{"没有 input.mock，未评分"}
		return finishScore(res, nil, false)
	}
	if mismatched {
		res.Failures = msgs
		res.Status = statusFailed
		res.Passed = false
		return finishScore(res, observations, true)
	}
	if missingObs {
		res.Failures = msgs
		res.Status = statusMissing
		res.Passed = false
		return finishScore(res, observations, true)
	}
	if item.Judge.Required {
		verdict, err := judge.Judge(ctx, Request{
			CaseID:   item.ID,
			Category: item.Category,
			Input:    item.Input,
			Expected: item.Expected,
			Rubric:   item.Judge.Rubric,
			Rules:    append([]string(nil), item.Rules...),
		})
		res.JudgeCalled = true
		if err != nil {
			msgs = append(msgs, "LLM 裁判失败："+err.Error())
		} else if !verdict.Pass {
			reason := verdict.Reason
			if reason == "" {
				reason = "未通过"
			}
			msgs = append(msgs, "LLM 裁判未通过："+reason)
		}
		if len(msgs) > 0 {
			res.Failures = msgs
			res.Status = statusFailed
			res.Passed = false
		}
	}
	return finishScore(res, observations, true)
}

func finishScore(res CaseResult, observations []gateObs, count bool) caseScore {
	if res.Failures == nil {
		res.Failures = []string{}
	}
	return caseScore{result: res, observations: observations, countGates: count}
}

func addGateSamples(tally *gateTally, gates GateSet, category, casePlatform, selected string, observations []gateObs) {
	byKey := indexObs(observations)
	for _, gate := range gates.Gates {
		for _, key := range gate.Keys {
			if !categoryMatches(category, key.Categories) {
				continue
			}
			obs, ok := byKey[key.Name]
			if !ok {
				continue
			}
			// 没有观测，或观测值不等于用例期望值，都记一次失败。布尔成功信号也走这条。
			recordGateSample(tally, gate.ID, !obs.present || obs.mismatch, casePlatform, selected)
		}
	}
}

func recordGateSample(tally *gateTally, id string, failed bool, casePlatform, selected string) {
	tally.samples[id]++
	if failed {
		tally.failures[id]++
	}
	if id != "sandbox" {
		return
	}
	platform := casePlatform
	if platform == "" && (selected == "darwin" || selected == "linux") {
		platform = selected
	}
	tally.sandbox = append(tally.sandbox, sandboxSample{platform: platform, failed: failed})
}

func indexObs(list []gateObs) map[string]gateObs {
	out := make(map[string]gateObs, len(list))
	for _, item := range list {
		prev, ok := out[item.key]
		if !ok {
			out[item.key] = item
			continue
		}
		if item.present {
			prev.present = true
			prev.actual = item.actual
			prev.expected = item.expected
		}
		prev.mismatch = prev.mismatch || item.mismatch
		out[item.key] = prev
	}
	return out
}

func hasGate(gates GateSet, id string) bool {
	for _, gate := range gates.Gates {
		if gate.ID == id {
			return true
		}
	}
	return false
}

func judgeConfigError(spec JudgeSpec, model string) string {
	if spec.Required {
		if spec.Model != model || strings.TrimSpace(spec.Rubric) == "" {
			return fmt.Sprintf("LLM 裁判配置错误：required 为 true 时模型必须是 %s，且 rubric 不能为空", model)
		}
		return ""
	}
	if spec.Model != "" || spec.Rubric != "" {
		return "LLM 裁判配置错误：required 为 false 时不能设置 model 或 rubric"
	}
	return ""
}

func filterReason(item Case, category, selected string) string {
	if category != "" && item.Category != category {
		return fmt.Sprintf("类别 %s 与本次筛选 %s 不符，未评分", item.Category, category)
	}
	if item.Platform == "" {
		return "未评分"
	}
	return fmt.Sprintf("平台 %s 与本次 %s 不符，未评分", item.Platform, selected)
}

func caseSelected(item Case, category, selected string) bool {
	if category != "" && item.Category != category {
		return false
	}
	if selected == "all" || item.Platform == "" || item.Platform == selected {
		return true
	}
	return false
}

func observation(mock map[string]any, key string) (any, bool) {
	if mock == nil {
		return nil, false
	}
	if value, ok := mock[key]; ok {
		return value, true
	}
	// _not 没有同名观测时回退到基键。两个键都没有则视为缺观测。
	if IsNotForm(key) {
		base, _ := strings.CutSuffix(key, "_not")
		if value, ok := mock[base]; ok {
			return value, true
		}
	}
	return nil, false
}

func ruleMatches(key, want string, actual any) bool {
	if IsNotForm(key) {
		return !containsValue(actual, true, want)
	}
	if list, ok := asList(actual); ok {
		for _, item := range list {
			if scalarString(item) == want {
				return true
			}
		}
		return false
	}
	return scalarString(actual) == want
}

func containsValue(actual any, present bool, want string) bool {
	if !present || actual == nil {
		return false
	}
	if list, ok := asList(actual); ok {
		for _, item := range list {
			if scalarString(item) == want {
				return true
			}
		}
		return false
	}
	return scalarString(actual) == want
}

func mismatchMessage(key, want string, actual any) string {
	if IsNotForm(key) {
		return fmt.Sprintf("%s 禁止 %s，实际出现了 %s", key, want, scalarString(actual))
	}
	return fmt.Sprintf("%s 实际 %s，要求 %s", key, scalarString(actual), want)
}

func mockState(input any) (map[string]any, bool) {
	m, ok := asMap(input)
	if !ok {
		return nil, false
	}
	raw, exists := m["mock"]
	if !exists {
		return nil, false
	}
	mock, ok := asMap(raw)
	if !ok {
		return map[string]any{}, true
	}
	return mock, true
}

func runID(mode, id string) string {
	if mode == "" {
		mode = ModeMock
	}
	return mode + ":" + id
}

func modeOrDefault(mode string) string {
	if mode == "" {
		return ModeMock
	}
	return mode
}

func resolvePlatform(flag string) string {
	switch flag {
	case "", "auto":
		return currentPlatform()
	default:
		return flag
	}
}
