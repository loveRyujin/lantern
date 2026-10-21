package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// CaseFingerprint 是用例内容的指纹。
// env 不参与计算：只改运行环境时指纹不变。source 和相对路径也不参与。
// input 里的对象键按 JSON 规则排序，YAML 里的键序不会改变指纹。
func CaseFingerprint(c Case, env Environment) (string, error) {
	_ = env
	rules := c.Rules
	if rules == nil {
		rules = []string{}
	}
	payload := struct {
		ID       string    `json:"id"`
		Category string    `json:"category"`
		Input    any       `json:"input"`
		Expected string    `json:"expected"`
		Rules    []string  `json:"rules"`
		Judge    JudgeSpec `json:"judge"`
		Platform string    `json:"platform"`
	}{
		ID:       c.ID,
		Category: c.Category,
		Input:    c.Input,
		Expected: c.Expected,
		Rules:    rules,
		Judge:    c.Judge,
		Platform: c.Platform,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("eval: 用例指纹: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
