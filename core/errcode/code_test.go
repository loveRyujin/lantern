package errcode_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/loveRyujin/lantern/core/errcode"
)

func TestCodeValues(t *testing.T) {
	want := map[errcode.Code]string{
		errcode.Timeout:            "timeout",
		errcode.OOM:                "oom",
		errcode.Killed:             "killed",
		errcode.Exec:               "error",
		errcode.RuntimeUnavailable: "runtime_unavailable",
		errcode.SandboxUnavailable: "sandbox_unavailable",
		errcode.EmbeddingMismatch:  "embedding_mismatch",
		errcode.StreamTruncated:    "stream_truncated",
		errcode.PathEscape:         "path_escape",
	}
	if len(want) != 9 {
		t.Fatalf("稳定错误码数量 = %d，想要 9", len(want))
	}
	for code, text := range want {
		if code.String() != text {
			t.Fatalf("Code.String() = %q，想要 %q", code.String(), text)
		}
		if !code.Known() {
			t.Fatalf("%q 应该是已知错误码", text)
		}
	}
	if errcode.Code("nope").Known() {
		t.Fatal("未知码不应该 Known")
	}
	if errcode.Code("").Known() {
		t.Fatal("空码不应该 Known")
	}
}

type stub struct{ code errcode.Code }

func (s stub) Error() string      { return "stub" }
func (s stub) Code() errcode.Code { return s.code }

func TestCodeOf(t *testing.T) {
	wrapped := fmt.Errorf("外层: %w", stub{code: errcode.Timeout})
	got, ok := errcode.CodeOf(wrapped)
	if !ok || got != errcode.Timeout {
		t.Fatalf("CodeOf = %q, %v", got, ok)
	}
	if _, ok := errcode.CodeOf(errors.New("普通错误")); ok {
		t.Fatal("普通错误不应该有错误码")
	}
	if _, ok := errcode.CodeOf(nil); ok {
		t.Fatal("nil 不应该有错误码")
	}
	if _, ok := errcode.CodeOf(stub{code: "nope"}); ok {
		t.Fatal("未知码不应该被 CodeOf 接受")
	}
}
