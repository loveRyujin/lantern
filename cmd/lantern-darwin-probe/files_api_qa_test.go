package main

import (
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/internal/supervisor"
)

func qaFileScan(files supervisor.FileScan) supervisor.FileScan {
	files.Scope = supervisor.FileScanScope
	return files
}

func qaFileObservation(spec fileAPISpec) fileAPICase {
	b := spec.Request.Budget
	c := fileAPICase{ID: spec.ID, WallMS: 180, Request: spec.Request, ExpectedReason: spec.WantReason, ExpectedAxis: spec.WantAxis, ExpectedConclusion: spec.WantResult}
	const goNS = uint64(2_000_000_000)
	r := supervisor.Result{Budget: b, HostPID: 1001, GuardianPID: 1002, Nonce: strings.Repeat("ab", 16), Registered: true, Armed: true, GoAuthorized: true, State: supervisor.Completed, HostWaitedGuardian: true, GuardianWaitedTask: true, GuardianAbsent: true, TaskAbsent: true, WorkRootClean: true}
	r.Task = supervisor.Identity{PID: 1003, StartSeconds: 1700000000, StartMicroseconds: 5, Nonce: r.Nonce, Image: "/owned/sealed-file-helper"}
	r.Worker = supervisor.WorkerResult{Entered: true, PID: r.Task.PID, Mode: spec.Request.Mode}
	at := func(ms int) uint64 { return goNS + uint64(time.Duration(ms)*time.Millisecond) }
	sample := func(ms int, files supervisor.FileScan) supervisor.ResourceSample {
		return supervisor.ResourceSample{PID: r.Task.PID, Phase: "running", MonotonicNS: at(ms), Valid: true, CPU: 20 * time.Millisecond, RSSBytes: 20 << 20, VirtualBytes: 1 << 30, Files: qaFileScan(files)}
	}
	pre := supervisor.ResourceSample{PID: r.Task.PID, Phase: "pre-go", MonotonicNS: goNS - 1, Valid: true, CPU: 10 * time.Millisecond, RSSBytes: 20 << 20, VirtualBytes: 1 << 30, Files: qaFileScan(supervisor.FileScan{})}
	var samples []supervisor.ResourceSample
	var trace []supervisor.FileCheckpoint
	switch spec.ID {
	case "file-small":
		final := qaFileScan(supervisor.FileScan{LogicalNamed: supervisor.FileSmallCount * supervisor.FileSmallEach, LogicalUnique: supervisor.FileSmallCount * supervisor.FileSmallEach, Allocated: 32 << 10, Entries: 4, UniqueInodes: 2})
		samples = []supervisor.ResourceSample{sample(30, final), sample(90, final)}
		trace = []supervisor.FileCheckpoint{{Phase: "entered"}, {Phase: "steady", Written: supervisor.FileSmallCount * supervisor.FileSmallEach}}
	case "file-multi", "file-grow":
		under := qaFileScan(supervisor.FileScan{Allocated: b.FileBytes / 2, LogicalNamed: b.FileBytes / 2, LogicalUnique: b.FileBytes / 2, Entries: 3, UniqueInodes: 1})
		over := under
		over.Allocated = b.FileBytes + 16<<10
		over.LogicalNamed, over.LogicalUnique = over.Allocated, over.Allocated
		samples = []supervisor.ResourceSample{sample(30, under), sample(90, over)}
		r.State, r.StopReason, r.Worker.ExitCode, r.Worker.Signal = supervisor.Failed, "file_limit", -1, 9
		c.Error, c.ErrorKinds = "supervisor sampled file budget exceeded", []string{"file_limit"}
		trace = []supervisor.FileCheckpoint{{Phase: "entered"}}
	case "file-entries":
		under := qaFileScan(supervisor.FileScan{Entries: 4, UniqueInodes: 2})
		over := qaFileScan(supervisor.FileScan{Entries: 12, UniqueInodes: 10})
		samples = []supervisor.ResourceSample{sample(30, under), sample(90, over)}
		r.State, r.StopReason, r.Worker.ExitCode, r.Worker.Signal = supervisor.Failed, "file_limit", -1, 9
		c.Error, c.ErrorKinds = "supervisor sampled file budget exceeded", []string{"file_limit"}
		trace = []supervisor.FileCheckpoint{{Phase: "entered"}}
	case "file-unlink":
		named := qaFileScan(supervisor.FileScan{Allocated: 16 << 10, LogicalNamed: supervisor.FileUnlinkNamed, LogicalUnique: supervisor.FileUnlinkNamed, Entries: 3, UniqueInodes: 1})
		gone := qaFileScan(supervisor.FileScan{Entries: 2})
		samples = []supervisor.ResourceSample{sample(30, named), sample(90, gone)}
		base := supervisor.FileCheckpoint{Dev: 7, Ino: 99}
		namedCP := base
		namedCP.Phase, namedCP.Written, namedCP.Live, namedCP.NLink, namedCP.Allocated = "named", supervisor.FileUnlinkNamed, true, 1, 16<<10
		namedCP.LogicalNamed, namedCP.LogicalUnique, namedCP.Entries, namedCP.UniqueInodes = supervisor.FileUnlinkNamed, supervisor.FileUnlinkNamed, 3, 1
		unlinked := namedCP
		unlinked.Phase, unlinked.Unlinked, unlinked.NLink, unlinked.Allocated, unlinked.Entries, unlinked.UniqueInodes = "unlinked", true, 0, 0, 2, 0
		unlinked.LogicalNamed, unlinked.LogicalUnique = 0, 0
		grew := unlinked
		grew.Phase, grew.Written = "grew", supervisor.FileUnlinkNamed+supervisor.FileUnlinkExtra
		closed := grew
		closed.Phase, closed.Live, closed.Closed = "closed", false, true
		trace = []supervisor.FileCheckpoint{{Phase: "entered"}, namedCP, unlinked, grew, closed}
	case "file-edges":
		unique := uint64(supervisor.FileSparseLogical + 2*supervisor.FileEdgeEach)
		final := qaFileScan(supervisor.FileScan{LogicalUnique: unique, LogicalNamed: unique + supervisor.FileEdgeEach, Allocated: 48 << 10, Entries: 8, UniqueInodes: 3, Symlinks: 2})
		samples = []supervisor.ResourceSample{sample(30, final), sample(90, final)}
		trace = []supervisor.FileCheckpoint{{Phase: "entered"}, {Phase: "steady", Written: supervisor.FileEdgeEach*2 + 1, LogicalNamed: final.LogicalNamed, LogicalUnique: final.LogicalUnique, Allocated: final.Allocated, Entries: 8, UniqueInodes: 3, Symlinks: 2, SameInodeNames: 2, RenameSameInode: true}}
	default:
		panic(spec.ID)
	}
	u := supervisor.ResourceUsage{MonitoringReady: true, MonitoringStarted: true, TimebaseNumer: 125, TimebaseDenom: 3, GoMonotonicNS: goNS, WaitMonotonicNS: at(95), ReceiptMonotonicNS: at(98), FileAlgorithm: supervisor.FileBudgetAlgorithm, FileCandidate: supervisor.FileBudgetCandidate, FileTrace: trace}
	u.Samples = append([]supervisor.ResourceSample{pre}, samples...)
	u.Wall = time.Duration(u.WaitMonotonicNS - u.GoMonotonicNS)
	u.ReceiptDelay = time.Duration(u.ReceiptMonotonicNS - u.WaitMonotonicNS)
	r.Usage = u
	c.Result = r
	running := fileRunning(c)
	pa, pn, pu, pe, _, _ := filePeaks(running)
	c.Result.Usage.PeakFileAllocated, c.Result.Usage.PeakFileLogicalNamed, c.Result.Usage.PeakFileLogicalUnique, c.Result.Usage.PeakFileEntries = pa, pn, pu, pe
	if pa > b.FileBytes {
		c.Result.Usage.FileOvershootBytes = pa - b.FileBytes
	}
	if b.FileEntries > 0 && pe > b.FileEntries {
		c.Result.Usage.FileOvershootEntries = pe - b.FileEntries
	}
	if r.StopReason == "file_limit" {
		c.Result.Usage.TriggerReason = "file_limit"
		c.Result.Usage.TriggerMonotonicNS = at(90)
		c.Result.Usage.FileTriggerAxis = spec.WantAxis
		c.Result.Usage.SignalAttemptMonotonicNS = at(90) + 1
		c.Result.Usage.SignalReturnMonotonicNS = at(90) + 2
		c.Result.Usage.SignalSent = true
	}
	c.Conclusion, c.BudgetProtectedDuringRun = fileConclusion(c.Result)
	return c
}

