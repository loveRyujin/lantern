//go:build !linux || !amd64

package sandbox

import "context"

func (r *Runner) run(_ context.Context, _ Spec) (Result, error) { return Result{}, ErrUnsupported }
