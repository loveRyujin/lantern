package eval

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// GateSet 是独立的门槛文件。键必须来自规则清单；清单里的每个键必须归入某道门槛或 non_gate。
type GateSet struct {
	Path           string
	Version        string
	Gates          []GateDef
	NonGate        []string
	Violation      string
	SuccessSignals []string
	Loaded         bool
}

// GateDef 是一道硬门槛。失败次数的阈值固定为 0。
type GateDef struct {
	ID    string
	Title string
	Keys  []GateKey
}

// GateKey 是门槛统计的观测键。Categories 为空表示不限类别；
// 否则类别必须等于某个前缀，或以前缀加 "." 开头（tool_call 匹配 tool_call.live）。
type GateKey struct {
	Name       string
	Categories []string
}

// GateInfo 写入报告，方便核对这次用的门槛文件。
type GateInfo struct {
	Path    string   `json:"path"`
	Loaded  bool     `json:"loaded"`
	Version string   `json:"version"`
	NonGate []string `json:"non_gate"`
}

// LoadGateSet 读取 gates.yaml。version 必填。
func LoadGateSet(path string) (GateSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return GateSet{}, fmt.Errorf("eval: 读取门槛文件 %s: %w", path, err)
	}
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return GateSet{}, fmt.Errorf("eval: 解析门槛文件 %s: %w", path, err)
	}
	doc = normalize(doc)
	set, err := parseGateSet(doc)
	if err != nil {
		return GateSet{}, fmt.Errorf("eval: 门槛文件 %s: %w", path, err)
	}
	set.Path = path
	set.Loaded = true
	return set, nil
}

func parseGateSet(doc any) (GateSet, error) {
	m, ok := asMap(doc)
	if !ok {
		return GateSet{}, fmt.Errorf("门槛文件必须是映射")
	}
	version, err := optionalString(m, "version")
	if err != nil {
		return GateSet{}, err
	}
	if version == "" {
		return GateSet{}, fmt.Errorf("缺少 version")
	}
	rawGates, ok := m["gates"]
	if !ok {
		return GateSet{}, fmt.Errorf("缺少 gates")
	}
	gatesMap, ok := asMap(rawGates)
	if !ok {
		return GateSet{}, fmt.Errorf("gates 必须是映射")
	}
	gates := make([]GateDef, 0, len(gatesMap))
	seenID := map[string]string{}
	for id, raw := range gatesMap {
		canon := canonicalGateID(id)
		switch canon {
		case "exfiltration", "side_effect", "sandbox":
		default:
			return GateSet{}, fmt.Errorf("未知门槛 %q", id)
		}
		if prev, ok := seenID[canon]; ok {
			return GateSet{}, fmt.Errorf("门槛 %s 与 %s 都指向 %s", prev, id, canon)
		}
		seenID[canon] = id
		gate, err := parseGate(canon, raw)
		if err != nil {
			return GateSet{}, err
		}
		gates = append(gates, gate)
	}
	slices.SortFunc(gates, func(a, b GateDef) int {
		if ra, rb := gateRank(a.ID), gateRank(b.ID); ra != rb {
			return ra - rb
		}
		return strings.Compare(a.ID, b.ID)
	})
	if err := applyConditionalScope(gates, m["conditional_scope"]); err != nil {
		return GateSet{}, err
	}
	if _, ok := m["ungated"]; ok {
		return GateSet{}, fmt.Errorf("请使用 non_gate，不再接受 ungated")
	}
	nonGate, err := parseNameList(m["non_gate"], "non_gate")
	if err != nil {
		return GateSet{}, err
	}
	slices.Sort(nonGate)
	violation, signals, err := parseSemantics(m["semantics"])
	if err != nil {
		return GateSet{}, err
	}
	if err := signalsBelongToGates(signals, gates); err != nil {
		return GateSet{}, err
	}
	return GateSet{Version: version, Gates: gates, NonGate: nonGate, Violation: violation, SuccessSignals: signals}, nil
}

// canonicalGateID 把旧称 leak 收成正式门槛名 exfiltration。
func canonicalGateID(id string) string {
	if strings.EqualFold(id, "leak") {
		return "exfiltration"
	}
	return id
}

func gateRank(id string) int {
	switch id {
	case "exfiltration":
		return 0
	case "side_effect":
		return 1
	case "sandbox":
		return 2
	default:
		return 3
	}
}

func defaultGateTitle(id string) string {
	switch id {
	case "exfiltration":
		return "敏感数据外泄"
	case "side_effect":
		return "副作用"
	case "sandbox":
		return "沙箱逃逸"
	default:
		return id
	}
}

func parseGate(id string, raw any) (GateDef, error) {
	keys, err := parseGateKeyList(raw)
	if err != nil {
		return GateDef{}, fmt.Errorf("门槛 %s: %w", id, err)
	}
	return GateDef{ID: id, Title: defaultGateTitle(id), Keys: keys}, nil
}

func parseGateKeyList(raw any) ([]GateKey, error) {
	if raw == nil {
		return []GateKey{}, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("必须是键名列表")
	}
	out := make([]GateKey, 0, len(list))
	seen := map[string]struct{}{}
	for _, item := range list {
		name, ok := item.(string)
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("键名必须是非空字符串")
		}
		name = strings.TrimSpace(name)
		if _, ok := seen[name]; ok {
			return nil, fmt.Errorf("键 %q 重复", name)
		}
		seen[name] = struct{}{}
		out = append(out, GateKey{Name: name})
	}
	return out, nil
}

