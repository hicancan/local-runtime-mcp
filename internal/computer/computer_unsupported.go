//go:build !windows

package computer

import (
	"context"
	"errors"
)

type unsupportedController struct{}

func New() Controller { return &unsupportedController{} }

func (*unsupportedController) Targets(context.Context) (TargetsResult, error) {
	return TargetsResult{}, errors.New("computer control is currently implemented only on Windows")
}

func (*unsupportedController) State(context.Context, StateOptions) ([]byte, State, error) {
	return nil, State{}, errors.New("computer control is currently implemented only on Windows")
}

func (*unsupportedController) Act(context.Context, Action) (ActionResult, error) {
	return ActionResult{}, errors.New("computer control is currently implemented only on Windows")
}
