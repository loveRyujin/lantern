package agent

import (
	"context"

	"github.com/loveRyujin/lantern/core/run"
)

// persistence 把运行状态和步骤接到 core/run。Agent 不自己写 runs 表。
type persistence struct {
	svc *run.Service
}

// Persist 返回给 Loop.Status 和 Loop.Steps 用的同一套存储。
// svc 为 nil 时两个返回值都是 nil，循环保持原来的不落库行为。
func Persist(svc *run.Service) (StatusStore, StepStore) {
	if svc == nil {
		return nil, nil
	}
	p := persistence{svc: svc}
	return p, p
}

func (p persistence) Set(ctx context.Context, runID, status string) error {
	return p.svc.Set(ctx, runID, status)
}

func (p persistence) Begin(ctx context.Context, step StepMark) error {
	return p.svc.Begin(ctx, step.ID, step.RunID, step.ToolName, step.Input)
}

func (p persistence) Finish(ctx context.Context, id, status string) error {
	return p.svc.Finish(ctx, id, status)
}