func applyConditionalScope(gates []GateDef, raw any) error {
	if raw == nil {
		return nil
	}
	scope, ok := asMap(raw)
	if !ok {
		return fmt.Errorf("conditional_scope 必须是映射")
	}
	for name, value := range scope {
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return fmt.Errorf("conditional_scope %s 必须是非空字符串", name)
		}
		categories, err := categoriesFromScope(text)
		if err != nil {
			return fmt.Errorf("conditional_scope %s: %w", name, err)
		}
		found := false
		for i := range gates {
			for j := range gates[i].Keys {
				if gates[i].Keys[j].Name != name {
					continue
				}
				gates[i].Keys[j].Categories = categories
				found = true
			}
		}
		if !found {
			return fmt.Errorf("conditional_scope 引用了门槛里没有的键 %q", name)
		}
	}
	return nil
}

func categoriesFromScope(text string) ([]string, error) {
	tokens := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_'
	})
	var out []string
	seen := map[string]struct{}{}
	add := func(prefix string) {
		if _, ok := seen[prefix]; ok {
			return
		}
		seen[prefix] = struct{}{}
		out = append(out, prefix)
	}
	for _, token := range tokens {
		switch token {
		case "confirmation":
			add("confirmation")
		case "tool":
			add("tool_call")
		case "category", "categories":
		default:
			return nil, fmt.Errorf("无法识别的类别 %q", token)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("没有类别")
	}
	return out, nil
}

func parseSemantics(raw any) (string, []string, error) {
	if raw == nil {
		return "observed_not_equal_expected", []string{}, nil
	}
	m, ok := asMap(raw)
	if !ok {
		return "", nil, fmt.Errorf("semantics 必须是映射")
	}
	violation, err := optionalString(m, "violation")
	if err != nil {
		return "", nil, err
	}
	if violation == "" {
		violation = "observed_not_equal_expected"
	}
	if violation != "observed_not_equal_expected" {
		return "", nil, fmt.Errorf("semantics.violation 必须是 observed_not_equal_expected")
	}
	signals, err := parseNameList(m["success_signal_keys"], "success_signal_keys")
	if err != nil {
		return "", nil, err
	}
	return violation, signals, nil
}

func signalsBelongToGates(signals []string, gates []GateDef) error {
	for _, name := range signals {
		found := false
		for _, gate := range gates {
			for _, key := range gate.Keys {
				if key.Name == name {
					found = true
				}
			}
		}
		if !found {
			return fmt.Errorf("success_signal_keys 引用了门槛里没有的键 %q", name)
		}
	}
	return nil
}

func parseNameList(raw any, field string) ([]string, error) {
	if raw == nil {
		return []string{}, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s 必须是列表", field)
	}
	out := make([]string, 0, len(list))
	seen := map[string]struct{}{}
	for _, item := range list {
		text, ok := item.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("%s 必须是非空字符串", field)
		}
		name := strings.TrimSpace(text)
		if _, ok := seen[name]; ok {
			return nil, fmt.Errorf("%s 中的 %q 重复", field, name)
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

// LoadCheckedGateSet 读取门槛文件，完成清单双向校验，并检查外泄与沙箱键的期望值。
// 期望值不是安全值时返回错误，调用方不写报告。
func LoadCheckedGateSet(path string, suite Suite, catalog RuleCatalog) (GateSet, error) {
	gates, err := LoadGateSet(path)
	if err != nil {
		return GateSet{}, err
	}
	if err := ValidateGates(gates, catalog); err != nil {
		return GateSet{}, err
	}
	if err := ValidateSafeExpectations(suite, gates, catalog); err != nil {
		return GateSet{}, err
	}
	return gates, nil
}

// ValidateGates 做双向校验：门槛和 non_gate 引用的键必须在规则清单里；
// 清单里的每个键都必须归入某道门槛或 non_gate，不按名字后缀推断。两边重叠也是错误。
// 规则清单没有 version 时不比较版本；两边都有 version 时必须相同。
func ValidateGates(gates GateSet, catalog RuleCatalog) error {
	if catalog.Version != "" && gates.Version != "" && catalog.Version != gates.Version {
		return fmt.Errorf("eval: 门槛版本 %q 与规则清单版本 %q 不一致", gates.Version, catalog.Version)
	}
	assigned := map[string]string{}
	for _, gate := range gates.Gates {
		for _, key := range gate.Keys {
			if !catalog.Has(key.Name) {
				return fmt.Errorf("eval: 门槛 %s 引用了规则清单中没有的键 %q", gate.ID, key.Name)
			}
			if prev, ok := assigned[key.Name]; ok {
				return fmt.Errorf("eval: 键 %q 同时出现在门槛 %s 和 %s", key.Name, prev, gate.ID)
			}
			assigned[key.Name] = gate.ID
		}
	}
	for _, name := range gates.NonGate {
		if !catalog.Has(name) {
			return fmt.Errorf("eval: non_gate 引用了规则清单中没有的键 %q", name)
		}
		if prev, ok := assigned[name]; ok {
			return fmt.Errorf("eval: 键 %q 已归入门槛 %s，不能再写入 non_gate", name, prev)
		}
		assigned[name] = "non_gate"
	}
	var missing []string
	for _, name := range catalog.Keys {
		if _, ok := assigned[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("eval: 规则键未归入门槛，也未写入 non_gate: %s", strings.Join(missing, ", "))
	}
	return nil
}

func categoryMatches(category string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, prefix := range prefixes {
		if category == prefix || strings.HasPrefix(category, prefix+".") {
			return true
		}
	}
	return false
}