func TestFileAPIOracleAcceptsDerivedObservations(t *testing.T) {
	for _, spec := range fileAPISpecs() {
		t.Run(spec.ID, func(t *testing.T) {
			c := qaFileObservation(spec)
			if c.Conclusion != spec.WantResult || c.BudgetProtectedDuringRun {
				t.Fatalf("conclusion %q protected %v", c.Conclusion, c.BudgetProtectedDuringRun)
			}
			for name, ok := range fileCaseChecks(c, 1001) {
				if !ok {
					t.Fatalf("check %s failed", name)
				}
			}
		})
	}
}

func TestFileAPIOracleRejectsCleanupAsUnlinkProtection(t *testing.T) {
	var spec fileAPISpec
	for _, s := range fileAPISpecs() {
		if s.ID == "file-unlink" {
			spec = s
		}
	}
	c := qaFileObservation(spec)
	c.BudgetProtectedDuringRun = true
	checks := fileCaseChecks(c, 1001)
	if checks["conclusion"] || checks["cleanup_is_not_runtime_protection"] || checks["unlink_open_gap"] == false && c.Result.WorkRootClean {
		t.Fatalf("cleanup or a protected flag was accepted: %+v", checks)
	}
	c = qaFileObservation(spec)
	for i := range c.Result.Usage.FileTrace {
		if c.Result.Usage.FileTrace[i].Phase == "grew" {
			c.Result.Usage.FileTrace[i].Allocated = c.Result.Usage.FileTrace[i].Written
		}
	}
	c.Conclusion, c.BudgetProtectedDuringRun = fileConclusion(c.Result)
	if c.Conclusion == "unresolved-unlinked-open" {
		t.Fatal("a scan that still showed the unlinked bytes was called unresolved")
	}
}

