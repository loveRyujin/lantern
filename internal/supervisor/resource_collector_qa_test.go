//go:build darwin

package supervisor

import (
	"errors"
	"testing"
	"time"
)

// These are synthetic collector checks, separate from real native Run proofs.
func TestResourceQACollectorRetainsRawDecreaseAndFailsAtCapacity(t *testing.T) {
	u := ResourceUsage{GoMonotonicNS: 100}
	m := resourceMeter{u: &u}
	for _, s := range []ResourceSample{
		{Phase: "running", MonotonicNS: 110, Valid: true, CPU: 100, RSSBytes: 8},
		{Phase: "running", MonotonicNS: 120, Valid: false, Error: "injected read failure"},
		{Phase: "running", MonotonicNS: 150, Valid: true, CPU: 50, RSSBytes: 6},
	} {
		if err := m.record(s); err != nil {
			t.Fatal(err)
		}
	}
	if u.CPU != 50 || u.PeakRSSBytes != 8 || u.MaxSampleGap != 40 || len(u.Samples) != 3 || u.Samples[1].Valid || u.Samples[1].Error == "" {
		t.Fatalf("collector erased raw decrease/failure or fabricated sample gap: %+v", u)
	}
	for len(u.Samples) < MaxResourceSamples-1 {
		if err := m.record(ResourceSample{Error: "synthetic capacity fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	err := m.record(ResourceSample{Error: "last retained sample"})
	if !errors.Is(err, ErrMonitor) || len(u.Samples) != MaxResourceSamples || u.Samples[MaxResourceSamples-1].Error != "last retained sample" {
		t.Fatal("full capacity silently dropped samples or monitoring failure")
	}
	if err := m.record(ResourceSample{}); !errors.Is(err, ErrMonitor) || len(u.Samples) != MaxResourceSamples {
		t.Fatal("collector exceeded bounded capacity")
	}
}

func TestResourceQACollectorActualWaitCutsActiveWindow(t *testing.T) {
	u := ResourceUsage{MonitoringStarted: true, GoMonotonicNS: 100, ReceiptMonotonicNS: 400, TriggerMonotonicNS: 350, TriggerReason: "cpu_limit", Samples: []ResourceSample{
		{Phase: "pre-go", MonotonicNS: 90, Valid: true, CPU: 900, RSSBytes: 900},
		{Phase: "running", MonotonicNS: 110, Valid: true, CPU: 100, RSSBytes: 30, VirtualBytes: 90},
		{Phase: "running", MonotonicNS: 200, Valid: true, CPU: 80, RSSBytes: 20, VirtualBytes: 80},
		{Phase: "running", MonotonicNS: 350, Valid: true, CPU: 500, RSSBytes: 500, VirtualBytes: 900},
	}}
	m := resourceMeter{u: &u, budget: ResourceBudget{CPU: 50, RSSBytes: 25}, clock: &resourceClock{now: func() uint64 { return 400 }, numer: 1, denom: 1}}
	if err := m.finish(&receipt{Waited: true, WaitMonotonicNS: 250}); err != nil {
		t.Fatal(err)
	}
	if u.CPU != 80 || u.PeakRSSBytes != 30 || u.PeakVirtualBytes != 90 || u.CPUOvershoot != 50 || u.RSSOvershootBytes != 5 || u.MaxSampleGap != 90 || u.GoToFirstSample != 10 || u.LastSampleToWait != 50 || u.Wall != 150*time.Nanosecond || u.SuccessfulSamples != 2 || len(u.Samples) != 4 {
		t.Fatalf("preGO/postWait samples contaminated active accounting: %+v", u)
	}
	if u.TriggerReason != "" || u.TriggerMonotonicNS != 0 || u.LateStopReason != "cpu_limit" || u.LateStopMonotonicNS != 350 {
		t.Fatal("post-Wait stop became primary enforcement")
	}
}
