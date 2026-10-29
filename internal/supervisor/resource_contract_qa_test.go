package supervisor

import (
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestResourceQABudgetDefaultsArePerFieldAndNeverClamp(t *testing.T) {
	for _, test := range []struct {
		name string
		in   ResourceBudget
		want ResourceBudget
	}{
		{"zero-defaults", ResourceBudget{}, ResourceBudget{CPU: time.Second, RSSBytes: 128 << 20, Wall: 3 * time.Second}},
		{"cpu-only", ResourceBudget{CPU: 250 * time.Millisecond}, ResourceBudget{CPU: 250 * time.Millisecond, RSSBytes: 128 << 20, Wall: 3 * time.Second}},
		{"rss-only", ResourceBudget{RSSBytes: 64 << 20}, ResourceBudget{CPU: time.Second, RSSBytes: 64 << 20, Wall: 3 * time.Second}},
		{"wall-only", ResourceBudget{Wall: 250 * time.Millisecond}, ResourceBudget{CPU: time.Second, RSSBytes: 128 << 20, Wall: 250 * time.Millisecond}},
		{"exact-hard-ceilings", ResourceBudget{CPU: 3 * time.Second, RSSBytes: 512 << 20, Wall: 5 * time.Second}, ResourceBudget{CPU: 3 * time.Second, RSSBytes: 512 << 20, Wall: 5 * time.Second}},
		{"small-positive-is-not-default", ResourceBudget{CPU: time.Nanosecond, RSSBytes: 1, Wall: time.Nanosecond}, ResourceBudget{CPU: time.Nanosecond, RSSBytes: 1, Wall: time.Nanosecond}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := test.in
			got, err := normalizeBudget(input)
			if err != nil || got != test.want || input != test.in {
				t.Fatalf("budget normalization changed scope or clamped values: got=%+v want=%+v error=%v", got, test.want, err)
			}
		})
	}
}

func TestResourceQAInvalidBudgetsRejectBeforeAnyActorOrFiles(t *testing.T) {
	for name, b := range map[string]ResourceBudget{
		"negative-cpu":      {CPU: -time.Nanosecond},
		"negative-wall":     {Wall: -time.Nanosecond},
		"cpu-over-ceiling":  {CPU: 3*time.Second + time.Nanosecond},
		"wall-over-ceiling": {Wall: 5*time.Second + time.Nanosecond},
		"rss-over-ceiling":  {RSSBytes: (512 << 20) + 1},
		"cpu-max-int64":     {CPU: time.Duration(math.MaxInt64)},
		"wall-min-int64":    {Wall: time.Duration(math.MinInt64)},
		"rss-max-uint64":    {RSSBytes: math.MaxUint64},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeBudget(b); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("invalid budget was normalized or silently clamped: %+v %v", b, err)
			}
			base := t.TempDir()
			root, evidence := filepath.Join(base, "unused-work"), filepath.Join(base, "unused-evidence")
			r, err := New(Config{TempRoot: root, EvidenceRoot: evidence}).Run(t.Context(), Request{Kind: GoResource, Mode: Normal, Budget: b})
			if !errors.Is(err, ErrInvalidRequest) || r.State != Failed {
				t.Fatalf("invalid real API request did not fail before preparation: %+v %v", r, err)
			}
			qaSupervisorUntouched(t, r, root, evidence)
			if r.Usage.MonitoringReady || r.Usage.MonitoringStarted || len(r.Usage.Samples) != 0 {
				t.Fatal("invalid budget falsely claimed installed monitoring")
			}
		})
	}
}

func TestResourceQASealedProfileCannotEnableMeterOnLifecycleOrUseGates(t *testing.T) {
	c := Config{TempRoot: t.TempDir()}
	for _, mode := range []Mode{Normal, CPUBusy, LowCPUSleep, TouchMemory, UntouchedMemory, MultiThreadCPU} {
		if err := validateRequest(c, Request{Kind: GoResource, Mode: mode}); err != nil {
			t.Fatalf("frozen sealed resource load %q rejected: %v", mode, err)
		}
	}
	if err := validateRequest(c, Request{Kind: GoLifecycle, Mode: Normal}); err != nil {
		t.Fatal("old lifecycle zero-budget profile regressed: ", err)
	}
	for _, gate := range []Gate{BeforeSpawn, AfterSpawn, Registered, Armed} {
		if err := validateRequest(c, Request{Kind: GoResource, Mode: Normal, Gate: gate}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("resource helper accepted a test pause that could escape its Wall start: gate=%q error=%v", gate, err)
		}
	}
	for _, mode := range []Mode{Hold, SetSID, SetPGID, ExitRace, "arbitrary-code"} {
		if err := validateRequest(c, Request{Kind: GoResource, Mode: mode}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("resource profile accepted an unregistered load: %q %v", mode, err)
		}
	}
	for _, b := range []ResourceBudget{{CPU: time.Second}, {RSSBytes: 1}, {Wall: time.Second}} {
		if err := validateRequest(c, Request{Kind: GoLifecycle, Mode: Normal, Budget: b}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("caller budget was silently ignored on the unmetered lifecycle profile: %+v %v", b, err)
		}
	}
	for _, mode := range []Mode{CPUBusy, LowCPUSleep, TouchMemory, UntouchedMemory, MultiThreadCPU} {
		if err := validateRequest(c, Request{Kind: GoLifecycle, Mode: mode}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("resource load leaked into old lifecycle profile: %q %v", mode, err)
		}
	}
}

func TestResourceQAStopCausesAreStableAndDistinct(t *testing.T) {
	for reason, want := range map[string]error{"cpu_limit": ErrCPULimit, "rss_limit": ErrRSSLimit, "wall_limit": ErrWallLimit, "monitor_error": ErrMonitor} {
		got := resourceCause(reason)
		if !errors.Is(got, want) {
			t.Fatalf("typed resource cause lost: reason=%s error=%v", reason, got)
		}
		for _, other := range []error{ErrCPULimit, ErrRSSLimit, ErrWallLimit, ErrMonitor, ErrPeerLost, ErrProtocol} {
			if other != want && errors.Is(got, other) {
				t.Fatalf("resource cause conflated %s with %v", reason, other)
			}
		}
	}
	if resourceCause("cancel") != nil || resourceCause("unknown") != nil {
		t.Fatal("caller cancellation or an arbitrary string was fabricated as a resource breach")
	}
	if ResourceSampleInterval != 10*time.Millisecond || ResourceReadFailureWindow != 50*time.Millisecond || MaxResourceSamples != 1024 {
		t.Fatal("declared sampling/read-failure/evidence bounds differ from the frozen contract")
	}
}