func TestFileAPIRecordsPRHeadSeparatelyFromCheckout(t *testing.T) {
	checkout := strings.Repeat("ab", 20)
	t.Setenv("LANTERN_PR_HEAD", "")
	got, err := notePRHead(checkout)
	if err != nil || got != checkout {
		t.Fatalf("missing PR head should keep the checkout SHA: %s %v", got, err)
	}
	want := strings.Repeat("cd", 20)
	t.Setenv("LANTERN_PR_HEAD", want)
	got, err = notePRHead(checkout)
	if err != nil || got != want {
		t.Fatalf("PR head was not kept beside the checkout: %s %v", got, err)
	}
	t.Setenv("LANTERN_PR_HEAD", "not-a-sha")
	if _, err = notePRHead(checkout); err == nil {
		t.Fatal("a non-sha PR head was accepted")
	}
}

func TestFileAPIOracleRejectsNaturalCompletionOverCandidateBudget(t *testing.T) {
	var spec fileAPISpec
	for _, s := range fileAPISpecs() {
		if s.ID == "file-grow" {
			spec = s
		}
	}
	c := qaFileObservation(spec)
	c.Result.State = supervisor.Completed
	c.Result.StopReason = ""
	c.Result.Worker.ExitCode = 0
	c.Result.Worker.Signal = 0
	c.Result.Usage.TriggerReason = ""
	c.Result.Usage.FileTriggerAxis = ""
	c.Error, c.ErrorKinds = "", nil
	c.Conclusion, c.BudgetProtectedDuringRun = fileConclusion(c.Result)
	if c.Conclusion == "named-file-budget-enforced" {
		t.Fatal("a finished helper still counted as an enforced stop")
	}
	checks := fileCaseChecks(c, 1001)
	if checks["termination"] || checks["conclusion"] {
		t.Fatalf("oracle accepted a removed candidate stop: %+v conclusion %s", checks, c.Conclusion)
	}
}
