//go:build windows && !amd64

package computer

import (
	"context"
	"errors"
)

type unsupportedController struct{}

func New(context.Context) Controller { return &unsupportedController{} }
func (*unsupportedController) Targets(context.Context) (TargetsResult, error) {
	return TargetsResult{}, errors.New("the Rust computer backend is currently distributed for Windows amd64 only")
}
func (*unsupportedController) State(context.Context, StateOptions) ([]byte, State, error) {
	return nil, State{}, errors.New("the Rust computer backend is currently distributed for Windows amd64 only")
}
func (*unsupportedController) Act(context.Context, Action) (ActionResult, error) {
	return ActionResult{}, errors.New("the Rust computer backend is currently distributed for Windows amd64 only")
}
func (*unsupportedController) Close() error { return nil }
