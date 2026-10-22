package sandbox

import (
	"context"
	"errors"
	"sync"
)

// Script 是 Fake 的一步预设结果。
type Script struct {
	Result Result
	Err    error
}

// Fake 是不执行代码、不访问网络、不启动进程的 Sandbox。
//
// Handler 非 nil 时，Run 记录规格后调用它。
// 否则按 Scripts 依次返回，用尽后重复最后一条。
// Scripts 为空且 Handler 为 nil 时，Run 成功并返回空 Result。
//
// Run 开始前若 ctx 已结束，返回 timeout，不再调用 Handler。
// Handler 执行期间的取消由 Handler 自己处理。
//
// Scripts 在第一次 Run 之后不应再修改。
type Fake struct {
	mu      sync.Mutex
	calls   []Spec
	next    int
	Handler func(ctx context.Context, spec Spec) (Result, error)
	Scripts []Script
}

var _ Sandbox = (*Fake)(nil)

// Run 实现 Sandbox。规格不合法时不记入 Calls。
func (f *Fake) Run(ctx context.Context, spec Spec) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("sandbox: ctx 不能为 nil")
	}
	if err := ValidateSpec(spec); err != nil {
		return Result{}, err
	}
	spec = cloneSpec(spec)
	if err := ctx.Err(); err != nil {
		f.record(spec)
		return align(Result{}, Timeout(err.Error()))
	}
	f.mu.Lock()
	f.calls = append(f.calls, spec)
	handler := f.Handler
	var script Script
	var hasScript bool
	if handler == nil && len(f.Scripts) > 0 {
		idx := f.next
		if idx >= len(f.Scripts) {
			idx = len(f.Scripts) - 1
		} else {
			f.next++
		}
		script = f.Scripts[idx]
		hasScript = true
	}
	f.mu.Unlock()
	if handler != nil {
		res, err := handler(ctx, spec)
		return align(res, err)
	}
	if hasScript {
		return align(script.Result, script.Err)
	}
	return Result{}, nil
}

func (f *Fake) record(spec Spec) {
	f.mu.Lock()
	f.calls = append(f.calls, spec)
	f.mu.Unlock()
}

// Calls 返回已经接受的规格副本。调用方修改返回值不会影响 Fake。
func (f *Fake) Calls() []Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Spec, len(f.calls))
	for i, spec := range f.calls {
		out[i] = cloneSpec(spec)
	}
	return out
}

func cloneSpec(spec Spec) Spec {
	if spec.Inputs != nil {
		inputs := make(map[string][]byte, len(spec.Inputs))
		for key, val := range spec.Inputs {
			inputs[key] = append([]byte(nil), val...)
		}
		spec.Inputs = inputs
	}
	spec.ExtraReadOnly = append([]string(nil), spec.ExtraReadOnly...)
	return spec
}

func cloneResult(res Result) Result {
	res.RuntimeWarnings = append([]string(nil), res.RuntimeWarnings...)
	if res.Artifacts == nil {
		return res
	}
	arts := make([]Artifact, len(res.Artifacts))
	for i, art := range res.Artifacts {
		art.Data = append([]byte(nil), art.Data...)
		arts[i] = art
	}
	res.Artifacts = arts
	return res
}
