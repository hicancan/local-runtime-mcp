package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestManagedProcessLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := NewManager(ctx)
	result, err := manager.Run(context.Background(), Options{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "delayed"},
		Environment: map[string]string{"LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, YieldTimeMS: 30,
	})
	if err != nil || !result.Running || result.SessionID == "" || !strings.Contains(result.Stdout, "first") {
		t.Fatalf("initial result = %+v, %v", result, err)
	}
	completed, err := manager.Continue(context.Background(), ContinueOptions{SessionID: result.SessionID, YieldTimeMS: 2000})
	if err != nil || completed.Running || completed.ExitCode != 0 || !strings.Contains(completed.Stdout, "second") {
		t.Fatalf("completed result = %+v, %v", completed, err)
	}
}

func TestManagedProcessStdinAndOutputLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := NewManager(ctx)
	started, err := manager.Run(context.Background(), Options{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "stdin"},
		Environment: map[string]string{"LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, KeepStdinOpen: true, YieldTimeMS: 20,
	})
	if err != nil || !started.Running {
		t.Fatalf("stdin process = %+v, %v", started, err)
	}
	completed, err := manager.Continue(context.Background(), ContinueOptions{SessionID: started.SessionID, Stdin: "hello", CloseStdin: true, YieldTimeMS: 2000})
	if err != nil || completed.Stdout != "hello" || completed.ExitCode != 0 {
		t.Fatalf("stdin completion = %+v, %v", completed, err)
	}
	limited, err := manager.Run(context.Background(), Options{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "output"},
		Environment: map[string]string{"LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, MaxOutputBytes: 10, YieldTimeMS: 2000,
	})
	if err != nil || limited.Running || limited.Stdout != strings.Repeat("x", 10) || !limited.StdoutTruncated {
		t.Fatalf("limited output = %+v, %v", limited, err)
	}
}

func TestPTYProcessLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := NewManager(ctx)
	result, err := manager.Run(context.Background(), Options{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "delayed"},
		Environment: map[string]string{"LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, IOMode: "pty", Columns: 100, Rows: 30, YieldTimeMS: 30,
	})
	if err != nil {
		t.Skipf("PTY unavailable: %v", err)
	}
	if !result.Running || result.SessionID == "" || result.IOMode != "pty" {
		t.Fatalf("initial PTY result = %+v", result)
	}
	completed, err := manager.Continue(context.Background(), ContinueOptions{SessionID: result.SessionID, Columns: 120, Rows: 40, YieldTimeMS: 2000})
	terminalOutput := result.Stdout + completed.Stdout
	if err != nil || completed.Running || completed.ExitCode != 0 || !strings.Contains(terminalOutput, "first") || !strings.Contains(terminalOutput, "second") || completed.Stderr != "" {
		t.Fatalf("completed PTY result = %+v, %v", completed, err)
	}
}

func TestPTYResolvesBareProgramThroughPATH(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := NewManager(ctx)
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(executable)+string(os.PathListSeparator)+os.Getenv("PATH"))
	result, err := manager.Run(context.Background(), Options{
		Program: filepath.Base(executable), Args: []string{"-test.run=TestProcessHelper", "--", "delayed"},
		Environment: map[string]string{"LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, IOMode: "pty", YieldTimeMS: 30,
	})
	if err != nil {
		t.Fatalf("bare PTY executable did not resolve through PATH: %v", err)
	}
	output := result.Stdout
	if result.Running {
		completed, continueErr := manager.Continue(context.Background(), ContinueOptions{SessionID: result.SessionID, YieldTimeMS: 2000})
		if continueErr != nil {
			t.Fatal(continueErr)
		}
		result = completed
		output += completed.Stdout
	}
	if result.Running || result.ExitCode != 0 || !strings.Contains(output, "first") || !strings.Contains(output, "second") {
		t.Fatalf("bare PTY executable result = %+v output=%q", result, output)
	}
}

func TestProcessHelper(t *testing.T) {
	if os.Getenv("LOCAL_RUNTIME_MCP_PROCESS_HELPER") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "delayed":
		fmt.Print("first\n")
		time.Sleep(150 * time.Millisecond)
		fmt.Print("second\n")
	case "stdin":
		data, _ := io.ReadAll(os.Stdin)
		fmt.Print(string(data))
	case "output":
		fmt.Print(strings.Repeat("x", 100))
	case "blocked_stdin":
		fmt.Print("ready\n")
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}

