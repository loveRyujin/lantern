//go:build !darwin

package supervisor

import "context"

func (s *Supervisor) run(context.Context, Request) (Result, error) {
	return Result{State: Failed, Error: ErrUnsupported.Error()}, ErrUnsupported
}
