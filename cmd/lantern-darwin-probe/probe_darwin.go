//go:build darwin

package main

import (
	"context"
	"fmt"
	"image/png"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/loveRyujin/lantern/core/pyrt"
)

const sentinel = "lantern-owned-sentinel\n"

type experiment struct {
	ctx                             context.Context
	out, base, exe, runtimeSentinel string
	rt                              pyrt.Runtime
	plan                            readPlan
	monitor                         *processMonitor
	report                          *Report
	signalPID                       int
	signalFile                      string
	tcpPort, udpPort                int
	unixSocket                      string
	fontSeeds                       map[string][]byte
}

func commandText(ctx context.Context, path string, args ...string) string {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b, e := exec.CommandContext(c, path, args...).CombinedOutput()
	if e != nil {
		return "error: " + e.Error() + " " + string(b)
	}
	return strings.TrimSpace(string(b))
}

func runProbe(ctx context.Context, o options) Report {
	r := newReport()
	r.Environment["source_commit"] = commandText(ctx, "git", "rev-parse", "HEAD")
	r.Environment["source_dirty"] = strconv.FormatBool(commandText(ctx, "git", "status", "--porcelain") != "")
	r.Environment["macos_version"] = commandText(ctx, "/usr/bin/sw_vers", "-productVersion")
	r.Environment["macos_build"] = commandText(ctx, "/usr/bin/sw_vers", "-buildVersion")
	r.Environment["uid"] = strconv.Itoa(os.Getuid())
	r.Environment["gid"] = strconv.Itoa(os.Getgid())
	r.Environment["sandbox_exec"] = "/usr/bin/sandbox-exec"
	r.Fingerprints["payload_sha256"] = digest(pythonPayload)
	rt, e := readRuntime(o.RuntimeConfig)
	if e != nil {
		r.Error = "runtime config: " + e.Error()
		return r
	}
	r.Runtime = rt
	r.Environment["python"] = rt.Base.Version.String()
	release, e := pyrt.AcquirePrepared(ctx, rt)
	if e != nil {
		r.Error = "prepared runtime: " + e.Error()
		return r
	}
	defer release()
	marker, e := os.ReadFile(filepath.Join(rt.Venv, ".lantern-requirements.sha256"))
	if e != nil {
		r.Error = e.Error()
		return r
	}
	r.Fingerprints["runtime_marker_sha256"] = digest(marker)
	if _, e = os.Stat("/usr/bin/sandbox-exec"); e != nil {
		r.Error = e.Error()
		return r
	}
	plan, e := runtimeReadPlan(rt)
	if e != nil {
		r.Error = "runtime closure: " + e.Error()
		return r
	}
	monitor, e := newProcessMonitor()
	if e != nil {
		r.Error = "libproc: " + e.Error()
		return r
	}
	exe, e := os.Executable()
	if e != nil {
		r.Error = e.Error()
		return r
	}
	exe = canonical(exe)
	b, e := os.ReadFile(exe)
	if e != nil {
		r.Error = e.Error()
		return r
	}
	r.Fingerprints["executable_sha256"] = digest(b)
	out, e := filepath.Abs(o.Out)
	if e != nil {
		r.Error = e.Error()
		return r
	}
	if e = os.MkdirAll(filepath.Join(out, "policies"), 0700); e != nil {
		r.Error = e.Error()
		return r
	}
	base, e := os.MkdirTemp("", "lantern-darwin-空格 spike-")
	if e != nil {
		r.Error = e.Error()
		return r
	}
	base = canonical(base)
	defer func() {
		if e := os.RemoveAll(base); e != nil {
			r.Status = "failed"
			r.Error = "fixture cleanup: " + e.Error()
		}
	}()
	f, e := os.CreateTemp(rt.Venv, ".lantern-spike-sentinel-")
	if e != nil {
		r.Error = e.Error()
		return r
	}
	runtimeSentinel := f.Name()
	if _, e = f.WriteString(sentinel); e != nil {
		f.Close()
		r.Error = e.Error()
		return r
	}
	f.Close()
	defer os.Remove(runtimeSentinel)
	x := &experiment{ctx: ctx, out: out, base: base, exe: exe, rt: rt, plan: plan, monitor: monitor, report: &r, runtimeSentinel: runtimeSentinel}
	if e = x.setupControls(); e != nil {
		r.Status = "failed"
		r.Error = "host fixtures: " + e.Error()
		return r
	}
	defer x.closeControls()
	r.Status = "failed" // All actual execution failures after prerequisites are failures.
	fontPreparation := x.prepareFonts()
	pre := stageStart("preflight", []string{"Prepared runtime validated offline; actual policy enters fixed payload and exits successfully; complete environment and bounded readonly closure."}, nil)
	c, e := x.task("vmem", true, exe)
	if e != nil {
		r.Error = e.Error()
		return r
	}
	run := runFixed(ctx, exe, c, monitor)
	pre.Runs = append(pre.Runs, fontPreparation, run)
	pre.check("prepared_runtime", goodRun(fontPreparation) && len(x.fontSeeds) > 0, "AcquirePrepared shared readonly lease held; fixed trusted bundled-only fontcache prepared, no model code")
	pre.check("sandbox_exec", goodRun(run), "actual sandbox-exec must successfully run the fixed payload")
	pre.check("environment", !strings.HasPrefix(r.Environment["source_commit"], "error:") && r.Environment["macos_version"] != "" && os.Getuid() != 0, "real nonroot macOS, arch, Go, Python and source metadata recorded")
	pre.check("payload_identity", run.Entered && run.PayloadPID == run.PID, "payload PID must match supervised exec chain")
	narrow := true
	for _, p := range plan.Directories {
		if p == "/" || p == "/usr" || p == "/System/Library" {
			narrow = false
		}
	}
	pre.check("readonly_plan", narrow, "no whole /usr or /System/Library content grant")
	pre.Details = map[string]interface{}{"runtime_read_plan": plan, "metadata_disclosure": "file-read-metadata and sysctl-read are allowed; host file contents remain denied"}
	r.Fingerprints["policy_sha256"] = digest([]byte(makePolicy(plan, c, exe)))
	r.stage(pre)
	_ = os.RemoveAll(c.Root)
	if pre.Status != "passed" {
		r.Error = "actual sandbox preflight failed; later stages not executed"
		return r
	}
	x.filesystemStage()
	x.networkStage()
	x.processStage()
	x.resourceStage()
	x.lifecycleStage()
	x.sqlStage()
	x.signedStage()
	r.Feasibility = "unresolved"
	r.Limitations = append(r.Limitations, "Guardian unexpected death has no kernel parent-death kill guarantee in this candidate; explicit harness rescue is not mechanism cleanup.", "Resource sampling covers one direct PID including trusted bootstrap, not a proven arbitrary descendant-tree quota.", "App Sandbox entitlement disabled for the baseline ad-hoc carrier; this does not establish that App Sandbox compatibility is impossible.")
	return r
}

