package eval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Suite 是一次加载得到的用例集。Cases 按 id 排序。
type Suite struct {
	Root    string
	Version string
	Cases   []Case
}

// Case 是一条评测用例。
type Case struct {
	ID       string
	Category string
	Input    any
	Expected string
	Rules    []string
	Judge    JudgeSpec
	Platform string
	RelPath  string
	Source   string
}

// LoadSuite 递归读取 root 下的 YAML 和 JSONL。
// rulesPath 指向规则清单时，该文件不会被当成用例；名为 rules.yaml 或 rules.yml 的文件同样跳过。
func LoadSuite(root, rulesPath string) (Suite, error) {
	info, err := os.Stat(root)
	if err != nil {
		return Suite{}, fmt.Errorf("eval: 用例目录不存在: %s", root)
	}
	if !info.IsDir() {
		return Suite{}, fmt.Errorf("eval: 用例目录不是文件夹: %s", root)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return Suite{}, fmt.Errorf("eval: 用例目录: %w", err)
	}
	absRules := ""
	if rulesPath != "" {
		absRules, err = filepath.Abs(rulesPath)
		if err != nil {
			return Suite{}, fmt.Errorf("eval: 规则清单路径: %w", err)
		}
	}

	var files []string
	err = filepath.WalkDir(absRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if name != "." && strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".") {
			return nil
		}
		if skipSuiteFile(path, absRules) {
			return nil
		}
		switch strings.ToLower(filepath.Ext(entry.Name())) {
		case ".yaml", ".yml", ".jsonl":
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return Suite{}, fmt.Errorf("eval: 遍历用例目录: %w", err)
	}
	slices.Sort(files)

	var cases []Case
	seen := make(map[string]string)
	for _, path := range files {
		rel, relErr := filepath.Rel(absRoot, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		batch, loadErr := loadFile(path, rel)
		if loadErr != nil {
			return Suite{}, loadErr
		}
		for _, item := range batch {
			if prev, ok := seen[item.ID]; ok {
				return Suite{}, fmt.Errorf("eval: 用例 id %q 重复（%s 与 %s）", item.ID, prev, item.Source)
			}
			seen[item.ID] = item.Source
			cases = append(cases, item)
		}
	}
	slices.SortFunc(cases, func(a, b Case) int {
		return strings.Compare(a.ID, b.ID)
	})
	if cases == nil {
		cases = []Case{}
	}
	return Suite{Root: absRoot, Version: inferSuiteVersion(absRoot), Cases: cases}, nil
}

func skipSuiteFile(path, rulesPath string) bool {
	base := strings.ToLower(filepath.Base(path))
	if base == "rules.yaml" || base == "rules.yml" {
		return true
	}
	if rulesPath == "" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	return filepath.Clean(abs) == filepath.Clean(rulesPath)
}

func inferSuiteVersion(root string) string {
	base := filepath.Base(filepath.Clean(root))
	if len(base) >= 2 && base[0] == 'v' {
		digits := true
		for _, r := range base[1:] {
			if r < '0' || r > '9' {
				digits = false
				break
			}
		}
		if digits {
			return base
		}
	}
	return "v1"
}

func loadFile(path, rel string) ([]Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: 读取 %s: %w", rel, err)
	}
	if strings.EqualFold(filepath.Ext(path), ".jsonl") {
		return loadJSONL(data, rel)
	}
	return loadYAML(data, rel)
}

func loadYAML(data []byte, rel string) ([]Case, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var cases []Case
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("eval: %s: %w", rel, err)
		}
		if node.Kind == 0 || node.Tag == "!!null" {
			continue
		}
		var doc any
		if err := node.Decode(&doc); err != nil {
			return nil, fmt.Errorf("eval: %s:%d: %w", rel, node.Line, err)
		}
		doc = normalize(doc)
		if doc == nil {
			continue
		}
		batch, err := casesFromDoc(doc, rel, node.Line)
		if err != nil {
			return nil, err
		}
		cases = append(cases, batch...)
	}
	return cases, nil
}

func loadJSONL(data []byte, rel string) ([]Case, error) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	var cases []Case
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if lineNo == 1 {
			line = strings.TrimPrefix(line, "\uFEFF")
		}
		if line == "" {
			continue
		}
		var doc any
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			return nil, fmt.Errorf("eval: %s:%d: %w", rel, lineNo, err)
		}
		doc = normalize(doc)
		batch, err := casesFromDoc(doc, rel, lineNo)
		if err != nil {
			return nil, err
		}
		cases = append(cases, batch...)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("eval: %s: %w", rel, err)
	}
	return cases, nil
}

