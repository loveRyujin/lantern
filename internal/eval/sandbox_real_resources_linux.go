//go:build linux && amd64

package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func (x *linuxRealExecutor) resource(ctx context.Context, item Case) SandboxEvidence {
	e := realEvidence()
	code := realPrelude()
	spec := x.spec(code)
	switch item.ID {
	case "SB-LINUX-14":
		spec.Code = code + "while True: pass\n"
		spec.Limits.Wall = 200 * time.Millisecond
		spec.Limits.CPU = 5 * time.Second
	case "SB-LINUX-15":
		spec.Code = code + "_memory=bytearray(128*1024*1024)\ntime.sleep(5)\n"
		spec.Limits.Memory = 32 << 20
		spec.Limits.Wall = 8 * time.Second
	case "SB-LINUX-16":
		// Bounded even when output defenses are removed during mutation tests.
		spec.Code = code + "sys.stdout.write('x'*(256<<10))\nsys.stdout.flush()\ntime.sleep(5)\n"
		spec.Limits.Wall = 8 * time.Second
	case "SB-LINUX-19":
		spec.Code = code + "raise AssertionError('missing runtime payload entered')\n"
		spec.Runtime.Python = filepath.Join(x.root, "intentionally-missing-python")
		if _, err := os.Lstat(spec.Runtime.Python); !os.IsNotExist(err) {
			return realFailure(e, fmt.Errorf("missing-runtime control is not absent: %v", err))
		}
	case "SB-LINUX-21":
		spec.Code = code + "while True: pass\n"
		spec.Limits.CPU = 100 * time.Millisecond
		spec.Limits.Wall = 5 * time.Second
	}
	result, _, e := x.run(ctx, item.ID, spec, e)
	switch item.ID {
	case "SB-LINUX-14":
		e.observe("wall_budget_enforced", e.ErrorCode == "timeout" && e.Usage.Wall >= spec.Limits.Wall && e.Usage.Wall < spec.Limits.Wall+2*time.Second)
		e.observe("termination_delay_ns", int64(e.Usage.Wall-spec.Limits.Wall))
	case "SB-LINUX-15":
		e.observe("memory_budget_enforced", e.ErrorCode == "oom" && e.Usage.Wall < 2*time.Second && (e.Usage.PeakRSS > spec.Limits.Memory || (e.Usage.WaitPeakRSS > spec.Limits.Memory && e.Usage.WaitPeakRSS > e.Usage.LauncherPeakRSS)))
	case "SB-LINUX-16":
		e.observe("output_bounded", e.ErrorCode == "error" && e.Usage.Wall < 2*time.Second && len(result.Stdout) <= 64<<10 && len(result.Stderr) <= 64<<10 && len(result.Artifacts) == 0)
		e.observe("stdout_bytes", len(result.Stdout))
		e.observe("stderr_bytes", len(result.Stderr))
		host := realHostAttempt(func() error {
			return os.WriteFile(filepath.Join(x.root, "file-budget-control"), make([]byte, 6<<20), 0600)
		})
		fileCode := realPrelude() + `payload=b'a'*(6<<20)
path=os.environ['OUTPUT_DIR']+'/bounded.csv'
written=0
error=0
with open(path,'wb',buffering=0) as handle:
 try:
  while written<len(payload): written+=handle.write(payload[written:])
 except OSError as failure: error=failure.errno
print('LANTERN_FILE_BUDGET:'+json.dumps(dict(errno=error,size=os.stat(path).st_size)),flush=True)
`
		fileResult, _, fileEvidence := x.run(ctx, item.ID+"-file", x.spec(fileCode), realEvidence())
		var packet struct {
			Errno int `json:"errno"`
			Size  int `json:"size"`
		}
		index := strings.LastIndex(fileResult.Stdout, "LANTERN_FILE_BUDGET:")
		parsed := false
		if index >= 0 {
			parsed = json.Unmarshal([]byte(strings.SplitN(fileResult.Stdout[index+len("LANTERN_FILE_BUDGET:"):], "\n", 2)[0]), &packet) == nil
		}
		bounded := host.Success && parsed && packet.Errno == int(unix.EFBIG) && packet.Size == 5<<20 && fileEvidence.ErrorCode == "none" && fileEvidence.PayloadEntered && fileEvidence.ProcessReaped && fileEvidence.TempClean && len(fileResult.Artifacts) == 1 && len(fileResult.Artifacts[0].Data) == 5<<20
		e.observe("file_quota_enforced", bounded)
		e.observe("file_bytes_observed", packet.Size)
		e.observe("file_probe_error_code", fileEvidence.ErrorCode)
		e.observe("file_probe_usage", fileEvidence.Usage)
		e.observe("file_probe_limits", fileEvidence.Limits)
		attempt := &SandboxAttempt{Attempted: parsed, Success: parsed && packet.Errno == 0}
		if parsed {
			attempt.Errno = &packet.Errno
		}
		e.Operations = append(e.Operations, SandboxOperation{Name: "file-write-beyond-5MiB", Host: host, Sandbox: attempt, Attribution: "kernel file size cap plus verified complete bounded artifact"})
	case "SB-LINUX-21":
		e.observe("cpu_budget_enforced", e.ErrorCode == "timeout" && e.Usage.CPU >= spec.Limits.CPU && e.Usage.CPU <= spec.Limits.CPU+250*time.Millisecond && e.Usage.Wall < 4*time.Second)
		e.observe("cpu_overshoot_ns", int64(e.Usage.CPU-spec.Limits.CPU))
	}
	return e
}