func (x *experiment) setupControls() error {
	for _, p := range []string{"host-secret", "host-secret-sibling", "workspace.sqlite"} {
		if e := os.WriteFile(filepath.Join(x.base, p), []byte(sentinel), 0600); e != nil {
			return e
		}
	}
	signalFile := filepath.Join(x.base, "signal-target")
	cmd := exec.CommandContext(x.ctx, x.exe, "__signal-target", signalFile)
	if e := cmd.Start(); e != nil {
		return e
	}
	x.signalPID = cmd.Process.Pid
	x.signalFile = signalFile
	go func() { _ = cmd.Wait() }()
	if !waitFile(x.ctx, signalFile, time.Second) {
		return fmt.Errorf("signal target did not become ready")
	}
	return nil
}
func (x *experiment) closeControls() {
	if x.signalPID > 0 {
		_ = syscall.Kill(x.signalPID, syscall.SIGKILL)
	}
}

func (x *experiment) task(mode string, sandboxed bool, exe string) (taskConfig, error) {
	root, e := os.MkdirTemp(x.base, "task-"+mode+"-中文 space-")
	if e != nil {
		return taskConfig{}, e
	}
	c := taskConfig{Mode: mode, Python: x.rt.Python, Root: root, Input: filepath.Join(root, "input", "table.csv"), Outside: filepath.Join(x.base, "host-secret"), Database: filepath.Join(x.base, "workspace.sqlite"), RuntimeSentinel: x.runtimeSentinel, Out: filepath.Join(root, "out"), Tmp: filepath.Join(root, "tmp"), Sandboxed: sandboxed, Budget: defaultBudget(), TCPPort: x.tcpPort, UDPPort: x.udpPort, UnixSocket: x.unixSocket, SignalPID: x.signalPID}
	for _, d := range []string{filepath.Dir(c.Input), c.Out, c.Tmp, filepath.Join(c.Tmp, "mpl")} {
		if e = os.MkdirAll(d, 0700); e != nil {
			return c, e
		}
	}
	c.Script = filepath.Join(root, "input", "payload.py")
	c.Policy = filepath.Join(x.out, "policies", filepath.Base(root)+".sb")
	for p, b := range map[string][]byte{c.Input: []byte(sentinel), c.Script: pythonPayload} {
		if e = os.WriteFile(p, b, 0600); e != nil {
			return c, e
		}
	}
	if e = os.Symlink(c.Outside, filepath.Join(root, "input", "escape-link")); e != nil {
		return c, e
	}
	if home, e := os.UserHomeDir(); e == nil {
		c.SensitiveTargets = []string{filepath.Join(home, ".ssh", "id_ed25519"), filepath.Join(home, ".aws", "credentials"), "/etc/master.passwd"}
	}
	for name, b := range x.fontSeeds {
		if e = os.WriteFile(filepath.Join(c.Tmp, "mpl", name), b, 0600); e != nil {
			return c, e
		}
	}
	return c, os.WriteFile(c.Policy, []byte(makePolicy(x.plan, c, exe)), 0600)
}

