package sandbox

import (
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/policy"
)

func TestRunnerCeilingsMatchPolicy(t *testing.T) {
	spec, err := normalize(Spec{Code: "pass"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Limits.CPU != time.Duration(policy.DefaultCPUMillis)*time.Millisecond ||
		spec.Limits.Wall != time.Duration(policy.DefaultWallMillis)*time.Millisecond ||
		spec.Limits.Memory != uint64(policy.DefaultMemoryBytes) ||
		spec.Limits.NProc != uint64(policy.DefaultNProc) {
		t.Fatalf("沙箱默认值和 Policy 不一致: %+v", spec.Limits)
	}
	accepted := []Limits{
		{CPU: time.Duration(policy.HardCPUMillis) * time.Millisecond},
		{Wall: time.Duration(policy.HardWallMillis) * time.Millisecond},
		{Memory: uint64(policy.HardMemoryBytes)},
		{NProc: uint64(policy.HardNProc)},
	}
	for _, limits := range accepted {
		if _, err := normalize(Spec{Code: "pass", Limits: limits}); err != nil {
			t.Fatalf("硬上限应仍可申请 %+v: %v", limits, err)
		}
	}
	rejected := []Limits{
		{CPU: time.Duration(policy.HardCPUMillis+1) * time.Millisecond},
		{Wall: time.Duration(policy.HardWallMillis+1) * time.Millisecond},
		{Memory: uint64(policy.HardMemoryBytes + 1)},
		{NProc: uint64(policy.HardNProc + 1)},
	}
	for _, limits := range rejected {
		if _, err := normalize(Spec{Code: "pass", Limits: limits}); err == nil {
			t.Fatalf("超过硬上限仍被沙箱接受: %+v", limits)
		}
	}
}
