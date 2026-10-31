package main

import (
	"time"

	"github.com/loveRyujin/lantern/internal/supervisor"
)

const fileAPISchema = "lantern.darwin-file-budget.v1"

type fileAPISpec struct {
	ID         string
	Request    supervisor.Request
	WantReason string
	WantAxis   string
	WantResult string
}

func fileLoose(bytes, entries uint64) supervisor.ResourceBudget {
	return supervisor.ResourceBudget{CPU: 3 * time.Second, RSSBytes: 256 << 20, Wall: 3 * time.Second, FileBytes: bytes, FileEntries: entries}
}

func fileAPISpecs() []fileAPISpec {
	return []fileAPISpec{
		{ID: "file-small", Request: supervisor.Request{Kind: supervisor.GoResource, Mode: supervisor.FileSmall, Budget: fileLoose(64<<10, 0)}, WantResult: "within-candidate-budget"},
		{ID: "file-multi", Request: supervisor.Request{Kind: supervisor.GoResource, Mode: supervisor.FileMulti, Budget: fileLoose(40<<10, 0)}, WantReason: "file_limit", WantAxis: "allocated_bytes", WantResult: "named-file-budget-enforced"},
		{ID: "file-entries", Request: supervisor.Request{Kind: supervisor.GoResource, Mode: supervisor.FileEntries, Budget: fileLoose(supervisor.MaxFileBudgetBytes, 8)}, WantReason: "file_limit", WantAxis: "entries", WantResult: "entry-ceiling-enforced"},
		{ID: "file-grow", Request: supervisor.Request{Kind: supervisor.GoResource, Mode: supervisor.FileGrow, Budget: fileLoose(48<<10, 0)}, WantReason: "file_limit", WantAxis: "allocated_bytes", WantResult: "named-file-budget-enforced"},
		{ID: "file-unlink", Request: supervisor.Request{Kind: supervisor.GoResource, Mode: supervisor.FileUnlink, Budget: fileLoose(48<<10, 0)}, WantResult: "unresolved-unlinked-open"},
		{ID: "file-edges", Request: supervisor.Request{Kind: supervisor.GoResource, Mode: supervisor.FileEdges, Budget: fileLoose(supervisor.MaxFileBudgetBytes, 0)}, WantResult: "axes-distinguished"},
	}
}

type fileAPICase struct {
	ValidationError          string             `json:"validation_error,omitempty"`
	ID                       string             `json:"id"`
	Status                   string             `json:"status"`
	Request                  supervisor.Request `json:"request"`
	ExpectedReason           string             `json:"expected_reason,omitempty"`
	ExpectedAxis             string             `json:"expected_axis,omitempty"`
	ExpectedConclusion       string             `json:"expected_conclusion"`
	Conclusion               string             `json:"conclusion"`
	BudgetProtectedDuringRun bool               `json:"budget_protected_during_run"`
	HarnessRescue            bool               `json:"harness_rescue"`
	Result                   supervisor.Result  `json:"result"`
	ErrorKinds               []string           `json:"error_kinds"`
	Error                    string             `json:"error,omitempty"`
	Checks                   map[string]bool    `json:"checks"`
	WallMS                   int64              `json:"wall_ms"`
}

type fileAPIReport struct {
	Schema                      string            `json:"schema"`
	Status                      string            `json:"status"`
	Source                      string            `json:"source_commit"`
	Dirty                       bool              `json:"source_dirty"`
	Environment                 map[string]string `json:"environment"`
	Fingerprints                map[string]string `json:"fingerprints"`
	Algorithm                   string            `json:"algorithm"`
	Candidate                   string            `json:"candidate"`
	Axes                        map[string]string `json:"axes"`
	UnlinkedOpen                string            `json:"unlinked_open"`
	CandidateMetersUnlinkedOpen bool              `json:"candidate_meters_unlinked_open"`
	CleanupIsBudgetProtection   bool              `json:"cleanup_is_budget_protection"`
	CleanupActor                string            `json:"cleanup_actor"`
	HardQuota                   bool              `json:"hard_disk_quota"`
	BusinessBackend             bool              `json:"business_backend_available"`
	HarnessRescue               bool              `json:"harness_rescue"`
	Cases                       []fileAPICase     `json:"cases"`
	Error                       string            `json:"error,omitempty"`
}

