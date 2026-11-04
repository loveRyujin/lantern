package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// ToolRunPython 是生产沙箱工具名。超权限规则只认这个名字。
	ToolRunPython = "run_python"

	// 这些数字和 core/sandbox Runner.normalize 的默认值、硬上限一致。
	// 0 表示沿用默认，不视为超权限。当前 Linux 单任务 profile 的进程数硬上限就是 1，
	// 因此不存在「高于默认且不超过硬上限」的进程数确认区间；超过 1 直接拒绝。
	DefaultCPUMillis   int64 = 10_000
	HardCPUMillis      int64 = 30_000
	DefaultWallMillis  int64 = 30_000
	HardWallMillis     int64 = 60_000
	DefaultMemoryBytes int64 = 512 << 20
	HardMemoryBytes    int64 = 1 << 30
	DefaultNProc       int64 = 1
	HardNProc          int64 = 1
)

var (
	errSandboxObject   = errors.New("policy: 沙箱参数不是 JSON 对象")
	errSandboxLimits   = errors.New("policy: 沙箱资源不是对象")
	errSandboxResource = errors.New("policy: 沙箱资源不是整数")
	errSandboxMounts   = errors.New("policy: 额外只读目录不是字符串数组")
)

// 模型可以塞这些字段，判定时丢掉。它们不能代替本人确认，也不能打开网络。
var ignoredModelKeys = map[string]struct{}{
	"requires_approval": {},
	"approved":          {},
	"needs_confirm":     {},
	"confirmed":         {},
}

// Gate 先按工具名走 Static，再对 run_python 读取资源、额外只读目录和网络。
// export_data 和 write_action 仍不读 Args。
type Gate struct {
	Names Static
}

// SandboxGrant 是从 run_python 参数里读出的申请。
// 时间为毫秒，内存为字节，进程数为个数。0 表示没有高于默认的申请。
// Network 为 true 时不能执行，也不能通过确认放开。
type SandboxGrant struct {
	CPUMillis     int64
	MemoryBytes   int64
	WallMillis    int64
	NProc         int64
	ExtraReadOnly []string
	Network       bool
	NetworkDetail string
}

// Decide 实现 Policy。名字名单命中后不再解析沙箱参数。
func (g Gate) Decide(ctx context.Context, req Request) (Decision, error) {
	named, err := g.Names.Decide(ctx, req)
	if err != nil || named.NeedsConfirm || named.Deny {
		return named, err
	}
	if req.Tool != ToolRunPython {
		return Decision{}, nil
	}
	grant, err := ParseRunPython(req.Args)
	if err != nil {
		return Decision{}, err
	}
	return classifySandbox(grant), nil
}

// ParseRunPython 只读取副本，不改传入的字节。
// 空参数表示没有超权限申请。无法识别的字段返回错误，调用方不得执行。
func ParseRunPython(args json.RawMessage) (SandboxGrant, error) {
	trim := bytes.TrimSpace(args)
	if len(trim) == 0 || bytes.Equal(trim, []byte("null")) {
		return SandboxGrant{}, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trim, &fields); err != nil || fields == nil {
		return SandboxGrant{}, errSandboxObject
	}
	for key := range fields {
		if _, ignore := ignoredModelKeys[key]; ignore {
			continue
		}
		switch key {
		case "code", "sql_call_id", "limits", "extra_read_only", "network":
		default:
			return SandboxGrant{}, fmt.Errorf("policy: 未识别的沙箱参数 %q", key)
		}
	}
	var grant SandboxGrant
	if raw, ok := fields["limits"]; ok {
		parsed, err := parseLimits(raw)
		if err != nil {
			return SandboxGrant{}, err
		}
		grant.CPUMillis = parsed.CPUMillis
		grant.MemoryBytes = parsed.MemoryBytes
		grant.WallMillis = parsed.WallMillis
		grant.NProc = parsed.NProc
	}
	if raw, ok := fields["extra_read_only"]; ok {
		dirs, err := parseMounts(raw)
		if err != nil {
			return SandboxGrant{}, err
		}
		grant.ExtraReadOnly = dirs
	}
	if raw, ok := fields["network"]; ok {
		requested, detail, err := parseNetwork(raw)
		if err != nil {
			return SandboxGrant{}, err
		}
		grant.Network = requested
		grant.NetworkDetail = detail
	}
	return grant, nil
}

// Limits 把已经允许确认的申请写成沙箱限额。
// 超过硬上限或带网络时返回错误，避免调用方把拒绝写成 Spec。
// 0 留给沙箱自己填默认值。
func (g SandboxGrant) Limits() (cpu time.Duration, memory uint64, nproc uint64, wall time.Duration, err error) {
	if g.Network || g.CPUMillis < 0 || g.MemoryBytes < 0 || g.WallMillis < 0 || g.NProc < 0 {
		return 0, 0, 0, 0, errors.New("policy: 不能把拒绝的申请写成沙箱限额")
	}
	if g.CPUMillis > HardCPUMillis || g.MemoryBytes > HardMemoryBytes || g.WallMillis > HardWallMillis || g.NProc > HardNProc {
		return 0, 0, 0, 0, errors.New("policy: 申请超过硬上限")
	}
	return time.Duration(g.CPUMillis) * time.Millisecond,
		uint64(g.MemoryBytes),
		uint64(g.NProc),
		time.Duration(g.WallMillis) * time.Millisecond,
		nil
}