func goodRun(r Execution) bool {
	return r.Entered && r.ExitCode == 0 && r.StopReason == "" && r.Error == "" && r.Usage.Waited
}
func findOp(r Execution, name string) (Observation, bool) {
	for _, o := range r.Observations {
		if o.Operation == name {
			return o, true
		}
	}
	return Observation{}, false
}
func opSuccess(r Execution, name string) bool {
	o, ok := findOp(r, name)
	return ok && o.Attempted && o.Success
}
func opDenied(r Execution, name string) bool {
	o, ok := findOp(r, name)
	return ok && o.Attempted && !o.Success && (o.Errno == int(syscall.EACCES) || o.Errno == int(syscall.EPERM))
}
func allWaited(runs []Execution) bool {
	for _, r := range runs {
		if !r.Usage.Waited {
			return false
		}
	}
	return len(runs) > 0
}
func (x *experiment) cleanTasks(roots ...string) bool {
	ok := true
	for _, root := range roots {
		if e := os.RemoveAll(root); e != nil {
			ok = false
		}
		if _, e := os.Lstat(root); !os.IsNotExist(e) {
			ok = false
		}
	}
	return ok
}
func waitFile(ctx context.Context, path string, d time.Duration) bool {
	until := time.NewTimer(d)
	defer until.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if st, e := os.Stat(path); e == nil && st.Size() > 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-until.C:
			return false
		case <-tick.C:
		}
	}
}

