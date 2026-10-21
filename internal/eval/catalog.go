package eval

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// RuleCatalog 是确定性规则的键清单。Has 按完整键匹配，tool_call_not 不会
// 因为清单里有 tool_call 就被当成已知规则。
type RuleCatalog struct {
	Path    string
	Version string
	Keys    []string
	Loaded  bool
	set     map[string]struct{}
	rules   []ruleMeta
}

type ruleMeta struct {
	Name       string
	Kind       string
	ParamTypes []string
}

// isCount 判断规则是不是计数类。名字以 _count 结尾，或者确定性参数类型是
// integer / integer_or_symbol（例如 citation_count_gte、requested_timeout_seconds）。
// llm_judge 的参数不参与这个判断。
func (r ruleMeta) isCount() bool {
	if strings.HasSuffix(r.Name, "_count") {
		return true
	}
	if r.Kind == "llm_judge" {
		return false
	}
	for _, typ := range r.ParamTypes {
		switch typ {
		case "integer", "integer_or_symbol":
			return true
		}
	}
	return false
}

// isBoolean 判断确定性规则的参数是不是布尔。拦截类布尔的安全期望是 true，
// 其余布尔（例如 write_outside_output）的安全期望是 false。
func (r ruleMeta) isBoolean() bool {
	if r.Kind == "llm_judge" {
		return false
	}
	for _, typ := range r.ParamTypes {
		if typ == "boolean" {
			return true
		}
	}
	return false
}

func (c RuleCatalog) meta(name string) (ruleMeta, bool) {
	for _, rule := range c.rules {
		if rule.Name == name {
			return rule, true
		}
	}
	return ruleMeta{}, false
}

// Has 报告 key 是否在清单中。
func (c RuleCatalog) Has(key string) bool {
	_, ok := c.set[key]
	return ok
}

// CountKeys 返回计数类规则键，按名字排序。
func (c RuleCatalog) CountKeys() []string {
	out := make([]string, 0)
	for _, rule := range c.rules {
		if rule.isCount() {
			out = append(out, rule.Name)
		}
	}
	slices.Sort(out)
	return out
}

// IsNotForm 报告 key 是不是“禁止”形式。禁止形式的键以 _not 结尾，且前面还有内容。
func IsNotForm(key string) bool {
	base, ok := strings.CutSuffix(key, "_not")
	return ok && base != ""
}

// ParseDeterministicRule 把“键:期望值”拆开。期望值里可以再含冒号。
func ParseDeterministicRule(raw string) (key, expected string, err error) {
	text := strings.TrimSpace(raw)
	idx := strings.IndexByte(text, ':')
	if idx <= 0 || idx == len(text)-1 {
		return "", "", fmt.Errorf("判定规则 %q 无法解析，需要“键:期望值”形式", raw)
	}
	key = strings.TrimSpace(text[:idx])
	expected = strings.TrimSpace(text[idx+1:])
	if key == "" || expected == "" || strings.ContainsAny(key, " \t") {
		return "", "", fmt.Errorf("判定规则 %q 无法解析，需要“键:期望值”形式", raw)
	}
	return key, expected, nil
}

// LoadRuleCatalog 读取规则清单。文件不存在时返回错误，调用方决定空用例集是否可以没有清单。
func LoadRuleCatalog(path string) (RuleCatalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RuleCatalog{}, fmt.Errorf("eval: 读取规则清单 %s: %w", path, err)
	}
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return RuleCatalog{}, fmt.Errorf("eval: 解析规则清单 %s: %w", path, err)
	}
	doc = normalize(doc)
	cat, err := parseCatalog(doc)
	if err != nil {
		return RuleCatalog{}, fmt.Errorf("eval: 规则清单 %s: %w", path, err)
	}
	cat.Path = path
	cat.Loaded = true
	return cat, nil
}

func parseCatalog(doc any) (RuleCatalog, error) {
	switch t := doc.(type) {
	case []any:
		return catalogFromList(t, "")
	case map[string]any:
		version, err := optionalString(t, "version")
		if err != nil {
			return RuleCatalog{}, err
		}
		if raw, ok := t["rules"]; ok {
			return catalogFromValue(raw, version)
		}
		if raw, ok := t["keys"]; ok {
			return catalogFromValue(raw, version)
		}
		return RuleCatalog{}, fmt.Errorf("无法识别规则清单，需要 rules 或 keys")
	case nil:
		return RuleCatalog{}, fmt.Errorf("规则清单是空的")
	default:
		return RuleCatalog{}, fmt.Errorf("无法识别规则清单，需要 rules 或 keys")
	}
}

