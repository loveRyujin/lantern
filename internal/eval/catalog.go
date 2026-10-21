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
}

// Has 报告 key 是否在清单中。
func (c RuleCatalog) Has(key string) bool {
	_, ok := c.set[key]
	return ok
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
		keys := make([]string, 0, len(t))
		for key := range t {
			if strings.TrimSpace(key) == "" {
				return RuleCatalog{}, fmt.Errorf("规则键不能为空")
			}
			keys = append(keys, key)
		}
		slices.Sort(keys)
		return newCatalog(keys, version)
	case nil:
		return RuleCatalog{}, fmt.Errorf("rules 不能为空")
	default:
		return RuleCatalog{}, fmt.Errorf("rules 必须是列表或映射")
	}
}

func catalogFromList(list []any, version string) (RuleCatalog, error) {
	keys := make([]string, 0, len(list))
	for _, item := range list {
		key, err := catalogKey(item)
		if err != nil {
			return RuleCatalog{}, err
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return newCatalog(keys, version)
}

func catalogKey(item any) (string, error) {
	switch t := item.(type) {
	case string:
		key := strings.TrimSpace(t)
		if key == "" {
			return "", fmt.Errorf("规则键不能为空")
		}
		return key, nil
	case map[string]any:
		for _, field := range []string{"name", "key", "id"} {
			if raw, ok := t[field]; ok {
				text, ok := raw.(string)
				if !ok || strings.TrimSpace(text) == "" {
					return "", fmt.Errorf("规则键 %s 必须是非空字符串", field)
				}
				return strings.TrimSpace(text), nil
			}
		}
		return "", fmt.Errorf("规则项缺少 name、key 或 id")
	default:
		return "", fmt.Errorf("规则项必须是字符串或带 name/key/id 的映射")
	}
}

func newCatalog(keys []string, version string) (RuleCatalog, error) {
	set := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, ok := set[key]; ok {
			return RuleCatalog{}, fmt.Errorf("规则键 %q 重复", key)
		}
		set[key] = struct{}{}
		out = append(out, key)
	}
	return RuleCatalog{Version: version, Keys: out, set: set}, nil
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
