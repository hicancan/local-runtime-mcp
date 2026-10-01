//go:build !windows

package computer

import (
	"context"
	"errors"
)

type unsupportedController struct{}

func New(ctx context.Context) Controller { return newCoordinator(ctx, &unsupportedController{}) }

func (*unsupportedController) Targets(context.Context) (TargetsResult, error) {
	return TargetsResult{}, errors.New("computer control is currently implemented only on Windows")
}

func (*unsupportedController) State(context.Context, StateOptions) ([]byte, State, error) {
	return nil, State{}, errors.New("computer control is currently implemented only on Windows")
}

func (*unsupportedController) Act(context.Context, Action) (ActionResult, error) {
	return ActionResult{}, errors.New("computer control is currently implemented only on Windows")
}
func (*unsupportedController) Close() error                     { return nil }
func (*unsupportedController) Reset(context.Context) error      { return nil }
func (*unsupportedController) Invalidate(context.Context) error { return nil }
func (*unsupportedController) ShowControl(context.Context, string) error {
	return errors.New("computer control is currently implemented only on Windows")
}
