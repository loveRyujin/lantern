//go:build linux && amd64

package pythonx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/loveRyujin/lantern/core/pyrt"
	"github.com/loveRyujin/lantern/core/pythonx"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
	"golang.org/x/sys/unix"
)

func pythonQAWorkspace(t *testing.T) *workspace.Handle {
	t.Helper()
	ws, err := workspace.Create(t.Context(), t.TempDir(), "python-qa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

func pythonQASession(t *testing.T, ws *workspace.Handle, run string, rt pyrt.Runtime) *pythonx.Session {
	t.Helper()
	s, err := pythonx.New(t.Context(), pythonx.Config{Workspace: ws, RunID: run, Runtime: rt})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func pythonQACall(ws *workspace.Handle, run, id, name string, args map[string]any) tools.Call {
	b, _ := json.Marshal(args)
	return tools.Call{ID: id, Name: name, Args: b, WorkspaceID: ws.ID, RunID: run}
}

func pythonQAError(t *testing.T, result tools.Result, err error, want string) {
	t.Helper()
	code, ok := tools.CodeOf(err)
	if !ok || code != want || result.Content != "" {
		t.Fatalf("want empty result/code %s: result=%+v error=%v code=%s", want, result, err, code)
	}
}

func pythonQAClean(t *testing.T, ws *workspace.Handle) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(ws.Dir, "tmp"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("workspace intermediates remain: %v %v", entries, err)
	}
}

func TestPythonQASchemaScopeAndUnavailable(t *testing.T) {
	ws := pythonQAWorkspace(t)
	s := pythonQASession(t, ws, "run-a", pyrt.Runtime{})
	reg := &tools.Registry{}
	if err := reg.Register(s.PythonTool()); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"code":"pass","network":true}`, `{"code":"pass","extra_read_only":["/"]}`,
		`{"code":"pass","inputs":{"secret":"x"}}`, `{"code":"pass","runtime":{"python":"/usr/bin/python3"}}`,
		`{"code":"pass","limits":{"memory":999999999}}`, `{"code":"pass","sql_call_id":null}`,
		`{"code":"pass","sql_call_id":""}`, `{"code":null}`, `{"code":17}`,
	} {
		if _, err := reg.PrepareRaw("run_python", []byte(raw)); !tools.ArgumentFailure(err) {
			t.Fatalf("permission/shape passed Schema: %s error=%v", raw, err)
		}
	}
	if _, err := reg.PrepareRaw("run_python", []byte(`{"code":"print(1)","sql_call_id":"source"}`)); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []tools.Tool{s.SQLTool(), s.PythonTool()} {
		call := pythonQACall(ws, "other-run", "x", tool.Spec().Name, map[string]any{"code": "pass", "sql": "SELECT 1"})
		// Give each production tool valid arguments so the scope assertion cannot
		// accidentally be satisfied by malformed input rejection.
		if tool.Spec().Name == "run_python" {
			call.Args = json.RawMessage(`{"code":"pass"}`)
		} else {
			call.Args = json.RawMessage(`{"sql":"SELECT 1"}`)
		}
		result, err := tool.Execute(t.Context(), call)
		pythonQAError(t, result, err, pythonx.CodeScope)
		call.RunID = "run-a"
		call.WorkspaceID = "other-workspace"
		result, err = tool.Execute(t.Context(), call)
		pythonQAError(t, result, err, pythonx.CodeScope)
	}
	marker := filepath.Join(t.TempDir(), "untrusted-must-not-run")
	call := pythonQACall(ws, "run-a", "p", "run_python", map[string]any{"code": fmt.Sprintf("open(%q,'w').write('escaped')", marker)})
	result, err := s.PythonTool().Execute(t.Context(), call)
	pythonQAError(t, result, err, "runtime_unavailable")
	if strings.Contains(err.Error(), ws.Dir) || strings.Contains(err.Error(), marker) {
		t.Fatalf("runtime feedback leaked host path: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unavailable runtime executed code: %v", err)
	}
	pythonQAClean(t, ws)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	result, err = s.PythonTool().Execute(t.Context(), call)
	pythonQAError(t, result, err, pythonx.CodeClosed)
}

func TestPythonQASQLCacheOwnershipAndBudget(t *testing.T) {
	ws := pythonQAWorkspace(t)
	s := pythonQASession(t, ws, "run-cache", pyrt.Runtime{})
	failed := pythonQACall(ws, "run-cache", "failed", "run_sql", map[string]any{"sql": "DELETE FROM users"})
	result, err := s.SQLTool().Execute(t.Context(), failed)
	pythonQAError(t, result, err, "sql_rejected")
	result, err = s.PythonTool().Execute(t.Context(), pythonQACall(ws, "run-cache", "read-failed", "run_python", map[string]any{"code": "pass", "sql_call_id": "failed"}))
	pythonQAError(t, result, err, pythonx.CodeInput)
	for n := 0; n < pythonx.MaxSQLCalls; n++ {
		call := pythonQACall(ws, "run-cache", fmt.Sprint(n), "run_sql", map[string]any{"sql": "SELECT 9007199254740993 AS n"})
		result, err := s.SQLTool().Execute(t.Context(), call)
		if err != nil || !strings.Contains(result.Content, "9007199254740993") {
			t.Fatalf("cache entry %d: %+v %v", n, result, err)
		}
	}
	call := pythonQACall(ws, "run-cache", "0", "run_sql", map[string]any{"sql": "SELECT 0 AS n"})
	result, err = s.SQLTool().Execute(t.Context(), call)
	pythonQAError(t, result, err, pythonx.CodeInput)
	call.ID = "too-many"
	result, err = s.SQLTool().Execute(t.Context(), call)
	pythonQAError(t, result, err, pythonx.CodeBudget)
	// A valid cached source proceeds to runtime validation; another run's source
	// cannot even reach that step, despite using the same workspace and call ID.
	read := pythonQACall(ws, "run-cache", "p", "run_python", map[string]any{"code": "columns,rows,meta=load_table()", "sql_call_id": "0"})
	result, err = s.PythonTool().Execute(t.Context(), read)
	pythonQAError(t, result, err, "runtime_unavailable")
	other := pythonQASession(t, ws, "run-other", pyrt.Runtime{})
	read.RunID = "run-other"
	result, err = other.PythonTool().Execute(t.Context(), read)
	pythonQAError(t, result, err, pythonx.CodeInput)
	pythonQAClean(t, ws)
}

func TestPythonQASQLByteBudgetAndConcurrentIDs(t *testing.T) {
	ws := pythonQAWorkspace(t)
	s := pythonQASession(t, ws, "bytes", pyrt.Runtime{})
	accepted := 0
	for n := 0; n < pythonx.MaxSQLCalls; n++ {
		result, err := s.SQLTool().Execute(t.Context(), pythonQACall(ws, "bytes", fmt.Sprint(n), "run_sql", map[string]any{"sql": "SELECT hex(zeroblob(450000)) AS payload"}))
		if err != nil {
			pythonQAError(t, result, err, pythonx.CodeBudget)
			break
		}
		accepted++
	}
	if accepted < 2 || accepted == pythonx.MaxSQLCalls {
		t.Fatalf("large SQL observations did not hit byte budget before count budget: accepted=%d", accepted)
	}
	result, err := s.PythonTool().Execute(t.Context(), pythonQACall(ws, "bytes", "existing", "run_python", map[string]any{"code": "pass", "sql_call_id": "0"}))
	pythonQAError(t, result, err, "runtime_unavailable")
	// Same ID is reserved atomically before the actual SQL worker starts. The
	// loser must not overwrite or publish a second observation.
	race := pythonQASession(t, ws, "race", pyrt.Runtime{})
	type outcome struct {
		result tools.Result
		err    error
	}
	done := make(chan outcome, 2)
	for n := 0; n < 2; n++ {
		go func() {
			result, err := race.SQLTool().Execute(t.Context(), pythonQACall(ws, "race", "same-id", "run_sql", map[string]any{"sql": "SELECT 777 AS marker"}))
			done <- outcome{result, err}
		}()
	}
	good := 0
	for n := 0; n < 2; n++ {
		got := <-done
		if got.err == nil {
			good++
			if !strings.Contains(got.result.Content, "777") {
				t.Fatal("winning worker observation was substituted")
			}
		} else {
			pythonQAError(t, got.result, got.err, pythonx.CodeInput)
		}
	}
	if good != 1 {
		t.Fatalf("expected exactly one production SQL worker success, got %d", good)
	}
}

type pythonQAObservation struct {
	Stdout          string             `json:"stdout"`
	Stderr          string             `json:"stderr"`
	StdoutTruncated bool               `json:"stdout_truncated"`
	StderrTruncated bool               `json:"stderr_truncated"`
	Artifacts       []pythonx.Manifest `json:"artifacts"`
}

func pythonQAObserve(t *testing.T, result tools.Result, err error) pythonQAObservation {
	t.Helper()
	if err != nil {
		t.Fatalf("production Python execution: %v", err)
	}
	var value pythonQAObservation
	if err := json.Unmarshal([]byte(result.Content), &value); err != nil {
		t.Fatalf("invalid model observation: %s %v", result.Content, err)
	}
	if len(result.Content) > 32<<10 || strings.Contains(result.Content, "base64") || strings.Contains(result.Content, `"Data"`) || strings.Contains(result.Content, `"data"`) {
		t.Fatalf("model received artifact bytes: %s", result.Content)
	}
	return value
}

func TestPythonQARealSessionLifecycle(t *testing.T) {
	if os.Getenv("SANDBOX_REAL_PACKAGES") != "1" {
		t.Skip("prepared production runtime requires SANDBOX_REAL_PACKAGES=1; default tests never install")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	rt, err := (pyrt.Config{Root: t.TempDir()}).Ensure(ctx)
	if err != nil {
		t.Fatalf("required prepared runtime: %v", err)
	}
	ws := pythonQAWorkspace(t)
	s := pythonQASession(t, ws, "run-real", rt)
	_, err = s.SQLTool().Execute(t.Context(), pythonQACall(ws, "run-real", "source", "run_sql", map[string]any{"sql": "SELECT 9223372036854775807 AS dup, 9007199254740993 AS dup"}))
	if err != nil {
		t.Fatal(err)
	}
	code := `import os,json,socket
columns,rows,meta=load_table()
assert columns==['dup','dup'] and rows[0]==[9223372036854775807,9007199254740993]
assert os.environ['MPLBACKEND']=='Agg'
assert os.path.commonpath([os.environ['MPLCONFIGDIR'],os.getcwd()])==os.getcwd()
try: socket.socket()
except PermissionError: pass
else: raise AssertionError('network socket permitted')
open(os.environ['OUTPUT_DIR']+'/exact.json','w').write(json.dumps({'big':rows[0][0],'exact':rows[0][1]}))
print('production-success')
print(os.environ['INPUT_DIR'])
import sys
print(sys.executable,sys.prefix,sys.base_prefix)
`
	result, err := s.PythonTool().Execute(t.Context(), pythonQACall(ws, "run-real", "p", "run_python", map[string]any{"code": code, "sql_call_id": "source"}))
	observation := pythonQAObserve(t, result, err)
	if len(observation.Artifacts) != 1 || observation.Artifacts[0].Name != "exact.json" || !strings.Contains(observation.Stdout, "production-success") || strings.Contains(result.Content, ws.Dir) || strings.Contains(result.Content, rt.Venv) || (len(rt.Base.Prefix) > 1 && strings.Contains(result.Content, rt.Base.Prefix)) {
		t.Fatalf("manifest/path contract: %+v %s", observation, result.Content)
	}
	id := observation.Artifacts[0].ID
	if len(id) < 32 || strings.ContainsAny(id, "/\\.") {
		t.Fatalf("artifact ID is not opaque: %q", id)
	}
	artifact, err := s.GetArtifact(ws.ID, "run-real", id)
	if err != nil || !bytes.Contains(artifact.Data, []byte("9223372036854775807")) || len(artifact.Data) != observation.Artifacts[0].Bytes {
		t.Fatalf("retained artifact=%+v error=%v", artifact, err)
	}
	artifact.Data[0] = 'x'
	copy, err := s.GetArtifact(ws.ID, "run-real", id)
	if err != nil || !json.Valid(copy.Data) {
		t.Fatalf("GetArtifact aliases retained bytes: %v", err)
	}
	for _, scope := range [][2]string{{"another-workspace", "run-real"}, {ws.ID, "another-run"}} {
		if _, err := s.GetArtifact(scope[0], scope[1], id); err == nil {
			t.Fatalf("cross-scope artifact read succeeded: %v", scope)
		}
	}
	if _, err := s.GetArtifact(ws.ID, "run-real", "missing"); err == nil {
		t.Fatal("unknown artifact ID accepted")
	}
	pythonQAClean(t, ws)
	t.Run("caller cancellation preserves session", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		type response struct {
			result tools.Result
			err    error
		}
		done := make(chan response, 1)
		go func() {
			result, err := s.PythonTool().Execute(ctx, pythonQACall(ws, "run-real", "context-cancel", "run_python", map[string]any{"code": "import os,time\nopen(os.environ['OUTPUT_DIR']+'/context-ready.json','w').write('{}')\ntime.sleep(20)"}))
			done <- response{result, err}
		}()
		deadline := time.Now().Add(8 * time.Second)
		for {
			paths, _ := filepath.Glob(filepath.Join(ws.Dir, "tmp", "*", "out", "context-ready.json"))
			if len(paths) > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("Python did not start before cancellation")
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
		select {
		case got := <-done:
			pythonQAError(t, got.result, got.err, "timeout")
		case <-time.After(5 * time.Second):
			t.Fatal("caller cancellation did not reap Python")
		}
		pythonQAClean(t, ws)
		result, err := s.PythonTool().Execute(t.Context(), pythonQACall(ws, "run-real", "after-cancel", "run_python", map[string]any{"code": "columns,rows,meta=load_table()\nprint(rows[0][0])", "sql_call_id": "source"}))
		if pythonQAObserve(t, result, err).Stdout != "9223372036854775807\n" {
			t.Fatal("cancellation damaged cached SQL or session recovery")
		}
	})
	// Runtime/SQL/parameter errors must not corrupt earlier sources or artifacts.
	failed := pythonQACall(ws, "run-real", "failed-source", "run_sql", map[string]any{"sql": "DELETE FROM users"})
	result, err = s.SQLTool().Execute(t.Context(), failed)
	pythonQAError(t, result, err, "sql_rejected")
	result, err = s.PythonTool().Execute(t.Context(), pythonQACall(ws, "run-real", "missing-input", "run_python", map[string]any{"code": "pass", "sql_call_id": "failed-source"}))
	pythonQAError(t, result, err, pythonx.CodeInput)
	result, err = s.PythonTool().Execute(t.Context(), pythonQACall(ws, "run-real", "raise", "run_python", map[string]any{"code": "raise ValueError('controlled')"}))
	pythonQAError(t, result, err, "error")
	result, err = s.PythonTool().Execute(t.Context(), pythonQACall(ws, "run-real", "recover", "run_python", map[string]any{"code": "print('recovered')"}))
	if pythonQAObserve(t, result, err).Stdout != "recovered\n" {
		t.Fatal("Python did not recover")
	}
	pythonQAClean(t, ws)
	t.Run("bounded model streams", func(t *testing.T) {
		result, err := s.PythonTool().Execute(t.Context(), pythonQACall(ws, "run-real", "large-stream", "run_python", map[string]any{"code": "import sys\nprint('测'*5000)\nprint('错'*5000,file=sys.stderr)"}))
		value := pythonQAObserve(t, result, err)
		if len(value.Stdout) > 8192 || len(value.Stderr) > 8192 || !utf8.ValidString(value.Stdout) || !utf8.ValidString(value.Stderr) || !value.StdoutTruncated || !value.StderrTruncated {
			t.Fatalf("unbounded/lossy streams: stdout=%d stderr=%d flags=%v/%v", len(value.Stdout), len(value.Stderr), value.StdoutTruncated, value.StderrTruncated)
		}
	})
	t.Run("runtime lease and close cancel", func(t *testing.T) {
		active := pythonQASession(t, ws, "run-close", rt)
		type response struct {
			result tools.Result
			err    error
		}
		done := make(chan response, 1)
		go func() {
			result, err := active.PythonTool().Execute(t.Context(), pythonQACall(ws, "run-close", "sleep", "run_python", map[string]any{"code": "import os,time\nopen(os.environ['OUTPUT_DIR']+'/ready.json','w').write('{}')\ntime.sleep(20)"}))
			done <- response{result, err}
		}()
		deadline := time.Now().Add(8 * time.Second)
		for {
			paths, _ := filepath.Glob(filepath.Join(ws.Dir, "tmp", "*", "out", "ready.json"))
			if len(paths) > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("real Python did not start")
			}
			time.Sleep(time.Millisecond)
		}
		lock, err := os.OpenFile(filepath.Join(filepath.Dir(filepath.Dir(rt.Venv)), ".pyrt.lock"), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
			t.Fatal("runtime shared lease was released before Run finished")
		}
		if err := active.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-done:
			pythonQAError(t, got.result, got.err, "timeout")
		case <-time.After(5 * time.Second):
			t.Fatal("Close did not wait for active Python cancellation")
		}
		if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatalf("runtime lease not released after Close: %v", err)
		}
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		pythonQAClean(t, ws)
	})
	if _, err := s.GetArtifact(ws.ID, "run-real", id); err != nil {
		t.Fatalf("earlier artifact was evicted: %v", err)
	}
	t.Run("prepared package origin", func(t *testing.T) {
		packageDir := filepath.Join(rt.Venv, "lib", fmt.Sprintf("python%d.%d", rt.Base.Version.Major, rt.Base.Version.Minor), "site-packages", "pandas")
		outside := filepath.Join(t.TempDir(), "pandas")
		if err := os.Rename(packageDir, outside); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = os.Remove(packageDir)
			if err := os.Rename(outside, packageDir); err != nil {
				t.Errorf("restore package fixture: %v", err)
			}
		}()
		if err := os.Symlink(outside, packageDir); err != nil {
			t.Fatal(err)
		}
		result, err := s.PythonTool().Execute(t.Context(), pythonQACall(ws, "run-real", "outside-package", "run_python", map[string]any{"code": "print('must-not-use-outside-packages')"}))
		pythonQAError(t, result, err, "runtime_unavailable")
		pythonQAClean(t, ws)
	})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetArtifact(ws.ID, "run-real", id); err == nil {
		t.Fatal("artifact accessible after Close")
	}
}

func TestPythonQARealArtifactStoreBudgetAtomicity(t *testing.T) {
	if os.Getenv("SANDBOX_REAL_PACKAGES") != "1" {
		t.Skip("prepared production runtime requires SANDBOX_REAL_PACKAGES=1")
	}
	rt, err := (pyrt.Config{Root: t.TempDir()}).Ensure(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ws := pythonQAWorkspace(t)
	s := pythonQASession(t, ws, "store", rt)
	var first string
	for n := 0; n < 3; n++ {
		code := "import os\nfor i in range(4):\n open(os.environ['OUTPUT_DIR']+'/'+str(i)+'.json','w').write('\\\"'+'x'*((5<<20)-2)+'\\\"')"
		result, err := s.PythonTool().Execute(t.Context(), pythonQACall(ws, "store", fmt.Sprint(n), "run_python", map[string]any{"code": code}))
		value := pythonQAObserve(t, result, err)
		if len(value.Artifacts) != 4 {
			t.Fatalf("expected full 20MiB batch: %+v", value)
		}
		if n == 0 {
			first = value.Artifacts[0].ID
		}
	}
	code := "import os\nfor name,size in [('small',1<<20),('large',5<<20)]:\n open(os.environ['OUTPUT_DIR']+'/'+name+'.json','w').write('\\\"'+'x'*(size-2)+'\\\"')"
	result, err := s.PythonTool().Execute(t.Context(), pythonQACall(ws, "store", "overflow", "run_python", map[string]any{"code": code}))
	pythonQAError(t, result, err, pythonx.CodeBudget)
	// Filling the exact remaining 4MiB proves the failed 6MiB batch did not
	// consume a partial 1MiB. The first batch must remain readable at capacity.
	code = "import os\nopen(os.environ['OUTPUT_DIR']+'/remaining.json','w').write('\\\"'+'x'*((4<<20)-2)+'\\\"')"
	result, err = s.PythonTool().Execute(t.Context(), pythonQACall(ws, "store", "fill", "run_python", map[string]any{"code": code}))
	if len(pythonQAObserve(t, result, err).Artifacts) != 1 {
		t.Fatal("store could not use exact remaining capacity")
	}
	artifact, err := s.GetArtifact(ws.ID, "store", first)
	if err != nil || len(artifact.Data) != 5<<20 || !json.Valid(artifact.Data) {
		t.Fatalf("store evicted/corrupted first artifact: bytes=%d error=%v", len(artifact.Data), err)
	}
	pythonQAClean(t, ws)
}
