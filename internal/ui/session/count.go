package session

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/loveRyujin/lantern/core/tools"
)

// CountTool 在 Execute 内部计数。批准前、拒绝和暂缓都不会增加。
type CountTool struct {
	spec  tools.Spec
	n     atomic.Int64
	mu    sync.Mutex
	calls []tools.Call
}

// NewExportCounter 注册成 export_data，让 Policy 强制确认。
func NewExportCounter() *CountTool {
	return &CountTool{spec: tools.SpecExportData}
}

// Spec 实现 tools.Tool。
func (t *CountTool) Spec() tools.Spec { return t.spec }

// N 返回 Execute 被调用的次数。
func (t *CountTool) N() int64 {
	if t == nil {
		return 0
	}
	return t.n.Load()
}

// Calls 返回 Execute 看到的调用副本。
func (t *CountTool) Calls() []tools.Call {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]tools.Call, len(t.calls))
	for i, call := range t.calls {
		call.Args = append([]byte(nil), call.Args...)
		out[i] = call
	}
	return out
}

// Execute 先计数，再返回固定观察。计数不在调用方。
func (t *CountTool) Execute(ctx context.Context, call tools.Call) (tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	if t == nil {
		return tools.Result{}, context.Canceled
	}
	t.n.Add(1)
	t.mu.Lock()
	call.Args = append([]byte(nil), call.Args...)
	t.calls = append(t.calls, call)
	t.mu.Unlock()
	return tools.Result{Content: "已执行", Summary: "已执行"}, nil
}
