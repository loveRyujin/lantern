package policy_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/policy"
)

func TestSandboxOverDefaultNeedsConfirm(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		args string
		want string
	}{
		{
			name: "cpu",
			args: `{"code":"print(1)","limits":{"cpu_ms":20000}}`,
			want: "沙箱申请超出默认权限：CPU 20000ms > 默认 10000ms",
		},
		{
			name: "memory",
			args: `{"code":"print(1)","limits":{"memory_bytes":805306368}}`,
			want: "沙箱申请超出默认权限：内存 805306368B > 默认 536870912B",
		},
		{
			name: "timeout",
			args: `{"code":"print(1)","limits":{"wall_ms":45000}}`,
			want: "沙箱申请超出默认权限：超时 45000ms > 默认 30000ms",
		},
		{
			name: "cpu at hard ceiling",
			args: `{"limits":{"cpu_ms":30000}}`,
			want: "沙箱申请超出默认权限：CPU 30000ms > 默认 10000ms",
		},
		{
			name: "timeout at hard ceiling",
			args: `{"limits":{"wall_ms":60000}}`,
			want: "沙箱申请超出默认权限：超时 60000ms > 默认 30000ms",
		},
		{
			name: "memory at hard ceiling",
			args: `{"limits":{"memory_bytes":1073741824}}`,
			want: "沙箱申请超出默认权限：内存 1073741824B > 默认 536870912B",
		},
		{
			name: "one mount",
			args: `{"code":"print(1)","extra_read_only":["/data/ro"]}`,
			want: "沙箱申请超出默认权限：额外只读目录 /data/ro（默认无额外挂载）",
		},
		{
			name: "mount order",
			args: `{"extra_read_only":["/data/ro","/var/extra"]}`,
			want: "沙箱申请超出默认权限：额外只读目录 /data/ro、/var/extra（默认无额外挂载）",
		},
		{
			name: "relative mount still confirms",
			args: `{"extra_read_only":["relative"]}`,
			want: "沙箱申请超出默认权限：额外只读目录 relative（默认无额外挂载）",
		},
		{
			name: "cpu and mount",
			args: `{"limits":{"cpu_ms":20000},"extra_read_only":["/data/ro"]}`,
			want: "沙箱申请超出默认权限：CPU 20000ms > 默认 10000ms；额外只读目录 /data/ro（默认无额外挂载）",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.args)
			before := append([]byte(nil), raw...)
			got, err := policy.Default().Decide(ctx, policy.Request{Tool: policy.ToolRunPython, Args: raw})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, before) {
				t.Fatal("策略改写了参数副本")
			}
			if !got.NeedsConfirm || got.Deny || got.Reason != tc.want || got.Risk != tc.want {
				t.Fatalf("判定 = %+v，想要 %s", got, tc.want)
			}
		})
	}
}

func TestSandboxNetworkAndHardCapDeny(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		args string
		want string
	}{
		{
			name: "bool",
			args: `{"code":"print(1)","network":true}`,
			want: "拒绝沙箱网络申请：true。默认禁网，回环也不能通过确认放开",
		},
		{
			name: "loopback",
			args: `{"network":"loopback"}`,
			want: "拒绝沙箱网络申请：loopback。默认禁网，回环也不能通过确认放开",
		},
		{
			name: "ipv4",
			args: `{"network":"127.0.0.1"}`,
			want: "拒绝沙箱网络申请：127.0.0.1。默认禁网，回环也不能通过确认放开",
		},
		{
			name: "ipv6",
			args: `{"network":"::1"}`,
			want: "拒绝沙箱网络申请：::1。默认禁网，回环也不能通过确认放开",
		},
		{
			name: "localhost",
			args: `{"network":"localhost"}`,
			want: "拒绝沙箱网络申请：localhost。默认禁网，回环也不能通过确认放开",
		},
		{
			name: "string false is still a request",
			args: `{"network":"false"}`,
			want: "拒绝沙箱网络申请：false。默认禁网，回环也不能通过确认放开",
		},
		{
			name: "object",
			args: `{"network":{"mode":"loopback"}}`,
			want: `拒绝沙箱网络申请：{"mode":"loopback"}。默认禁网，回环也不能通过确认放开`,
		},
		{
			name: "cpu above hard",
			args: `{"limits":{"cpu_ms":30001}}`,
			want: "拒绝沙箱资源申请：CPU 30001ms 超过硬上限 30000ms（默认 10000ms），不能通过确认放开",
		},
		{
			name: "timeout above hard",
			args: `{"limits":{"wall_ms":60001}}`,
			want: "拒绝沙箱资源申请：超时 60001ms 超过硬上限 60000ms（默认 30000ms），不能通过确认放开",
		},
		{
			name: "memory above hard",
			args: `{"limits":{"memory_bytes":1073741825}}`,
			want: "拒绝沙箱资源申请：内存 1073741825B 超过硬上限 1073741824B（默认 536870912B），不能通过确认放开",
		},
		{
			name: "nproc above single-task ceiling",
			args: `{"limits":{"nproc":2}}`,
			want: "拒绝沙箱资源申请：进程数 2 超过硬上限 1（默认 1），不能通过确认放开",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := policy.Default().Decide(ctx, policy.Request{Tool: policy.ToolRunPython, Args: []byte(tc.args)})
			if err != nil {
				t.Fatal(err)
			}
			if got.NeedsConfirm || !got.Deny || got.Reason != tc.want || got.Risk != tc.want {
				t.Fatalf("判定 = %+v，想要 %s", got, tc.want)
			}
		})
	}
}

