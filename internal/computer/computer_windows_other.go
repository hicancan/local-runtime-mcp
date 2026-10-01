//go:build windows && !amd64

package computer

import (
	"context"
	"errors"
)

type unsupportedController struct{}

func New(ctx context.Context) Controller { return newCoordinator(ctx, &unsupportedController{}) }
func (*unsupportedController) Targets(context.Context) (TargetsResult, error) {
	return TargetsResult{}, errors.New("the Rust computer backend is currently distributed for Windows amd64 only")
}
func (*unsupportedController) State(context.Context, StateOptions) ([]byte, State, error) {
	return nil, State{}, errors.New("the Rust computer backend is currently distributed for Windows amd64 only")
}
func (*unsupportedController) Act(context.Context, Action) (ActionResult, error) {
	return ActionResult{}, errors.New("the Rust computer backend is currently distributed for Windows amd64 only")
}
func (*unsupportedController) Close() error                     { return nil }
func (*unsupportedController) Reset(context.Context) error      { return nil }
func (*unsupportedController) Invalidate(context.Context) error { return nil }
func (*unsupportedController) ShowControl(context.Context, string) error {
	return errors.New("the Rust computer backend is currently distributed for Windows amd64 only")
}