func (x *experiment) filesystemStage() {
	s := stageStart("filesystem", []string{"Same-UID owned host controls must succeed; isolated input/runtime reads and output/tmp writes succeed; host/db/input/runtime writes, symlink escape and sibling reads are denied; actual sensitive paths open-only."}, nil)
	h, e := x.task("filesystem", false, x.exe)
	if e != nil {
		s.check("host_controls", false, e.Error())
		x.report.stage(s)
		return
	}
	host := runFixed(x.ctx, x.exe, h, x.monitor)
	s.Runs = append(s.Runs, host)
	controls := goodRun(host)
	for _, name := range []string{"input_read", "host_read", "database_read", "runtime_read", "input_write", "host_write", "runtime_write", "out_write", "tmp_write", "symlink_read", "sibling_read"} {
		controls = controls && opSuccess(host, name)
	}
	for _, p := range []string{h.Input, h.Outside, h.RuntimeSentinel} {
		_ = os.WriteFile(p, []byte(sentinel), 0600)
	}
	c, e := x.task("filesystem", true, x.exe)
	if e != nil {
		s.check("host_controls", false, e.Error())
		x.report.stage(s)
		return
	}
	run := runFixed(x.ctx, x.exe, c, x.monitor)
	s.Runs = append(s.Runs, run)
	s.check("host_controls", controls, "all owned same-UID read/write controls executed successfully before isolation; fixtures restored")
	for _, name := range []string{"input_read", "runtime_read", "out_write", "tmp_write"} {
		s.check(name, opSuccess(run, name), "actual isolated positive operation")
	}
	for _, name := range []string{"host_read", "database_read", "input_write", "host_write", "runtime_write", "symlink_read", "sibling_read"} {
		s.check(name+"_denied", opDenied(run, name), "requires actual EPERM/EACCES; ENOENT is not confinement evidence")
	}
	actual := true
	for i := range c.SensitiveTargets {
		o, ok := findOp(run, fmt.Sprintf("actual_target_%d", i))
		actual = actual && ok && o.Attempted && !o.Success
	}
	s.check("actual_targets", actual, "open/close only; host ambient denials remain observations, not independent policy proof")
	s.check("payload_completed", goodRun(run), "valid marker plus successful process exit")
	s.check("waited", allWaited(s.Runs), "both direct children Waited")
	s.check("temp_clean", x.cleanTasks(h.Root, c.Root), "task roots removed after Wait")
	x.report.stage(s)
}

func echoStream(l net.Listener) {
	for {
		c, e := l.Accept()
		if e != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(time.Second))
			b := make([]byte, 64)
			n, e := c.Read(b)
			if e == nil && strings.HasPrefix(string(b[:n]), "lantern-") {
				_, _ = c.Write([]byte("lantern-ack"))
			}
		}(c)
	}
}
func (x *experiment) networkFixtures() (func(), error) {
	tcp, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return nil, e
	}
	udp, e := net.ListenPacket("udp4", "127.0.0.1:0")
	if e != nil {
		tcp.Close()
		return nil, e
	}
	// Unix paths are bounded below Darwin's sockaddr_un size; task paths include
	// Unicode separately. This path gets a file grant so network deny is exercised.
	dir, e := os.MkdirTemp("", "lnsock-")
	if e != nil {
		tcp.Close()
		udp.Close()
		return nil, e
	}
	path := filepath.Join(canonical(dir), "s")
	unix, e := net.Listen("unix", path)
	if e != nil {
		tcp.Close()
		udp.Close()
		os.RemoveAll(dir)
		return nil, e
	}
	x.tcpPort = tcp.Addr().(*net.TCPAddr).Port
	x.udpPort = udp.LocalAddr().(*net.UDPAddr).Port
	x.unixSocket = path
	go echoStream(tcp)
	go echoStream(unix)
	go func() {
		b := make([]byte, 64)
		for {
			n, a, e := udp.ReadFrom(b)
			if e != nil {
				return
			}
			if string(b[:n]) == "lantern-udp" {
				_, _ = udp.WriteTo([]byte("lantern-ack"), a)
			}
		}
	}()
	return func() { tcp.Close(); udp.Close(); unix.Close(); os.RemoveAll(dir); x.unixSocket = "" }, nil
}
func (x *experiment) networkStage() {
	s := stageStart("network", []string{"Host TCP/UDP/Unix controlled roundtrips succeed; identical isolated operations are denied, without public internet dependency."}, nil)
	close, e := x.networkFixtures()
	if e != nil {
		s.check("host_controls", false, e.Error())
		x.report.stage(s)
		return
	}
	defer close()
	h, e := x.task("network", false, x.exe)
	if e != nil {
		s.check("host_controls", false, e.Error())
		x.report.stage(s)
		return
	}
	host := runFixed(x.ctx, x.exe, h, x.monitor)
	c, e := x.task("network", true, x.exe)
	if e != nil {
		s.check("host_controls", false, e.Error())
		x.report.stage(s)
		return
	}
	run := runFixed(x.ctx, x.exe, c, x.monitor)
	s.Runs = []Execution{host, run}
	s.check("host_controls", goodRun(host) && opSuccess(host, "tcp_connect") && opSuccess(host, "udp_exchange") && opSuccess(host, "unix_connect"), "all three same-payload host roundtrips")
	for label, name := range map[string]string{"tcp": "tcp_connect", "udp": "udp_exchange", "unix": "unix_connect"} {
		s.check(label+"_denied", opDenied(run, name), "must observe EPERM/EACCES on the real operation")
	}
	s.check("payload_completed", goodRun(run), "fixed network payload entered and completed")
	s.check("waited", allWaited(s.Runs), "host and sandbox children Waited")
	s.check("temp_clean", x.cleanTasks(h.Root, c.Root), "task roots removed")
	x.report.stage(s)
}

