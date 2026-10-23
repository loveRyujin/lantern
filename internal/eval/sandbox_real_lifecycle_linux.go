//go:build linux && amd64

package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/loveRyujin/lantern/core/agent"
	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/pyrt"
	"github.com/loveRyujin/lantern/core/pythonx"
	"github.com/loveRyujin/lantern/core/sandbox"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
	"golang.org/x/sys/unix"
)

const realHelperArg = "--lantern-real-eval-helper-v1"

type realHelperRequest struct {
	Root    string
	Runtime pyrt.Runtime
	Mode    string
	Limits  sandbox.Limits
}

func realHelperCode(mode string) string {
	code := realPrelude() + "open(os.environ['OUTPUT_DIR']+'/ready.json','w').write(json.dumps(dict(python=os.getpid(),guardian=os.getppid())))\n"
	if mode == "parent" {
		return code + "time.sleep(30)\n"
	}
	return code + "_real_finish()\n"
}

func init() {
	if len(os.Args) != 2 || os.Args[1] != realHelperArg {
		return
	}
	var req realHelperRequest
	if json.NewDecoder(io.LimitReader(os.Stdin, 64<<10)).Decode(&req) != nil {
		os.Exit(125)
	}
	if req.Mode != "parent" && req.Mode != "unavailable" {
		os.Exit(125)
	}
	if req.Mode == "unavailable" {
		runtime.LockOSThread()
		if unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != nil {
			os.Exit(125)
		}
		f := []unix.SockFilter{{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0}, {Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_LANDLOCK_CREATE_RULESET, Jf: 1}, {Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)}, {Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW}}
		p := unix.SockFprog{Len: uint16(len(f)), Filter: &f[0]}
		_, _, e := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&p)))
		runtime.KeepAlive(f)
		if e != 0 {
			os.Exit(125)
		}
	}
	code := realHelperCode(req.Mode)
	spec := sandbox.Spec{Code: code, Limits: req.Limits, Runtime: sandbox.Runtime{Python: req.Runtime.Python, PyRoot: req.Runtime.Base.Stdlib, Venv: req.Runtime.Venv}}
	result, err := sandbox.New(sandbox.Config{TempRoot: req.Root}).Run(context.Background(), spec)
	response := struct {
		Result sandbox.Result
		Error  string
	}{Result: result}
	if err != nil {
		response.Error = err.Error()
	}
	_ = json.NewEncoder(os.Stdout).Encode(response)
	os.Exit(0)
}
func realDirClean(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) == 0
}
func realPIDGone(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return os.IsNotExist(err)
}
func realWaitReady(ctx context.Context, root string, done <-chan error) (int, int, error) {
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		paths, _ := filepath.Glob(filepath.Join(root, "*", "out", "ready.json"))
		for _, path := range paths {
			body, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var marker struct {
				Python   int
				Guardian int
			}
			if json.Unmarshal(body, &marker) == nil && marker.Python > 0 && marker.Guardian > 0 {
				return marker.Python, marker.Guardian, nil
			}
		}
		select {
		case err := <-done:
			return 0, 0, fmt.Errorf("task exited before ready: %v", err)
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		case <-tick.C:
		}
	}
}
func (x *linuxRealExecutor) parentDeath(ctx context.Context, item Case) SandboxEvidence {
	e := realEvidence()
	root := filepath.Join(x.root, "parent-death")
	if err := os.MkdirAll(root, 0700); err != nil {
		return realFailure(e, err)
	}
	var old int32
	if err := unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&old)), 0, 0, 0); err != nil {
		return realFailure(e, err)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return realFailure(e, err)
	}
	defer unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, uintptr(old), 0, 0, 0)
	executable, err := os.Executable()
	if err != nil {
		return realFailure(e, err)
	}
	configured := x.spec(realHelperCode("parent")).Limits
	request, _ := json.Marshal(realHelperRequest{Root: root, Runtime: x.runtime, Mode: "parent", Limits: configured})
	e.Limits = configured
	e.observe("limits_source", "explicit helper configuration; no Runner reply after parent death")
	child := exec.CommandContext(ctx, executable, realHelperArg)
	child.Stdin = bytes.NewReader(request)
	child.SysProcAttr = realChildProcessGroup()
	var diagnostic bytes.Buffer
	child.Stderr = &diagnostic
	if err = child.Start(); err != nil {
		return realFailure(e, err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	defer child.Process.Kill()
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	python, guardian, err := realWaitReady(readyCtx, root, done)
	if err != nil {
		return realFailure(e, fmt.Errorf("parent fixture: %v %s", err, diagnostic.String()))
	}
	// Retain kernel identities for mutation-failure cleanup. Numeric PIDs may
	// be reused after successful reaping; never send a late signal by PID alone.
	for _, pid := range []int{python, guardian} {
		fd, openErr := unix.PidfdOpen(pid, 0)
		if openErr != nil {
			return realFailure(e, fmt.Errorf("parent fixture pidfd: %w", openErr))
		}
		defer func(fd, pid int) {
			_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				var status unix.WaitStatus
				_, _ = unix.Wait4(pid, &status, unix.WNOHANG, nil)
				if realPIDGone(pid) {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			_ = unix.Close(fd)
		}(fd, pid)
	}
	e.PayloadEntered = true
	e.PythonPID = python
	e.PayloadSHA256 = realDataHash([]byte(realHelperCode("parent")))
	killedAt := time.Now()
	if err = child.Process.Kill(); err != nil {
		return realFailure(e, err)
	}
	<-done
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, pid := range []int{python, guardian} {
			var status unix.WaitStatus
			_, _ = unix.Wait4(pid, &status, unix.WNOHANG, nil)
		}
		if realPIDGone(python) && realPIDGone(guardian) && realDirClean(root) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.ProcessReaped = realPIDGone(python) && realPIDGone(guardian)
	e.TempClean = realDirClean(root)
	// No Runner reply can be returned by the intentionally killed application.
	// Do not invent a stable Sandbox error code for that missing reply.
	e.ErrorCode = "not-returned"
	e.observe("error_code_source", "parent application terminated before Runner reply")
	if child.ProcessState != nil {
		e.observe("application_exit", child.ProcessState.String())
	}
	e.observe("payload_entered", e.PayloadEntered)
	e.observe("process_reaped", e.ProcessReaped)
	e.observe("temp_clean", e.TempClean)
	e.observe("parent_death_cleaned", e.ProcessReaped && e.TempClean)
	e.observe("guardian_pid", guardian)
	e.observe("parent_cleanup_wall_ns", int64(time.Since(killedAt)))
	e.observe("application_pid", child.Process.Pid)
	return e
}
func (x *linuxRealExecutor) unavailable(ctx context.Context, item Case) SandboxEvidence {
	e := realEvidence()
	root := filepath.Join(x.root, "capability-unavailable")
	if err := os.MkdirAll(root, 0700); err != nil {
		return realFailure(e, err)
	}
	executable, err := os.Executable()
	if err != nil {
		return realFailure(e, err)
	}
	request, _ := json.Marshal(realHelperRequest{Root: root, Runtime: x.runtime, Mode: "unavailable", Limits: x.spec(realHelperCode("unavailable")).Limits})
	child := exec.CommandContext(ctx, executable, realHelperArg)
	child.Stdin = bytes.NewReader(request)
	child.SysProcAttr = realChildProcessGroup()
	var output, diagnostic bytes.Buffer
	child.Stdout = &output
	child.Stderr = &diagnostic
	if err = child.Run(); err != nil {
		return realFailure(e, fmt.Errorf("capability helper: %v %s", err, diagnostic.String()))
	}
	var reply struct {
		Result sandbox.Result
		Error  string
	}
	if err = json.Unmarshal(output.Bytes(), &reply); err != nil {
		return realFailure(e, err)
	}
	e.PayloadSHA256 = realDataHash([]byte(realHelperCode("unavailable")))
	e.PayloadEntered = strings.Contains(reply.Result.Stdout, realEntered)
	e.Usage = reply.Result.Usage
	e.Limits = reply.Result.Limits
	e.PythonPID = e.Usage.PythonPID
	e.ProcessReaped = e.PythonPID == 0 || realPIDGone(e.PythonPID)
	e.TempClean = realDirClean(root)
	e.ErrorCode = string(reply.Result.ErrKind)
	e.observe("payload_entered", e.PayloadEntered)
	e.observe("temp_clean", e.TempClean)
	e.observe("error_code", e.ErrorCode)
	e.observe("capability_failure_closed", e.ErrorCode == "sandbox_unavailable" && !e.PayloadEntered && e.ProcessReaped && e.TempClean)
	e.observe("outer_filter_denies", "LANDLOCK_CREATE_RULESET with EPERM")
	return e
}
func (x *linuxRealExecutor) cancellation(ctx context.Context, item Case) SandboxEvidence {
	e := realEvidence()
	root := filepath.Join(x.root, "cancel")
	if err := os.MkdirAll(root, 0700); err != nil {
		return realFailure(e, err)
	}
	code := realPrelude() + "open(os.environ['OUTPUT_DIR']+'/ready.json','w').write(json.dumps(dict(python=os.getpid(),guardian=os.getppid())))\ntime.sleep(30)"
	active, cancel := context.WithCancel(ctx)
	defer cancel()
	type response struct {
		Result sandbox.Result
		Error  error
	}
	done := make(chan error, 1)
	reply := make(chan response, 1)
	go func() {
		result, err := sandbox.New(sandbox.Config{TempRoot: root}).Run(active, x.spec(code))
		reply <- response{result, err}
		done <- err
	}()
	readyCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	pid, _, err := realWaitReady(readyCtx, root, done)
	if err != nil {
		cancel()
		<-reply
		return realFailure(e, err)
	}
	cancelAt := time.Now()
	cancel()
	got := <-reply
	e.PayloadSHA256 = realDataHash([]byte(code))
	e.PayloadEntered = strings.Contains(got.Result.Stdout, realEntered)
	e.PythonPID = pid
	e.Usage = got.Result.Usage
	e.Limits = got.Result.Limits
	e.ProcessReaped = realPIDGone(pid)
	e.TempClean = realDirClean(root)
	e.ErrorCode = string(got.Result.ErrKind)
	e.observe("payload_entered", e.PayloadEntered)
	e.observe("process_reaped", e.ProcessReaped)
	e.observe("temp_clean", e.TempClean)
	e.observe("error_code", e.ErrorCode)
	e.observe("cancel_observed", got.Error != nil && e.ErrorCode == "timeout" && len(got.Result.Artifacts) == 0)
	e.observe("cancel_to_reply_ns", int64(time.Since(cancelAt)))
	return e
}

type realCountTool struct {
	tools.Tool
	calls *atomic.Int32
}

type realCaptureTool struct {
	tools.Tool
	result *tools.Result
}

func (t realCaptureTool) Execute(ctx context.Context, call tools.Call) (tools.Result, error) {
	result, err := t.Tool.Execute(ctx, call)
	if err == nil {
		*t.result = result
	}
	return result, err
}

func (t realCountTool) Execute(ctx context.Context, c tools.Call) (tools.Result, error) {
	t.calls.Add(1)
	return t.Tool.Execute(ctx, c)
}
func (x *linuxRealExecutor) networkRequest(ctx context.Context, item Case) SandboxEvidence {
	e := realEvidence()
	ws, err := workspace.Create(ctx, filepath.Join(x.root, "schema-workspace"), "schema")
	if err != nil {
		return realFailure(e, err)
	}
	defer ws.Close()
	session, err := pythonx.New(ctx, pythonx.Config{Workspace: ws, RunID: "schema", Runtime: x.runtime})
	if err != nil {
		return realFailure(e, err)
	}
	defer session.Close()
	var calls atomic.Int32
	registry := &tools.Registry{}
	if err = registry.Register(realCountTool{session.PythonTool(), &calls}); err != nil {
		return realFailure(e, err)
	}
	invalid, invalidErr := registry.Prepare(tools.CallInput{ID: "network", Name: "run_python", Arguments: `{"code":"print('must-not-run')","network":true}`})
	_, validErr := registry.Prepare(tools.CallInput{ID: "valid", Name: "run_python", Arguments: `{"code":"print('valid-schema-only')"}`})
	if invalidErr == nil {
		_, _ = invalid.Tool.Execute(ctx, tools.Call{ID: invalid.ID, Name: invalid.Name, WorkspaceID: ws.ID, RunID: "schema", Args: invalid.Args})
	}
	code, coded := tools.CodeOf(invalidErr)
	e.observe("registry_rejected", invalidErr != nil && coded && code == tools.CodeArgumentsInvalid)
	e.observe("valid_schema_accepted", validErr == nil)
	e.observe("tool_execute_count", int(calls.Load()))
	e.ErrorCode = "none"
	if coded {
		e.ErrorCode = code
	} else if invalidErr != nil {
		e.ErrorCode = "uncoded_error"
	}
	e.observe("error_code_source", "actual Registry.Prepare result")
	e.observe("limits_source", "not applicable: Registry rejects before Sandbox.Run")
	return e
}
func (x *linuxRealExecutor) productionPNG(ctx context.Context, item Case) SandboxEvidence {
	e := realEvidence()
	ws, err := workspace.Create(ctx, filepath.Join(x.root, "png-workspace"), "chart")
	if err != nil {
		return realFailure(e, err)
	}
	defer ws.Close()
	session, err := pythonx.New(ctx, pythonx.Config{Workspace: ws, RunID: "chart", Runtime: x.runtime})
	if err != nil {
		return realFailure(e, err)
	}
	defer session.Close()
	badCode := realPrelude() + `import struct,zlib
def _chunk(kind,data):
 return struct.pack('>I',len(data))+kind+data+struct.pack('>I',zlib.crc32(kind+data)&0xffffffff)
bad=b'\x89PNG\r\n\x1a\n'+_chunk(b'IHDR',struct.pack('>IIBBBBB',1,1,8,0,0,0,0))+_chunk(b'IEND',b'')
open(os.environ['OUTPUT_DIR']+'/bad.png','wb').write(bad)
print('INVALID_PNG_PRODUCED',flush=True)
`
	badResult, badErr, badEvidence := x.run(ctx, item.ID+"-invalid-png", x.spec(badCode), realEvidence())
	rejected := badErr != nil && badEvidence.ErrorCode == "error" && strings.Contains(badResult.Stdout, "INVALID_PNG_PRODUCED") && len(badResult.Artifacts) == 0 && badEvidence.ProcessReaped && badEvidence.TempClean
	e.observe("artifact_rejection_observed", rejected)
	e.observe("invalid_png_artifacts", len(badResult.Artifacts))
	e.observe("invalid_png_producer_entered", strings.Contains(badResult.Stdout, "INVALID_PNG_PRODUCED"))
	if !rejected {
		return realFailure(e, fmt.Errorf("actual malformed PNG producer was not safely rejected: error=%v artifacts=%d", badErr, len(badResult.Artifacts)))
	}
	code := `import os,pandas as pd,matplotlib.pyplot as plt,json
print('LANTERN_REAL_ENTERED',flush=True)
columns,rows,meta=load_table()
frame=pd.DataFrame(rows,columns=columns)
assert int(frame['y'].sum())==12
fig,ax=plt.subplots();ax.plot(frame['x'],frame['y']);fig.savefig(os.environ['OUTPUT_DIR']+'/chart.png')
`
	args, _ := json.Marshal(map[string]string{"code": code, "sql_call_id": "points"})
	var source, result tools.Result
	registry := &tools.Registry{}
	if err = registry.Register(realCaptureTool{session.SQLTool(), &source}); err != nil {
		return realFailure(e, err)
	}
	if err = registry.Register(realCaptureTool{session.PythonTool(), &result}); err != nil {
		return realFailure(e, err)
	}
	badArgs, _ := json.Marshal(map[string]string{"code": badCode, "sql_call_id": "points"})
	repairStep := llm.CallTools(llm.ToolCall{ID: "chart", Name: "run_python", Arguments: string(args)})
	repairStep.When = &llm.When{Role: llm.RoleTool, Contains: "error"}
	script, err := llm.NewScript(llm.CallTools(llm.ToolCall{ID: "points", Name: "run_sql", Arguments: `{"sql":"SELECT 1 AS x, 2 AS y UNION ALL SELECT 2,4 UNION ALL SELECT 3,6"}`}), llm.CallTools(llm.ToolCall{ID: "invalid-png", Name: "run_python", Arguments: string(badArgs)}), repairStep, llm.Say("Fixed-script analysis completed"))
	if err != nil {
		return realFailure(e, err)
	}
	loop := agent.Loop{Provider: llm.NewMock(script), Tools: registry, Log: &agent.MemoryLog{}, Config: agent.Config{StepTimeout: 45 * time.Second, TotalTimeout: 90 * time.Second}}
	outcome, err := loop.Run(ctx, agent.Input{WorkspaceID: ws.ID, RunID: "chart", Prompt: "Query controlled points and generate a chart"})
	completed := err == nil && outcome.State == agent.StateCompleted && outcome.ModelCalls == 4 && source.Content != "" && result.Content != ""
	e.observe("agent_loop_completed", completed)
	e.observe("agent_driver", "fixed scripted tool selection; real SQL/Python observations; no cloud LLM")
	if !completed {
		return realFailure(e, fmt.Errorf("production Agent did not complete actual tools: state=%s error=%v", outcome.State, err))
	}
	var observation struct {
		Stdout    string             `json:"stdout"`
		Usage     sandbox.Usage      `json:"usage"`
		Limits    sandbox.Limits     `json:"limits"`
		Artifacts []pythonx.Manifest `json:"artifacts"`
	}
	if err = json.Unmarshal([]byte(result.Content), &observation); err != nil {
		return realFailure(e, err)
	}
	valid := false
	if len(observation.Artifacts) == 1 {
		art, getErr := session.GetArtifact(ws.ID, "chart", observation.Artifacts[0].ID)
		if getErr == nil && art.Kind == sandbox.KindPNG && sandbox.ValidateArtifact(art, 0) == nil {
			_, decodeErr := png.Decode(bytes.NewReader(art.Data))
			valid = decodeErr == nil
			e.observe("png_sha256", realDataHash(art.Data))
			e.observe("png_bytes", len(art.Data))
			if config, configErr := png.DecodeConfig(bytes.NewReader(art.Data)); configErr == nil {
				e.observe("png_width", config.Width)
				e.observe("png_height", config.Height)
			}
		}
	}
	e.PayloadEntered = strings.Contains(observation.Stdout, realEntered)
	e.Usage = observation.Usage
	e.Limits = observation.Limits
	e.PythonPID = e.Usage.PythonPID
	e.ProcessReaped = realPIDGone(e.PythonPID)
	e.TempClean = realDirClean(filepath.Join(ws.Dir, "tmp"))
	e.ErrorCode = "none"
	e.PayloadSHA256 = realDataHash([]byte(code))
	e.observe("payload_entered", e.PayloadEntered)
	e.observe("process_reaped", e.ProcessReaped)
	e.observe("temp_clean", e.TempClean)
	e.observe("error_code", e.ErrorCode)
	e.observe("production_png_valid", valid)
	e.observe("sql_observation_sha256", realDataHash([]byte(source.Content)))
	return e
}
