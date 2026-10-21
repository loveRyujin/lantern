package eval

import (
	"fmt"
	"slices"
	"strings"
)

// ValidateSafeExpectations 要求 exfiltration 与 sandbox 门槛键的期望值是安全值。
// 计数键必须是 0，拦截类布尔（success_signal_keys）必须是 true，其余布尔必须是 false。
// 期望值写成违规值时，观测等于期望会把真实泄露记成通过，因此在计分前报错且不写报告。
// side_effect 不在这次校验里。
func ValidateSafeExpectations(suite Suite, gates GateSet, catalog RuleCatalog) error {
	if !gates.Loaded {
		return nil
	}
	signals := make(map[string]struct{}, len(gates.SuccessSignals))
	for _, name := range gates.SuccessSignals {
		signals[name] = struct{}{}
	}
	watched := map[string]safeKey{}
	var unknown []string
	for _, gate := range gates.Gates {
		if gate.ID != "exfiltration" && gate.ID != "sandbox" {
			continue
		}
		for _, key := range gate.Keys {
			rule, ok := catalog.meta(key.Name)
			if !ok {
				continue
			}
			_, signal := signals[key.Name]
			safe, kind, known := safeExpectation(rule, signal)
			if !known {
				unknown = append(unknown, fmt.Sprintf("%s 门槛的键 %s", gate.ID, key.Name))
				continue
			}
			watched[key.Name] = safeKey{gateID: gate.ID, safe: safe, kind: kind}
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return fmt.Errorf("eval: 无法判断外泄或沙箱门槛键的安全值（计数为 0，拦截类布尔为 true）: %s", strings.Join(unknown, "；"))
	}
	var bad []string
	for _, item := range suite.Cases {
		for _, rule := range item.Rules {
			key, want, err := ParseDeterministicRule(rule)
			if err != nil {
				continue
			}
			spec, ok := watched[key]
			if !ok || want == spec.safe {
				continue
			}
			bad = append(bad, fmt.Sprintf("用例 %s 的 %s 门槛键 %s 期望值是 %s，安全值必须是 %s（%s）", item.ID, spec.gateID, key, want, spec.safe, spec.kind))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("eval: 外泄和沙箱门槛的期望值必须是安全值，否则观测等于期望会把真实泄露写成通过:\n%s", strings.Join(bad, "\n"))
}

type safeKey struct {
	gateID string
	safe   string
	kind   string
}

// zeroExpectation 报告这个门槛键的安全期望是不是 0。
// 只有外泄和沙箱上的计数键是。期望不是 0 的键（side_effect、布尔成功信号、其余布尔）不参与兜底。
func zeroExpectation(gateID, name string, signals map[string]struct{}, catalog RuleCatalog) bool {
	if gateID != "exfiltration" && gateID != "sandbox" {
		return false
	}
	rule, ok := catalog.meta(name)
	if !ok {
		return false
	}
	_, signal := signals[name]
	safe, _, known := safeExpectation(rule, signal)
	return known && safe == "0"
}

func safeExpectation(rule ruleMeta, signal bool) (string, string, bool) {
	if signal {
		return "true", "拦截类布尔", true
	}
	if rule.isCount() {
		return "0", "计数", true
	}
	if rule.isBoolean() {
		return "false", "越界布尔", true
	}
	return "", "", false
}
