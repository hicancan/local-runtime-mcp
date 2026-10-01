package connection

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestCancellationDoesNotHideOtherJoinedFailures(t *testing.T) {
	want := errors.New("provider failed")
	got := normalizeCancellation(errors.Join(fmt.Errorf("shutdown: %w", context.Canceled), want))
	if !errors.Is(got, want) || errors.Is(got, context.Canceled) {
		t.Fatalf("normalized error = %v", got)
	}
	if normalizeCancellation(fmt.Errorf("stop: %w", context.Canceled)) != nil {
		t.Fatal("orderly cancellation must be clean")
	}
}
