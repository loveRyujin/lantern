package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"path/filepath"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/errcode"
	"github.com/loveRyujin/lantern/core/sandbox"
)

func TestFakeRunScriptsAndRecordsSpec(t *testing.T) {
	fake := &sandbox.Fake{
		Scripts: []sandbox.Script{{
			Result: sandbox.Result{Stdout: "ok", Artifacts: []sandbox.Artifact{{
				Name: "out.csv",
				Kind: sandbox.KindCSV,
				Data: []byte("a,b\n"),
			}}},
		}},
	}
	raw := []byte("1,2\n")
	spec := sandbox.Spec{
		Code:          "print(1)",
		Inputs:        map[string][]byte{"in/rows.csv": raw},
		ExtraReadOnly: []string{filepath.Clean(t.TempDir())},
		Limits:        sandbox.Limits{Wall: time.Second, Memory: 32 << 20},
	}
	res, err := fake.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run 错误: %v", err)
	}
	if res.Stdout != "ok" || res.ErrKind != "" {
		t.Fatalf("结果 = %+v", res)
	}
	if got := res.Artifacts[0].Data; string(got) != "a,b\n" {
		t.Fatalf("产物 = %q", got)
	}
	raw[0] = '9'
	calls := fake.Calls()
	if len(calls) != 1 || string(calls[0].Inputs["in/rows.csv"]) != "1,2\n" {
		t.Fatalf("Calls 被调用方改写: %+v", calls)
	}
	calls[0].Inputs["in/rows.csv"][0] = '8'
	if string(fake.Calls()[0].Inputs["in/rows.csv"]) != "1,2\n" {
		t.Fatal("Calls 返回的副本和内部状态共享了缓冲区")
	}
}

func TestFakeErrorCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code errcode.Code
		is   error
	}{
		{"timeout", sandbox.Timeout("wall"), errcode.Timeout, sandbox.ErrTimeout},
		{"oom", sandbox.OOM("rss"), errcode.OOM, sandbox.ErrOOM},
		{"killed", sandbox.Killed("signal"), errcode.Killed, sandbox.ErrKilled},
		{"error", sandbox.Failed("exit 1"), errcode.Exec, sandbox.ErrFailed},
		{"runtime_unavailable", sandbox.RuntimeUnavailable("python"), errcode.RuntimeUnavailable, sandbox.ErrRuntimeUnavailable},
		{"sandbox_unavailable", sandbox.SandboxUnavailable("landlock"), errcode.SandboxUnavailable, sandbox.ErrSandboxUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &sandbox.Fake{Scripts: []sandbox.Script{{Err: tc.err}, {Result: sandbox.Result{Stdout: "later"}}}}
			res, err := fake.Run(context.Background(), sandbox.Spec{Code: "print(1)"})
			if !errors.Is(err, tc.is) {
				t.Fatalf("errors.Is = false, err = %v", err)
			}
			wrapped := fmt.Errorf("run: %w", err)
			if !errors.Is(wrapped, tc.is) {
				t.Fatal("包装后 errors.Is 失败")
			}
			got, ok := errcode.CodeOf(err)
			if !ok || got != tc.code || res.ErrKind != tc.code {
				t.Fatalf("码 = %q ok=%v kind=%q", got, ok, res.ErrKind)
			}
			if _, ok := errcode.CodeOf(sandbox.ErrUnsupported); ok {
				t.Fatal("ErrUnsupported 不是稳定错误码")
			}
		})
	}
}

func TestFakeRepeatsLastScript(t *testing.T) {
	fake := &sandbox.Fake{Scripts: []sandbox.Script{{Err: sandbox.OOM("rss")}}}
	for i := 0; i < 2; i++ {
		_, err := fake.Run(context.Background(), sandbox.Spec{Code: "x"})
		if !errors.Is(err, sandbox.ErrOOM) {
			t.Fatalf("第 %d 次 err = %v", i, err)
		}
	}
}

func TestFakeCancelledContextIsTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &sandbox.Fake{Handler: func(context.Context, sandbox.Spec) (sandbox.Result, error) {
		t.Fatal("ctx 已结束时不应该执行 handler")
		return sandbox.Result{}, nil
	}}
	res, err := fake.Run(ctx, sandbox.Spec{Code: "print(1)"})
	if !errors.Is(err, sandbox.ErrTimeout) || res.ErrKind != errcode.Timeout {
		t.Fatalf("err=%v kind=%q", err, res.ErrKind)
	}
	if len(fake.Calls()) != 1 {
		t.Fatalf("Calls = %d", len(fake.Calls()))
	}
}