func fileErrorKinds(err error) []string {
	return resourceErrorKinds(err)
}

func fileRunning(c fileAPICase) []supervisor.ResourceSample {
	u := c.Result.Usage
	out := []supervisor.ResourceSample{}
	for _, s := range u.Samples {
		if s.Valid && s.Phase == "running" && s.MonotonicNS >= u.GoMonotonicNS && s.MonotonicNS <= u.WaitMonotonicNS && s.PID == c.Result.Task.PID {
			out = append(out, s)
		}
	}
	return out
}

func filePhase(r supervisor.Result, phase string) (supervisor.FileCheckpoint, bool) {
	for _, cp := range r.Usage.FileTrace {
		if cp.Phase == phase {
			return cp, true
		}
	}
	return supervisor.FileCheckpoint{}, false
}

func fileTracePhases(r supervisor.Result) []string {
	out := make([]string, 0, len(r.Usage.FileTrace))
	for _, cp := range r.Usage.FileTrace {
		out = append(out, cp.Phase)
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fileLastRunning(samples []supervisor.ResourceSample) (supervisor.ResourceSample, bool) {
	if len(samples) == 0 {
		return supervisor.ResourceSample{}, false
	}
	return samples[len(samples)-1], true
}

func filePeaks(samples []supervisor.ResourceSample) (allocated, logicalNamed, logicalUnique, entries, byteOver, entryOver uint64) {
	for _, s := range samples {
		if s.Files.Allocated > allocated {
			allocated = s.Files.Allocated
		}
		if s.Files.LogicalNamed > logicalNamed {
			logicalNamed = s.Files.LogicalNamed
		}
		if s.Files.LogicalUnique > logicalUnique {
			logicalUnique = s.Files.LogicalUnique
		}
		if s.Files.Entries > entries {
			entries = s.Files.Entries
		}
	}
	return allocated, logicalNamed, logicalUnique, entries, 0, 0
}

func fileScanDropped(samples []supervisor.ResourceSample) bool {
	seenNamed := false
	for _, s := range samples {
		if s.Files.Scope != supervisor.FileScanScope || s.Files.Followed {
			return false
		}
		if s.Files.Allocated > 0 {
			seenNamed = true
		}
		if seenNamed && s.Files.Allocated == 0 {
			return true
		}
	}
	return false
}

func fileOuterQuiet(c fileAPICase) bool {
	r, u := c.Result, c.Result.Usage
	if u.TriggerMonotonicNS < u.GoMonotonicNS || u.TriggerMonotonicNS > u.WaitMonotonicNS || time.Duration(u.TriggerMonotonicNS-u.GoMonotonicNS) >= r.Budget.Wall {
		return false
	}
	for _, s := range fileRunning(c) {
		if s.MonotonicNS <= u.TriggerMonotonicNS && (s.CPU >= r.Budget.CPU || s.RSSBytes > r.Budget.RSSBytes) {
			return false
		}
	}
	return true
}

func fileAxisHit(c fileAPICase) bool {
	r := c.Result
	for _, s := range fileRunning(c) {
		if s.MonotonicNS > r.Usage.TriggerMonotonicNS || s.Files.Scope != supervisor.FileScanScope || s.Files.Followed {
			continue
		}
		switch r.Usage.FileTriggerAxis {
		case "allocated_bytes":
			if s.Files.Allocated > r.Budget.FileBytes {
				return true
			}
		case "entries":
			if r.Budget.FileEntries > 0 && s.Files.Entries > r.Budget.FileEntries && s.Files.Allocated <= r.Budget.FileBytes {
				return true
			}
		}
	}
	return false
}

// fileConclusion is derived from the result. Cleanup success never produces a
// protected conclusion: an unlinked-open gap stays unresolved.
func fileConclusion(r supervisor.Result) (string, bool) {
	switch {
	case fileUnlinkResolved(r):
		return "unresolved-unlinked-open", false
	case fileNamedStop(r):
		return "named-file-budget-enforced", false
	case fileEntryStop(r):
		return "entry-ceiling-enforced", false
	case fileAxesSplit(r):
		return "axes-distinguished", false
	case fileWithin(r):
		return "within-candidate-budget", false
	default:
		return "observation-incomplete", false
	}
}

func fileUnlinkResolved(r supervisor.Result) bool {
	if r.State != supervisor.Completed || r.StopReason != "" || r.Worker.ExitCode != 0 || r.Usage.TriggerReason != "" {
		return false
	}
	if !sameStrings(fileTracePhases(r), []string{"entered", "named", "unlinked", "grew", "closed"}) {
		return false
	}
	named, unlinked, grew, closed := mustPhases(r)
	if named.Written != supervisor.FileUnlinkNamed || !named.Live || named.Unlinked || named.Closed || named.NLink < 1 || named.Ino == 0 || named.Allocated == 0 || named.Allocated > r.Budget.FileBytes {
		return false
	}
	if !unlinked.Live || !unlinked.Unlinked || unlinked.Closed || unlinked.NLink != 0 || unlinked.Written != named.Written || unlinked.Dev != named.Dev || unlinked.Ino != named.Ino || unlinked.Allocated != 0 {
		return false
	}
	if !grew.Live || !grew.Unlinked || grew.Closed || grew.NLink != 0 || grew.Written != supervisor.FileUnlinkNamed+supervisor.FileUnlinkExtra || grew.Written <= r.Budget.FileBytes || grew.Dev != named.Dev || grew.Ino != named.Ino || grew.Allocated != 0 {
		return false
	}
	if grew.Allocated >= grew.Written || closed.Live || !closed.Closed || !closed.Unlinked || closed.Written != grew.Written || closed.Dev != named.Dev || closed.Ino != named.Ino || closed.Allocated != 0 {
		return false
	}
	running := runningOf(r)
	peak, _, _, _, _, _ := filePeaks(running)
	return peak > 0 && peak < r.Budget.FileBytes && peak < grew.Written && fileScanDropped(running) && r.WorkRootClean
}

func mustPhases(r supervisor.Result) (named, unlinked, grew, closed supervisor.FileCheckpoint) {
	named, _ = filePhase(r, "named")
	unlinked, _ = filePhase(r, "unlinked")
	grew, _ = filePhase(r, "grew")
	closed, _ = filePhase(r, "closed")
	return named, unlinked, grew, closed
}

func runningOf(r supervisor.Result) []supervisor.ResourceSample {
	return fileRunning(fileAPICase{Result: r})
}

func fileNamedStop(r supervisor.Result) bool {
	if r.State != supervisor.Failed || r.StopReason != "file_limit" || r.Usage.TriggerReason != "file_limit" || r.Usage.FileTriggerAxis != "allocated_bytes" || r.Worker.ExitCode != -1 || r.Worker.Signal != 9 {
		return false
	}
	c := fileAPICase{Result: r}
	if !fileOuterQuiet(c) || !fileAxisHit(c) {
		return false
	}
	peak, _, _, _, _, _ := filePeaks(fileRunning(c))
	return peak > r.Budget.FileBytes && r.Usage.FileOvershootBytes == peak-r.Budget.FileBytes
}

func fileEntryStop(r supervisor.Result) bool {
	if r.State != supervisor.Failed || r.StopReason != "file_limit" || r.Usage.FileTriggerAxis != "entries" || r.Budget.FileEntries == 0 || r.Worker.Signal != 9 {
		return false
	}
	c := fileAPICase{Result: r}
	if !fileOuterQuiet(c) || !fileAxisHit(c) {
		return false
	}
	peakAlloc, _, _, peakEntries, _, _ := filePeaks(fileRunning(c))
	return peakEntries > r.Budget.FileEntries && peakAlloc <= r.Budget.FileBytes && r.Usage.FileOvershootEntries == peakEntries-r.Budget.FileEntries
}

func fileAxesSplit(r supervisor.Result) bool {
	if r.State != supervisor.Completed || r.StopReason != "" || r.Worker.ExitCode != 0 {
		return false
	}
	steady, ok := filePhase(r, "steady")
	if !ok || !steady.RenameSameInode || steady.SameInodeNames != 2 || steady.Symlinks != 2 || steady.UniqueInodes != 3 || steady.Entries != 8 {
		return false
	}
	wantUnique := uint64(supervisor.FileSparseLogical + 2*supervisor.FileEdgeEach)
	if steady.LogicalUnique != wantUnique || steady.LogicalNamed != wantUnique+supervisor.FileEdgeEach || steady.Allocated >= r.Budget.FileBytes || steady.LogicalUnique <= r.Budget.FileBytes {
		return false
	}
	running := runningOf(r)
	last, ok := fileLastRunning(running)
	if !ok {
		return false
	}
	peakAlloc, peakNamed, _, _, _, _ := filePeaks(running)
	return last.Files.Entries == 8 && last.Files.Symlinks == 2 && last.Files.UniqueInodes == 3 && last.Files.LogicalUnique == wantUnique && last.Files.LogicalNamed == steady.LogicalNamed && last.Files.Allocated == steady.Allocated && peakNamed > r.Budget.FileBytes && peakAlloc < r.Budget.FileBytes && !last.Files.Followed
}

func fileWithin(r supervisor.Result) bool {
	if r.State != supervisor.Completed || r.StopReason != "" || r.Worker.ExitCode != 0 || r.Usage.TriggerReason != "" {
		return false
	}
	if _, ok := filePhase(r, "unlinked"); ok {
		return false
	}
	if steady, ok := filePhase(r, "steady"); ok && steady.RenameSameInode {
		return false
	}
	running := runningOf(r)
	last, ok := fileLastRunning(running)
	if !ok {
		return false
	}
	peakAlloc, _, peakUnique, _, _, _ := filePeaks(running)
	return peakAlloc > 0 && peakAlloc <= r.Budget.FileBytes && peakUnique == supervisor.FileSmallCount*supervisor.FileSmallEach && peakUnique <= r.Budget.FileBytes && last.Files.LogicalUnique == peakUnique && last.Files.Entries == 4 && last.Files.UniqueInodes == 2 && last.Files.Symlinks == 0 && last.Files.Allocated == peakAlloc
}

func fileCaseChecks(c fileAPICase, hostPID int) map[string]bool {
	r, u := c.Result, c.Result.Usage
	checks := map[string]bool{}
	checks["actual_host_and_task"] = r.HostPID == hostPID && r.Task.PID > 0 && r.Task.PID != hostPID && r.Worker.PID == r.Task.PID && r.Worker.Entered && r.Worker.Mode == c.Request.Mode
	checks["handshake"] = r.Registered && r.Armed && r.GoAuthorized && len(r.Nonce) == 32 && r.Task.Nonce == r.Nonce && r.Task.StartSeconds > 0
	want := c.Request.Budget
	if want.CPU == 0 {
		want.CPU = time.Second
	}
	if want.RSSBytes == 0 {
		want.RSSBytes = 128 << 20
	}
	if want.Wall == 0 {
		want.Wall = 3 * time.Second
	}
	checks["normalized_budget"] = r.Budget == want && want.FileBytes > 0
	running := fileRunning(c)
	scansOK := u.MonitoringReady && u.MonitoringStarted && u.TimebaseNumer > 0 && u.TimebaseDenom > 0 && len(running) >= 2 && len(u.Samples) <= supervisor.MaxResourceSamples
	sawPre := false
	var previous uint64
	for _, s := range u.Samples {
		if s.PID != r.Task.PID || s.MonotonicNS == 0 || previous > 0 && s.MonotonicNS <= previous {
			scansOK = false
		}
		previous = s.MonotonicNS
		if !s.Valid {
			continue
		}
		if s.Files.Scope != supervisor.FileScanScope || s.Files.Followed {
			scansOK = false
		}
		if s.Phase == "pre-go" {
			sawPre = true
			if s.Files.Allocated != 0 || s.Files.Entries != 0 || s.Files.LogicalNamed != 0 || s.Files.LogicalUnique != 0 || s.Files.Symlinks != 0 || s.Files.UniqueInodes != 0 {
				scansOK = false
			}
		}
	}
	checks["monitor_and_private_scan"] = scansOK && sawPre
	checks["wall_scope"] = u.GoMonotonicNS > 0 && u.WaitMonotonicNS >= u.GoMonotonicNS && u.ReceiptMonotonicNS >= u.WaitMonotonicNS && u.Wall == time.Duration(u.WaitMonotonicNS-u.GoMonotonicNS) && u.ReceiptDelay == time.Duration(u.ReceiptMonotonicNS-u.WaitMonotonicNS)
	checks["component_cleanup"] = r.HostWaitedGuardian && r.GuardianWaitedTask && r.TaskAbsent && r.GuardianAbsent && r.WorkRootClean && !r.Worker.ControlFDsInherited
	checks["no_harness_rescue"] = !c.HarnessRescue
	checks["algorithm"] = u.FileAlgorithm == supervisor.FileBudgetAlgorithm && u.FileCandidate == supervisor.FileBudgetCandidate
	peakAlloc, peakNamed, peakUnique, peakEntries, _, _ := filePeaks(running)
	var byteOver, entryOver uint64
	for _, s := range running {
		if s.Files.Allocated > want.FileBytes && s.Files.Allocated-want.FileBytes > byteOver {
			byteOver = s.Files.Allocated - want.FileBytes
		}
		if want.FileEntries > 0 && s.Files.Entries > want.FileEntries && s.Files.Entries-want.FileEntries > entryOver {
			entryOver = s.Files.Entries - want.FileEntries
		}
	}
	checks["usage_matches_scan_window"] = u.PeakFileAllocated == peakAlloc && u.PeakFileLogicalNamed == peakNamed && u.PeakFileLogicalUnique == peakUnique && u.PeakFileEntries == peakEntries && u.FileOvershootBytes == byteOver && u.FileOvershootEntries == entryOver
	got, protected := fileConclusion(r)
	checks["conclusion"] = c.Conclusion == c.ExpectedConclusion && c.Conclusion == got && c.BudgetProtectedDuringRun == protected && !protected && c.Conclusion != "observation-incomplete"
	checks["case_deadline"] = c.WallMS >= 0 && c.WallMS < 7000
	switch c.ExpectedConclusion {
	case "named-file-budget-enforced":
		checks["termination"] = fileNamedStop(r) && c.Error != "" && len(c.ErrorKinds) == 1 && c.ErrorKinds[0] == "file_limit" && u.SignalSent && u.TriggerMonotonicNS <= u.SignalAttemptMonotonicNS && u.SignalAttemptMonotonicNS <= u.SignalReturnMonotonicNS && u.SignalAttemptMonotonicNS <= u.WaitMonotonicNS
		checks["not_outer_budget"] = fileOuterQuiet(c) && r.StopReason != "cpu_limit" && r.StopReason != "rss_limit" && r.StopReason != "wall_limit"
	case "entry-ceiling-enforced":
		checks["termination"] = fileEntryStop(r) && c.Error != "" && len(c.ErrorKinds) == 1 && c.ErrorKinds[0] == "file_limit" && u.SignalSent
		checks["entries_are_not_bytes"] = peakAlloc <= want.FileBytes && peakEntries > want.FileEntries
	case "unresolved-unlinked-open":
		checks["unlink_open_gap"] = fileUnlinkResolved(r)
		checks["cleanup_is_not_runtime_protection"] = r.WorkRootClean && !c.BudgetProtectedDuringRun && c.Conclusion == "unresolved-unlinked-open"
	case "axes-distinguished":
		checks["sparse_link_rename"] = fileAxesSplit(r)
		checks["logical_length_is_not_the_budget"] = peakNamed > want.FileBytes && peakAlloc < want.FileBytes && r.StopReason == ""
	case "within-candidate-budget":
		checks["termination"] = fileWithin(r) && c.Error == "" && len(c.ErrorKinds) == 0
	default:
		checks["known_conclusion"] = false
	}
	return checks
}