func casesFromDoc(doc any, rel string, line int) ([]Case, error) {
	source := sourceAt(rel, line)
	switch t := doc.(type) {
	case []any:
		out := make([]Case, 0, len(t))
		for i, item := range t {
			m, ok := asMap(item)
			if !ok {
				return nil, fmt.Errorf("eval: %s: 第 %d 条用例必须是映射", source, i+1)
			}
			itemSource := source
			c, err := caseFromMap(m, itemSource, rel)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, nil
	case map[string]any:
		_, hasCases := t["cases"]
		_, hasID := t["id"]
		if hasCases && hasID {
			return nil, fmt.Errorf("eval: %s: 不能同时是单条用例和 cases 列表", source)
		}
		if hasCases {
			list, ok := t["cases"].([]any)
			if !ok {
				return nil, fmt.Errorf("eval: %s: cases 必须是列表", source)
			}
			return casesFromDoc(list, rel, line)
		}
		if hasID {
			c, err := caseFromMap(t, source, rel)
			if err != nil {
				return nil, err
			}
			return []Case{c}, nil
		}
		return nil, fmt.Errorf("eval: %s: 无法识别的文档，需要用例或 cases 列表", source)
	default:
		return nil, fmt.Errorf("eval: %s: 无法识别的文档", source)
	}
}

func caseFromMap(m map[string]any, source, rel string) (Case, error) {
	id, err := requiredString(m, "id", source)
	if err != nil {
		return Case{}, err
	}
	category, err := requiredString(m, "category", source)
	if err != nil {
		return Case{}, err
	}
	input, ok := m["input"]
	if !ok || isEmptyInput(input) {
		return Case{}, fmt.Errorf("eval: %s: 缺少字段 \"input\"", source)
	}
	expected, err := expectedOf(m, source)
	if err != nil {
		return Case{}, err
	}
	rules, judge, err := criteriaOf(m, source)
	if err != nil {
		return Case{}, err
	}
	platform, err := optionalPlatform(m, source)
	if err != nil {
		return Case{}, err
	}
	inferred := inferPlatform(rel)
	if platform != "" && inferred != "" && platform != inferred {
		return Case{}, fmt.Errorf("eval: %s: platform %q 与路径推断的 %q 不一致", source, platform, inferred)
	}
	if platform == "" {
		platform = inferred
	}
	return Case{
		ID:       id,
		Category: category,
		Input:    input,
		Expected: expected,
		Rules:    rules,
		Judge:    judge,
		Platform: platform,
		RelPath:  rel,
		Source:   source,
	}, nil
}

func expectedOf(m map[string]any, source string) (string, error) {
	behavior, hasBehavior, err := lookupString(m, "expected_behavior")
	if err != nil {
		return "", fmt.Errorf("eval: %s: %w", source, err)
	}
	alias, hasAlias, err := lookupString(m, "expected")
	if err != nil {
		return "", fmt.Errorf("eval: %s: %w", source, err)
	}
	switch {
	case hasBehavior && hasAlias && behavior != alias:
		return "", fmt.Errorf("eval: %s: expected_behavior 与 expected 不一致", source)
	case hasBehavior:
		if strings.TrimSpace(behavior) == "" {
			return "", fmt.Errorf("eval: %s: 字段 \"expected_behavior\" 不能为空", source)
		}
		return behavior, nil
	case hasAlias:
		if strings.TrimSpace(alias) == "" {
			return "", fmt.Errorf("eval: %s: 字段 \"expected\" 不能为空", source)
		}
		return alias, nil
	default:
		return "", fmt.Errorf("eval: %s: 缺少字段 \"expected_behavior\"", source)
	}
}

func criteriaOf(m map[string]any, source string) ([]string, JudgeSpec, error) {
	_, hasCanon := m["judging_criteria"]
	_, hasAlias := m["rule"]
	if hasCanon && hasAlias {
		return nil, JudgeSpec{}, fmt.Errorf("eval: %s: 不能同时设置 judging_criteria 和 rule", source)
	}
	if !hasCanon && !hasAlias {
		return nil, JudgeSpec{}, fmt.Errorf("eval: %s: 缺少判定规则（judging_criteria 或 rule）", source)
	}
	raw := m["judging_criteria"]
	if hasAlias {
		raw = m["rule"]
	}
	if raw == nil {
		return nil, JudgeSpec{}, fmt.Errorf("eval: %s: 判定规则不能为空", source)
	}
	rules, judge, err := parseCriteria(raw)
	if err != nil {
		return nil, JudgeSpec{}, fmt.Errorf("eval: %s: %w", source, err)
	}
	return rules, judge, nil
}

func parseCriteria(raw any) ([]string, JudgeSpec, error) {
	switch t := raw.(type) {
	case string:
		rule := strings.TrimSpace(t)
		if rule == "" {
			return nil, JudgeSpec{}, fmt.Errorf("判定规则不能为空")
		}
		return []string{rule}, JudgeSpec{}, nil
	case []any:
		rules, err := stringList(t)
		if err != nil {
			return nil, JudgeSpec{}, err
		}
		if len(rules) == 0 {
			return nil, JudgeSpec{}, fmt.Errorf("至少需要一条 deterministic 判定规则")
		}
		return rules, JudgeSpec{}, nil
	case map[string]any:
		rawRules, ok := t["deterministic"]
		if !ok || rawRules == nil {
			return nil, JudgeSpec{}, fmt.Errorf("判定规则缺少 deterministic")
		}
		list, ok := rawRules.([]any)
		if !ok {
			return nil, JudgeSpec{}, fmt.Errorf("deterministic 必须是列表")
		}
		rules, err := stringList(list)
		if err != nil {
			return nil, JudgeSpec{}, err
		}
		if len(rules) == 0 {
			return nil, JudgeSpec{}, fmt.Errorf("至少需要一条 deterministic 判定规则")
		}
		judge, err := parseJudgeSpec(t["llm_judge"])
		if err != nil {
			return nil, JudgeSpec{}, err
		}
		return rules, judge, nil
	default:
		return nil, JudgeSpec{}, fmt.Errorf("判定规则必须是字符串、列表或映射")
	}
}

func parseJudgeSpec(raw any) (JudgeSpec, error) {
	if raw == nil {
		return JudgeSpec{}, nil
	}
	m, ok := asMap(raw)
	if !ok {
		return JudgeSpec{}, fmt.Errorf("llm_judge 必须是映射")
	}
	var spec JudgeSpec
	if value, ok := m["required"]; ok && value != nil {
		flag, ok := value.(bool)
		if !ok {
			return JudgeSpec{}, fmt.Errorf("llm_judge.required 必须是布尔值")
		}
		spec.Required = flag
	}
	model, err := optionalString(m, "model")
	if err != nil {
		return JudgeSpec{}, fmt.Errorf("llm_judge.%w", err)
	}
	rubric, err := optionalString(m, "rubric")
	if err != nil {
		return JudgeSpec{}, fmt.Errorf("llm_judge.%w", err)
	}
	spec.Model = model
	spec.Rubric = rubric
	return spec, nil
}

func stringList(list []any) ([]string, error) {
	out := make([]string, 0, len(list))
	for _, item := range list {
		text, ok := item.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("deterministic 规则必须是非空字符串")
		}
		out = append(out, strings.TrimSpace(text))
	}
	return out, nil
}

