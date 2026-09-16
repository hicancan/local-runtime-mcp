package process

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	}
	os.Exit(0)
}