func classifySandbox(g SandboxGrant) Decision {
	if g.Network {
		detail := g.NetworkDetail
		if detail == "" {
			detail = "（空）"
		}
		reason := "拒绝沙箱网络申请：" + detail + "。默认禁网，回环也不能通过确认放开"
		return Decision{Deny: true, Reason: reason, Risk: reason}
	}
	var denies, confirms []string
	deny, confirm := classifyMillis("CPU", g.CPUMillis, DefaultCPUMillis, HardCPUMillis)
	denies = appendText(denies, deny)
	confirms = appendText(confirms, confirm)
	deny, confirm = classifyCount("内存", g.MemoryBytes, DefaultMemoryBytes, HardMemoryBytes, "B")
	denies = appendText(denies, deny)
	confirms = appendText(confirms, confirm)
	deny, confirm = classifyMillis("超时", g.WallMillis, DefaultWallMillis, HardWallMillis)
	denies = appendText(denies, deny)
	confirms = appendText(confirms, confirm)
	deny, confirm = classifyCount("进程数", g.NProc, DefaultNProc, HardNProc, "")
	denies = appendText(denies, deny)
	confirms = appendText(confirms, confirm)
	if len(g.ExtraReadOnly) > 0 {
		confirms = append(confirms, "额外只读目录 "+strings.Join(g.ExtraReadOnly, "、")+"（默认无额外挂载）")
	}
	if len(denies) > 0 {
		reason := "拒绝沙箱资源申请：" + strings.Join(denies, "；")
		return Decision{Deny: true, Reason: reason, Risk: reason}
	}
	if len(confirms) > 0 {
		reason := "沙箱申请超出默认权限：" + strings.Join(confirms, "；")
		return Decision{NeedsConfirm: true, Reason: reason, Risk: reason}
	}
	return Decision{}
}

func classifyMillis(label string, requested, def, hard int64) (deny, confirm string) {
	return classifyCount(label, requested, def, hard, "ms")
}

func classifyCount(label string, requested, def, hard int64, unit string) (deny, confirm string) {
	if requested == 0 {
		return "", ""
	}
	shown := func(n int64) string {
		if unit == "" {
			return fmt.Sprintf("%d", n)
		}
		return fmt.Sprintf("%d%s", n, unit)
	}
	if requested > hard {
		return fmt.Sprintf("%s %s 超过硬上限 %s（默认 %s），不能通过确认放开", label, shown(requested), shown(hard), shown(def)), ""
	}
	if requested > def {
		return "", fmt.Sprintf("%s %s > 默认 %s", label, shown(requested), shown(def))
	}
	return "", ""
}

func appendText(dst []string, text string) []string {
	if text == "" {
		return dst
	}
	return append(dst, text)
}

func parseLimits(raw json.RawMessage) (SandboxGrant, error) {
	if isNull(raw) {
		return SandboxGrant{}, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return SandboxGrant{}, errSandboxLimits
	}
	known := map[string]*int64{
		"cpu_ms":       new(int64),
		"memory_bytes": new(int64),
		"wall_ms":      new(int64),
		"nproc":        new(int64),
	}
	for key, value := range fields {
		slot, ok := known[key]
		if !ok {
			return SandboxGrant{}, fmt.Errorf("policy: 未识别的沙箱资源 %q", key)
		}
		parsed, err := parseResource(value)
		if err != nil {
			return SandboxGrant{}, err
		}
		*slot = parsed
	}
	return SandboxGrant{
		CPUMillis:   *known["cpu_ms"],
		MemoryBytes: *known["memory_bytes"],
		WallMillis:  *known["wall_ms"],
		NProc:       *known["nproc"],
	}, nil
}

func parseResource(raw json.RawMessage) (int64, error) {
	if isNull(raw) {
		return 0, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var num json.Number
	if err := dec.Decode(&num); err != nil || dec.More() {
		return 0, errSandboxResource
	}
	value, err := num.Int64()
	if err != nil || value < 0 {
		return 0, errSandboxResource
	}
	return value, nil
}

func parseMounts(raw json.RawMessage) ([]string, error) {
	if isNull(raw) {
		return nil, nil
	}
	var dirs []string
	if err := json.Unmarshal(raw, &dirs); err != nil {
		return nil, errSandboxMounts
	}
	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			return nil, errSandboxMounts
		}
	}
	if len(dirs) == 0 {
		return nil, nil
	}
	return append([]string(nil), dirs...), nil
}

func parseNetwork(raw json.RawMessage) (bool, string, error) {
	if isNull(raw) {
		return false, "", nil
	}
	var flag bool
	if err := json.Unmarshal(raw, &flag); err == nil {
		if !flag {
			return false, "", nil
		}
		return true, "true", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if strings.TrimSpace(text) == "" {
			text = "（空）"
		}
		return true, clipToken(text), nil
	}
	return true, clipToken(string(bytes.TrimSpace(raw))), nil
}

func isNull(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func clipToken(text string) string {
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", " ")
	runes := 0
	for i := range text {
		if runes == 80 {
			return text[:i]
		}
		runes++
	}
	return text
}