func requiredString(m map[string]any, key, source string) (string, error) {
	text, ok, err := lookupString(m, key)
	if err != nil {
		return "", fmt.Errorf("eval: %s: %w", source, err)
	}
	if !ok || strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("eval: %s: 缺少字段 %q", source, key)
	}
	if strings.TrimSpace(text) != text || strings.ContainsAny(text, "\r\n") {
		return "", fmt.Errorf("eval: %s: 字段 %q 含有首尾空白或换行", source, key)
	}
	return text, nil
}

func lookupString(m map[string]any, key string) (string, bool, error) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return "", false, nil
	}
	text, ok := raw.(string)
	if !ok {
		return "", false, fmt.Errorf("字段 %q 必须是字符串", key)
	}
	return text, true, nil
}

func optionalPlatform(m map[string]any, source string) (string, error) {
	text, ok, err := lookupString(m, "platform")
	if err != nil {
		return "", fmt.Errorf("eval: %s: %w", source, err)
	}
	if !ok {
		return "", nil
	}
	switch text {
	case "", "darwin", "linux":
		return text, nil
	default:
		return "", fmt.Errorf("eval: %s: platform 必须是 darwin 或 linux", source)
	}
}

func inferPlatform(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "sandbox" && (parts[i+1] == "darwin" || parts[i+1] == "linux") {
			return parts[i+1]
		}
	}
	return ""
}

func isEmptyInput(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	default:
		return false
	}
}

func sourceAt(rel string, line int) string {
	if line <= 0 {
		return rel
	}
	return fmt.Sprintf("%s:%d", rel, line)
}