func TestFakeRejectsInvalidSpecWithoutRecording(t *testing.T) {
	fake := &sandbox.Fake{}
	cases := []sandbox.Spec{
		{},
		{Code: "   "},
		{Code: "x", Inputs: map[string][]byte{"../secret": []byte("a")}},
		{Code: "x", Inputs: map[string][]byte{"/etc/passwd": []byte("a")}},
		{Code: "x", Inputs: map[string][]byte{`a\b`: []byte("a")}},
		{Code: "x", ExtraReadOnly: []string{"relative"}},
		{Code: "x", ExtraReadOnly: []string{"/tmp/../etc"}},
		{Code: "x", Limits: sandbox.Limits{Wall: -time.Second}},
	}
	for _, spec := range cases {
		if _, err := fake.Run(context.Background(), spec); !errors.Is(err, sandbox.ErrInvalidSpec) {
			t.Fatalf("spec %+v err = %v", spec, err)
		}
	}
	if _, err := fake.Run(context.Background(), sandbox.Spec{Code: "ok", Inputs: map[string][]byte{"in/a.csv": []byte("a")}, ExtraReadOnly: []string{filepath.Clean(t.TempDir())}}); err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls()) != 1 {
		t.Fatalf("只应该记下合法调用, Calls=%d", len(fake.Calls()))
	}
}

func TestValidateArtifact(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.ValidateArtifact(sandbox.Artifact{Name: "chart.png", Kind: sandbox.KindPNG, Data: encoded.Bytes()}, 0); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.ValidateArtifact(sandbox.Artifact{Name: "chart.png", Kind: sandbox.KindPNG, Data: []byte("not-png!!!!")}, 0); !errors.Is(err, sandbox.ErrArtifact) {
		t.Fatalf("坏 PNG err=%v", err)
	}
	if err := sandbox.ValidateArtifact(sandbox.Artifact{Name: "rows.csv", Kind: sandbox.KindCSV, Data: []byte("a,b\n1,2\n")}, 0); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.ValidateArtifact(sandbox.Artifact{Name: "rows.csv", Kind: sandbox.KindCSV, Data: []byte{'a', 0, 'b'}}, 0); !errors.Is(err, sandbox.ErrArtifact) {
		t.Fatal("CSV 含 NUL 应该被拒绝")
	}
	if err := sandbox.ValidateArtifact(sandbox.Artifact{Name: "meta.json", Kind: sandbox.KindJSON, Data: []byte(`{"a":1}`)}, 0); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.ValidateArtifact(sandbox.Artifact{Name: "meta.json", Kind: sandbox.KindJSON, Data: []byte("not-json")}, 0); !errors.Is(err, sandbox.ErrArtifact) {
		t.Fatal("坏 JSON 应该被拒绝")
	}
	if err := sandbox.ValidateArtifact(sandbox.Artifact{Name: "../x.csv", Kind: sandbox.KindCSV, Data: []byte("a")}, 0); !errors.Is(err, sandbox.ErrArtifact) {
		t.Fatal("带路径的文件名应该被拒绝")
	}
	if err := sandbox.ValidateArtifact(sandbox.Artifact{Name: "a.txt", Kind: sandbox.KindCSV, Data: []byte("a")}, 0); !errors.Is(err, sandbox.ErrArtifact) {
		t.Fatal("扩展名不一致应该被拒绝")
	}
	if err := sandbox.ValidateArtifact(sandbox.Artifact{Name: "a.csv", Kind: sandbox.KindCSV, Data: []byte("abcd")}, 4); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.ValidateArtifact(sandbox.Artifact{Name: "a.csv", Kind: sandbox.KindCSV, Data: []byte("abcde")}, 4); !errors.Is(err, sandbox.ErrArtifact) {
		t.Fatal("超过本次上限应该被拒绝")
	}
	big := bytes.Repeat([]byte("a"), int(sandbox.MaxArtifactBytes))
	if err := sandbox.ValidateArtifact(sandbox.Artifact{Name: "a.csv", Kind: sandbox.KindCSV, Data: big}, 0); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.ValidateArtifact(sandbox.Artifact{Name: "a.csv", Kind: sandbox.KindCSV, Data: append(big, 'a')}, 0); !errors.Is(err, sandbox.ErrArtifact) {
		t.Fatal("超过 5MiB 应该被拒绝")
	}
}

func TestFakeNilContext(t *testing.T) {
	if _, err := (&sandbox.Fake{}).Run(nil, sandbox.Spec{Code: "x"}); err == nil {
		t.Fatal("nil ctx 应该返回错误")
	}
}