func (x *experiment) processStage() {
	s := stageStart("process-profile", []string{"Bounded process/control experiments actually execute; same-UID host controls work; direct children Wait and task roots clean."}, []string{"fork", "posix_spawn", "thread", "setsid", "setpgid", "cross_signal"})
	h, e := x.task("process-profile", false, x.exe)
	if e != nil {
		s.check("host_controls", false, e.Error())
		x.report.stage(s)
		return
	}
	host := runFixed(x.ctx, x.exe, h, x.monitor)
	time.Sleep(30 * time.Millisecond)
	before, _ := os.ReadFile(x.signalFile)
	c, e := x.task("process-profile", true, x.exe)
	if e != nil {
		s.check("host_controls", false, e.Error())
		x.report.stage(s)
		return
	}
	run := runFixed(x.ctx, x.exe, c, x.monitor)
	s.Runs = []Execution{host, run}
	time.Sleep(30 * time.Millisecond)
	after, _ := os.ReadFile(x.signalFile)
	controls := goodRun(host)
	attempted := true
	for _, name := range []string{"fork", "posix_spawn", "thread", "setsid", "setpgid", "cross_signal"} {
		controls = controls && opSuccess(host, name)
		o, ok := findOp(run, name)
		attempted = attempted && ok && o.Attempted
	}
	s.check("host_controls", controls && strings.Contains(string(before), "signal"), "host fork/spawn/thread/session/group and actual SIGUSR1 against owned signal-target")
	s.check("all_attempted", attempted, "capabilities are observations, not a claim that Linux's strict no-thread profile exists")
	s.check("payload_completed", goodRun(run), "fixed process experiment completed")
	s.check("waited", allWaited(s.Runs), "direct and bounded fork/spawn children waited")
	s.check("temp_clean", x.cleanTasks(h.Root, c.Root), "task roots removed")
	s.Details = map[string]interface{}{"signal_target_changed_during_sandbox": string(before) != string(after), "profile": "single direct PID monitoring; threads/session/group capabilities characterized, no arbitrary process-tree quota claimed"}
	s.Limitations = []string{"Any successful process/thread/session/group capability must be considered before a production profile is selected."}
	x.report.stage(s)
}

