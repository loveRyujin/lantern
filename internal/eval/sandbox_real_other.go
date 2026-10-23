//go:build !linux || !amd64

package eval

import (
	"context"
	"fmt"
	"github.com/loveRyujin/lantern/core/pyrt"
	"runtime"
)

func newSandboxRealExecutor(context.Context, pyrt.Runtime) (sandboxRealExecutor, map[string]any, error) {
	return nil, map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH}, fmt.Errorf("sandbox-real requires Linux amd64; no fallback")
}