func TestSandboxDenyWinsOverConfirmableFields(t *testing.T) {
	ctx := context.Background()
	network, err := policy.Default().Decide(ctx, policy.Request{
		Tool: policy.ToolRunPython,
		Args: []byte(`{"network":"loopback","extra_read_only":["/data/ro"],"limits":{"wall_ms":45000}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if network.NeedsConfirm || !network.Deny || !strings.Contains(network.Reason, "loopback") || strings.Contains(network.Reason, "额外只读目录") {
		t.Fatalf("网络申请仍可能进入确认: %+v", network)
	}
	hard, err := policy.Default().Decide(ctx, policy.Request{
		Tool: policy.ToolRunPython,
		Args: []byte(`{"limits":{"cpu_ms":30001,"wall_ms":45000},"extra_read_only":["/data/ro"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if hard.NeedsConfirm || !hard.Deny || !strings.Contains(hard.Reason, "30001ms") || strings.Contains(hard.Reason, "45000ms") {
		t.Fatalf("超硬上限仍可能进入确认: %+v", hard)
	}
}

func TestSandboxDefaultAllowanceIsNotBlocked(t *testing.T) {
	ctx := context.Background()
	for _, args := range []string{
		``,
		`null`,
		`{"code":"print(1)"}`,
		`{"code":"print(1)","sql_call_id":"query-1"}`,
		`{"code":"print(1)","network":false}`,
		`{"code":"print(1)","extra_read_only":[]}`,
		`{"code":"print(1)","limits":{}}`,
		`{"code":"print(1)","limits":{"cpu_ms":10000,"memory_bytes":536870912,"wall_ms":30000,"nproc":1}}`,
		`{"code":"print(1)","limits":{"cpu_ms":1000,"memory_bytes":1,"wall_ms":1,"nproc":0}}`,
		`{"code":"print(1)","limits":{"cpu_ms":0,"memory_bytes":0,"wall_ms":0,"nproc":0},"network":false,"extra_read_only":null}`,
		`{"code":"print(1)","approved":true,"requires_approval":false,"needs_confirm":true,"confirmed":true}`,
	} {
		got, err := policy.Default().Decide(ctx, policy.Request{Tool: policy.ToolRunPython, Args: []byte(args)})
		if err != nil || got.NeedsConfirm || got.Deny || got.Reason != "" || got.Risk != "" {
			t.Fatalf("默认权限被误拦 %s: %+v %v", args, got, err)
		}
	}
}

func TestSandboxModelApprovalFlagsCannotBypass(t *testing.T) {
	ctx := context.Background()
	confirm, err := policy.Default().Decide(ctx, policy.Request{
		Tool: policy.ToolRunPython,
		Args: []byte(`{"code":"print(1)","limits":{"wall_ms":45000},"requires_approval":false,"approved":true,"needs_confirm":false,"confirmed":true}`),
	})
	if err != nil || !confirm.NeedsConfirm || confirm.Deny || !strings.Contains(confirm.Risk, "45000ms") || !strings.Contains(confirm.Risk, "30000ms") {
		t.Fatalf("批准标记绕过了超权限确认: %+v %v", confirm, err)
	}
	deny, err := policy.Default().Decide(ctx, policy.Request{
		Tool: policy.ToolRunPython,
		Args: []byte(`{"code":"print(1)","network":"127.0.0.1","approved":true,"requires_approval":false}`),
	})
	if err != nil || deny.NeedsConfirm || !deny.Deny || !strings.Contains(deny.Reason, "127.0.0.1") {
		t.Fatalf("批准标记绕过了网络拒绝: %+v %v", deny, err)
	}
}

func TestStaticNameRulesIgnoreSandboxArgs(t *testing.T) {
	ctx := context.Background()
	bare, err := policy.Static{}.Decide(ctx, policy.Request{
		Tool: policy.ToolRunPython,
		Args: []byte(`{"network":true,"limits":{"wall_ms":45000}}`),
	})
	if err != nil || bare.NeedsConfirm || bare.Deny {
		t.Fatalf("Static 读了 Args: %+v %v", bare, err)
	}
	for _, tool := range []string{"export_data", "write_action"} {
		got, err := policy.Default().Decide(ctx, policy.Request{
			Tool: tool,
			Args: []byte(`{"network":true,"approved":true,"requires_approval":false}`),
		})
		if err != nil || !got.NeedsConfirm || got.Deny || got.Reason != "该动作需要本人确认" || got.Risk != "将执行 "+tool {
			t.Fatalf("%s 的名字规则变了: %+v %v", tool, got, err)
		}
	}
	read, err := policy.Default().Decide(ctx, policy.Request{
		Tool: "run_sql",
		Args: []byte(`{"sql":"select 1","network":true,"approved":true}`),
	})
	if err != nil || read.NeedsConfirm || read.Deny {
		t.Fatalf("非沙箱工具被超权限规则误拦: %+v %v", read, err)
	}
}

func TestSandboxArgsMustStayWellFormed(t *testing.T) {
	ctx := context.Background()
	for _, args := range []string{
		`{`,
		`[]`,
		`"x"`,
		`{"code":"x","inputs":{"a":"b"}}`,
		`{"limits":{"memory":1}}`,
		`{"limits":[]}`,
		`{"limits":{"cpu_ms":-1}}`,
		`{"limits":{"cpu_ms":1.5}}`,
		`{"extra_read_only":" / "}`,
		`{"extra_read_only":[""]}`,
	} {
		got, err := policy.Default().Decide(ctx, policy.Request{Tool: policy.ToolRunPython, Args: []byte(args)})
		if err == nil || got.NeedsConfirm || got.Deny {
			t.Fatalf("坏参数被放行 %s: %+v %v", args, got, err)
		}
	}
}

func TestSandboxPolicyObservesCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := policy.Default().Decide(ctx, policy.Request{Tool: policy.ToolRunPython, Args: []byte(`{"network":true}`)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消没有传回来: %v", err)
	}
}

func TestSandboxGrantLimitsStayInsideHardCeiling(t *testing.T) {
	grant, err := policy.ParseRunPython([]byte(`{"code":"print(1)","limits":{"wall_ms":45000,"cpu_ms":20000},"extra_read_only":["/data/ro"]}`))
	if err != nil {
		t.Fatal(err)
	}
	cpu, memory, nproc, wall, err := grant.Limits()
	if err != nil || cpu != 20*time.Second || wall != 45*time.Second || memory != 0 || nproc != 0 || len(grant.ExtraReadOnly) != 1 {
		t.Fatalf("可确认申请没有写成限额: cpu=%s wall=%s mem=%d nproc=%d err=%v", cpu, wall, memory, nproc, err)
	}
	over, err := policy.ParseRunPython([]byte(`{"limits":{"cpu_ms":30001}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := over.Limits(); err == nil {
		t.Fatal("超硬上限被写成了沙箱限额")
	}
	net, err := policy.ParseRunPython([]byte(`{"network":"::1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := net.Limits(); err == nil || !net.Network {
		t.Fatal("网络申请被写成了沙箱限额")
	}
}
