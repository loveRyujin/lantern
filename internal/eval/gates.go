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
	Path    string
	Version string
	Gates   []GateDef
	NonGate []string
	Loaded  bool
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
	if _, ok := m["ungated"]; ok {
		return GateSet{}, fmt.Errorf("请使用 non_gate，不再接受 ungated")
	}
	nonGate, err := parseNameList(m["non_gate"], "non_gate")
	if err != nil {
		return GateSet{}, err
	}
	slices.Sort(nonGate)
	return GateSet{Version: version, Gates: gates, NonGate: nonGate}, nil
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
	m, ok := asMap(raw)
	if !ok {
		return GateDef{}, fmt.Errorf("门槛 %s 必须是映射", id)
	}
	title, err := optionalString(m, "title")
	if err != nil {
		return GateDef{}, err
	}
	if title == "" {
		title = defaultGateTitle(id)
	}
	keys, err := parseGateKeys(m["keys"])
	if err != nil {
		return GateDef{}, fmt.Errorf("门槛 %s: %w", id, err)
	}
	return GateDef{ID: id, Title: title, Keys: keys}, nil
}

func parseGateKeys(raw any) ([]GateKey, error) {
	if raw == nil {
		return []GateKey{}, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("keys 必须是列表")
	}
	out := make([]GateKey, 0, len(list))
	seen := map[string]struct{}{}
	for _, item := range list {
		key, err := parseGateKey(item)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[key.Name]; ok {
			return nil, fmt.Errorf("键 %q 重复", key.Name)
		}
		seen[key.Name] = struct{}{}
		out = append(out, key)
	}
	return out, nil
}

func parseGateKey(item any) (GateKey, error) {
	switch t := item.(type) {
	case string:
		name := strings.TrimSpace(t)
		if name == "" {
			return GateKey{}, fmt.Errorf("键不能为空")
		}
		return GateKey{Name: name}, nil
	case map[string]any:
		name, err := optionalString(t, "name")
		if err != nil {
			return GateKey{}, err
		}
		if name == "" {
			name, err = optionalString(t, "key")
			if err != nil {
				return GateKey{}, err
			}
		}
		if name == "" {
			return GateKey{}, fmt.Errorf("键缺少 name")
		}
		categories, err := parseKeyCategories(t)
		if err != nil {
			return GateKey{}, err
		}
		return GateKey{Name: name, Categories: categories}, nil
	default:
		return GateKey{}, fmt.Errorf("键必须是字符串或映射")
	}
}

func parseKeyCategories(m map[string]any) ([]string, error) {
	_, hasOne := m["category"]
	_, hasMany := m["categories"]
	if hasOne && hasMany {
		return nil, fmt.Errorf("不能同时设置 category 和 categories")
	}
	if hasOne {
		text, err := optionalString(m, "category")
		if err != nil {
			return nil, err
		}
		if text == "" {
			return nil, fmt.Errorf("category 必须是非空字符串")
		}
		return []string{text}, nil
	}
	if !hasMany || m["categories"] == nil {
		return nil, nil
	}
	return parseNameList(m["categories"], "categories")
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