func TestTerminateUnblocksStdinAndCloseJoins(t *testing.T) {
	manager := NewManager(context.Background())
	t.Cleanup(func() { _ = manager.Close() })
	started, err := manager.Run(context.Background(), Options{Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "blocked_stdin"}, Environment: map[string]string{"LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, KeepStdinOpen: true, YieldTimeMS: 20})
	if err != nil || !started.Running {
		t.Fatalf("start = %+v, %v", started, err)
	}
	writeDone := make(chan error, 1)
	go func() {
		_, err := manager.Continue(context.Background(), ContinueOptions{SessionID: started.SessionID, Stdin: strings.Repeat("x", 8<<20)})
		writeDone <- err
	}()
	// Observe the input gate instead of depending on child startup timing.
	manager.mu.Lock()
	entry := manager.sessions[started.SessionID]
	manager.mu.Unlock()
	deadline := time.After(3 * time.Second)
	for len(entry.inputGate) == 0 {
		select {
		case <-deadline:
			t.Fatal("stdin writer did not start")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	terminated, err := manager.Continue(context.Background(), ContinueOptions{SessionID: started.SessionID, Terminate: true, YieldTimeMS: 3000})
	if err != nil || terminated.Running {
		t.Fatalf("terminate = %+v, %v", terminated, err)
	}
	select {
	case <-writeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("blocked stdin was not released")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Run(context.Background(), Options{Program: os.Args[0]}); err == nil {
		t.Fatal("closed manager admitted a process")
	}
}

func TestInitialStdinCancellationDoesNotLeak(t *testing.T) {
	manager := NewManager(context.Background())
	defer manager.Close()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, err := manager.Run(ctx, Options{Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "blocked_stdin"}, Environment: map[string]string{"LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, Stdin: strings.Repeat("x", 8<<20), YieldTimeMS: 60000})
		finished <- err
	}()
	deadline := time.After(3 * time.Second)
	for {
		manager.mu.Lock()
		count := len(manager.sessions)
		manager.mu.Unlock()
		if count > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("process did not start")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("initial stdin cancellation blocked")
	}
	manager.mu.Lock()
	count := len(manager.sessions)
	manager.mu.Unlock()
	if count != 0 {
		t.Fatalf("canceled Run left %d sessions", count)
	}
}

func TestContinueCancellationOnlyCancelsWait(t *testing.T) {
	manager := NewManager(context.Background())
	defer manager.Close()
	started, err := manager.Run(context.Background(), Options{Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "blocked_stdin"}, Environment: map[string]string{"LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, YieldTimeMS: 10})
	if err != nil || !started.Running {
		t.Fatalf("start = %+v, %v", started, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := manager.Continue(ctx, ContinueOptions{SessionID: started.SessionID, YieldTimeMS: 60000}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait cancellation = %v", err)
	}
	result, err := manager.Continue(context.Background(), ContinueOptions{SessionID: started.SessionID, YieldTimeMS: 1})
	if err != nil || !result.Running {
		t.Fatalf("wait cancellation killed process: %+v, %v", result, err)
	}
}

func TestConcurrentOutputDrainIsOneConsumer(t *testing.T) {
	entry := &session{stdout: newStreamBuffer(100), stderr: newStreamBuffer(100), done: make(chan struct{}), started: time.Now(), exitCode: -1}
	entry.stdout.Write([]byte("out"))
	entry.stderr.Write([]byte("err"))
	results := make(chan Result, 2)
	var calls sync.WaitGroup
	for range 2 {
		calls.Add(1)
		go func() { defer calls.Done(); results <- entry.result(true) }()
	}
	calls.Wait()
	close(results)
	owners := 0
	for result := range results {
		if result.Stdout != "" || result.Stderr != "" {
			owners++
			if result.Stdout != "out" || result.Stderr != "err" {
				t.Fatalf("split result: %+v", result)
			}
		}
	}
	if owners != 1 {
		t.Fatalf("output consumed %d times", owners)
	}
}

func TestChildPATHOverridesApplyToPipeAndPTY(t *testing.T) {
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"pipe", "pty"} {
		t.Run(mode, func(t *testing.T) {
			manager := NewManager(context.Background())
			defer manager.Close()
			result, err := manager.Run(context.Background(), Options{Program: filepath.Base(executable), Args: []string{"-test.run=TestProcessHelper", "--", "output"}, Environment: map[string]string{"PATH": filepath.Dir(executable), "LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, IOMode: mode, YieldTimeMS: 3000})
			if err != nil || result.Running || result.ExitCode != 0 || !strings.Contains(result.Stdout, "xxx") {
				t.Fatalf("child PATH result = %+v, %v", result, err)
			}
		})
	}
}

func TestCloseUnblocksOutstandingStdin(t *testing.T) {
	for _, mode := range []string{"pipe", "pty"} {
		t.Run(mode, func(t *testing.T) {
			manager := NewManager(context.Background())
			defer manager.Close()
			started, err := manager.Run(context.Background(), Options{Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "blocked_stdin"}, Environment: map[string]string{"LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, KeepStdinOpen: true, IOMode: mode, YieldTimeMS: 10})
			if err != nil || !started.Running {
				t.Fatalf("start = %+v, %v", started, err)
			}
			writeDone := make(chan error, 1)
			go func() {
				_, err := manager.Continue(context.Background(), ContinueOptions{SessionID: started.SessionID, Stdin: strings.Repeat("x", 8<<20)})
				writeDone <- err
			}()
			manager.mu.Lock()
			entry := manager.sessions[started.SessionID]
			manager.mu.Unlock()
			deadline := time.After(3 * time.Second)
			for len(entry.inputGate) == 0 {
				select {
				case <-deadline:
					t.Fatal("stdin writer did not start")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			closed := make(chan struct{})
			go func() { _ = manager.Close(); close(closed) }()
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("Close did not join blocked process")
			}
			select {
			case <-writeDone:
			case <-time.After(time.Second):
				t.Fatal("Close left blocked stdin")
			}
			manager.mu.Lock()
			count := len(manager.sessions)
			manager.mu.Unlock()
			if count != 0 {
				t.Fatalf("Close retained %d sessions", count)
			}
		})
	}
}

func TestSeparateSessionsRunConcurrently(t *testing.T) {
	manager := NewManager(context.Background())
	defer manager.Close()
	start := make(chan struct{})
	results := make(chan Result, 2)
	errors := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			result, err := manager.Run(context.Background(), Options{Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "blocked_stdin"}, Environment: map[string]string{"LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, YieldTimeMS: 10})
			results <- result
			errors <- err
		}()
	}
	close(start)
	first, second := <-results, <-results
	if firstErr, secondErr := <-errors, <-errors; firstErr != nil || secondErr != nil || !first.Running || !second.Running || first.SessionID == second.SessionID {
		t.Fatalf("parallel sessions = %+v %+v, %v %v", first, second, firstErr, secondErr)
	}
	manager.mu.Lock()
	count := len(manager.sessions)
	manager.mu.Unlock()
	if count != 2 {
		t.Fatalf("parallel sessions retained %d processes", count)
	}
}
