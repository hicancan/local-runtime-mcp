package openai

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRunReconnectsAfterEmbeddedServerConnectionEnds(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var generations atomic.Int32
	reconnected := make(chan struct{})
	factory := func(transport mcp.Transport) (tunnelRunner, error) {
		generation := generations.Add(1)
		return runnerFunc(func(ctx context.Context) error {
			connection, err := transport.Connect(ctx)
			if err != nil {
				return err
			}
			if generation == 1 {
				if err := connection.Close(); err != nil {
					return err
				}
				<-ctx.Done()
				return nil
			}
			close(reconnected)
			<-ctx.Done()
			return nil
		}), nil
	}

	done := make(chan error, 1)
	go func() { done <- run(ctx, server, factory, 0) }()

	select {
	case <-reconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("OpenAI tunnel did not rebuild the in-memory MCP session")
	}
	if got := generations.Load(); got != 2 {
		t.Fatalf("tunnel generations = %d, want 2", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v after cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not stop after cancellation")
	}
}

func TestRunReturnsTunnelClientFailure(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	want := errors.New("tunnel authentication failed")
	var generations atomic.Int32
	factory := func(mcp.Transport) (tunnelRunner, error) {
		generations.Add(1)
		return runnerFunc(func(context.Context) error { return want }), nil
	}

	err := run(context.Background(), server, factory, 0)
	if !errors.Is(err, want) {
		t.Fatalf("run error = %v, want %v", err, want)
	}
	if got := generations.Load(); got != 1 {
		t.Fatalf("tunnel generations = %d, want 1", got)
	}
}

type runnerFunc func(context.Context) error

func (f runnerFunc) Run(ctx context.Context) error {
	return f(ctx)
}