func (x *experiment) resourceStage() {
	s := stageStart("resources", []string{"Fixed bounded workloads exceed their declared budget; sampled CPU/RSS/disk and wall/output monitor stop and Wait them; large untouched virtual mapping remains a positive control."}, nil)
	roots := []string{}
	allSampled := true
	for _, tc := range []struct{ mode, check, reason string }{{"cpu", "cpu_limit", "cpu_limit"}, {"rss", "rss_limit", "rss_limit"}, {"vmem", "vmem_positive", ""}, {"wall", "wall_limit", "wall_limit"}, {"output", "stdout_limit", "stdout_limit"}, {"stderr", "stderr_limit", "stderr_limit"}, {"disk", "disk_limit", "disk_limit"}} {
		c, e := x.task(tc.mode, true, x.exe)
		if e != nil {
			s.check(tc.check, false, e.Error())
			continue
		}
		roots = append(roots, c.Root)
		switch tc.mode {
		case "cpu":
			c.Budget.CPUSeconds = 0.15
			c.Budget.CPURLimit = 1
		case "rss", "vmem":
			c.Budget.RSSBytes = 64 << 20
		case "wall":
			c.Budget.WallMS = 200
		case "disk":
			c.Budget.DiskBytes = 4 << 20
		}
		run := runFixed(x.ctx, x.exe, c, x.monitor)
		s.Runs = append(s.Runs, run)
		pass := run.Entered && run.Usage.Waited && run.StopReason == tc.reason
		if tc.mode == "vmem" {
			pass = goodRun(run) && run.Usage.PeakRSS < c.Budget.RSSBytes && run.Usage.PeakVirtual >= 256<<20
		} else {
			pass = pass && run.ExitCode != 0 && run.Usage.KillLatencyMS < 2000
		}
		if tc.mode == "output" {
			pass = pass && len(run.Stdout) <= c.Budget.OutputBytes
		}
		if tc.mode == "stderr" {
			pass = pass && len(run.Stderr) <= c.Budget.OutputBytes
		}
		s.check(tc.check, pass, fmt.Sprintf("reason=%s samples=%d kill_to_wait=%.2fms", run.StopReason, run.Usage.Samples, run.Usage.KillLatencyMS))
		allSampled = allSampled && run.Usage.Samples > 0 && run.Usage.MaxSampleGapMS > 0
	}
	s.check("sampling", allSampled, "actual 10ms sampling target and measured maximum gap recorded; not hard instantaneous RSS/disk quota")
	s.check("waited", allWaited(s.Runs), "every started direct child Waited")
	s.check("temp_clean", x.cleanTasks(roots...), "task roots removed after Wait")
	s.Limitations = []string{"CPU/RSS include trusted launcher and sandbox-exec; no Python-only accounting claim.", "RLIMIT_CPU/FSIZE/NOFILE are per-process inherited backstops; NPROC is UID-scoped and is not installed as a per-task limit.", "Disk sampling counts named files under out/tmp; unlinked-open and arbitrary descendant resources remain unresolved production requirements."}
	x.report.stage(s)
}

func (x *experiment) sqlStage() {
	s := stageStart("sql-worker", []string{"A real fixed modernc SQLite query returns the expected sum under the candidate monitor; child Waits and files clean; production SQL remains unavailable."}, []string{"Whether bootstrap-inclusive monitoring is suitable for a future dedicated SQL worker"})
	c, e := x.task("sql", true, x.exe)
	if e != nil {
		s.check("query_result", false, e.Error())
		x.report.stage(s)
		return
	}
	run := runFixed(x.ctx, x.exe, c, x.monitor)
	s.Runs = []Execution{run}
	s.check("query_result", goodRun(run) && opSuccess(run, "sqlite_sum"), "WITH RECURSIVE sum(1..100000) == 5000050000, real SQLite")
	s.check("sampled", run.Usage.Samples > 0 && run.Usage.PeakRSS > 0, "Go worker RSS/virtual/CPU include bootstrap; address space is not RSS")
	s.check("waited", run.Usage.Waited, "Wait completed")
	s.check("temp_clean", x.cleanTasks(c.Root), "database/task directory removed")
	s.Limitations = []string{"Fixed trusted SQL only; no production run_sql route is enabled."}
	x.report.stage(s)
}

func pngValid(path string) bool {
	f, e := os.Open(path)
	if e != nil {
		return false
	}
	defer f.Close()
	image, e := png.Decode(io.LimitReader(f, 5<<20))
	return e == nil && image.Bounds().Dx() > 0 && image.Bounds().Dy() > 0 && image.Bounds().Dx()*image.Bounds().Dy() <= 1000000
}