func catalogFromValue(raw any, version string) (RuleCatalog, error) {
	switch t := raw.(type) {
	case []any:
		return catalogFromList(t, version)
	case map[string]any:
		names := make([]string, 0, len(t))
		for key := range t {
			if strings.TrimSpace(key) == "" {
				return RuleCatalog{}, fmt.Errorf("规则键不能为空")
			}
			names = append(names, key)
		}
		slices.Sort(names)
		rules := make([]ruleMeta, 0, len(names))
		for _, name := range names {
			rule, err := ruleFromValue(name, t[name])
			if err != nil {
				return RuleCatalog{}, err
			}
			rules = append(rules, rule)
		}
		return newCatalog(rules, version)
	case nil:
		return RuleCatalog{}, fmt.Errorf("rules 不能为空")
	default:
		return RuleCatalog{}, fmt.Errorf("rules 必须是列表或映射")
	}
}

func catalogFromList(list []any, version string) (RuleCatalog, error) {
	rules := make([]ruleMeta, 0, len(list))
	for _, item := range list {
		rule, err := ruleFromItem(item)
		if err != nil {
			return RuleCatalog{}, err
		}
		rules = append(rules, rule)
	}
	return newCatalog(rules, version)
}

func ruleFromItem(item any) (ruleMeta, error) {
	switch t := item.(type) {
	case string:
		key := strings.TrimSpace(t)
		if key == "" {
			return ruleMeta{}, fmt.Errorf("规则键不能为空")
		}
		return ruleMeta{Name: key}, nil
	case map[string]any:
		name, err := catalogKey(t)
		if err != nil {
			return ruleMeta{}, err
		}
		return ruleFromValue(name, t)
	default:
		return ruleMeta{}, fmt.Errorf("规则项必须是字符串或带 name/key/id 的映射")
	}
}

func ruleFromValue(name string, raw any) (ruleMeta, error) {
	rule := ruleMeta{Name: name}
	if raw == nil {
		return rule, nil
	}
	m, ok := asMap(raw)
	if !ok {
		return ruleMeta{}, fmt.Errorf("规则 %s 的说明必须是映射", name)
	}
	kind, err := optionalString(m, "kind")
	if err != nil {
		return ruleMeta{}, err
	}
	types, err := parameterTypes(m["parameters"])
	if err != nil {
		return ruleMeta{}, err
	}
	rule.Kind = kind
	rule.ParamTypes = types
	return rule, nil
}

func parameterTypes(raw any) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("parameters 必须是列表")
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		m, ok := asMap(item)
		if !ok {
			return nil, fmt.Errorf("parameters 项必须是映射")
		}
		typ, err := optionalString(m, "type")
		if err != nil {
			return nil, err
		}
		if typ != "" {
			out = append(out, typ)
		}
	}
	return out, nil
}

func catalogKey(item map[string]any) (string, error) {
	for _, field := range []string{"name", "key", "id"} {
		if raw, ok := item[field]; ok {
			text, ok := raw.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return "", fmt.Errorf("规则键 %s 必须是非空字符串", field)
			}
			return strings.TrimSpace(text), nil
		}
	}
	return "", fmt.Errorf("规则项缺少 name、key 或 id")
}

func newCatalog(rules []ruleMeta, version string) (RuleCatalog, error) {
	set := make(map[string]struct{}, len(rules))
	out := make([]ruleMeta, 0, len(rules))
	names := make([]string, 0, len(rules))
	for _, rule := range rules {
		if _, ok := set[rule.Name]; ok {
			return RuleCatalog{}, fmt.Errorf("规则键 %q 重复", rule.Name)
		}
		set[rule.Name] = struct{}{}
		out = append(out, rule)
		names = append(names, rule.Name)
	}
	slices.Sort(names)
	slices.SortFunc(out, func(a, b ruleMeta) int {
		return strings.Compare(a.Name, b.Name)
	})
	return RuleCatalog{Version: version, Keys: names, set: set, rules: out}, nil
}

func optionalString(m map[string]any, key string) (string, error) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return "", nil
	}
	text, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s 必须是字符串", key)
	}
	return strings.TrimSpace(text), nil
}
