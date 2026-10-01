package process

import (
	"context"
	"encoding/json"
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
	completed, err := manager.Continue(context.Background(), ContinueOptions{SessionID: result.SessionID, OutputCursor: result.OutputCursor, YieldTimeMS: 2000})
	if err != nil || completed.Running || completed.ExitCode != 0 || !strings.Contains(completed.Stdout, "second") {
		t.Fatalf("completed result = %+v, %v", completed, err)
	}
}

func TestProcessDurationStopsAtExit(t *testing.T) {
	for _, mode := range []string{"pipe", "pty"} {
		t.Run(mode, func(t *testing.T) {
			manager := NewManager(context.Background())
			t.Cleanup(func() { _ = manager.Close() })
			started, err := manager.Run(context.Background(), Options{
				Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "delayed"},
				Environment: map[string]string{"LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, IOMode: mode, YieldTimeMS: 1,
			})
			if err != nil || !started.Running || started.SessionID == "" {
				t.Fatalf("start = %+v, %v", started, err)
			}
			manager.mu.Lock()
			entry := manager.sessions[started.SessionID]
			manager.mu.Unlock()
			select {
			case <-entry.done:
			case <-time.After(5 * time.Second):
				t.Fatal("process did not exit")
			}
			exited := readSessionResult(t, entry, false, started.OutputCursor)
			if exited.Running || exited.ExitCode != 0 || exited.DurationMS < started.DurationMS {
				t.Fatalf("exited = %+v; started = %+v", exited, started)
			}
			// Delay collection after the process has exited. The elapsed lifetime
			// must not include how long the client waited before collecting it.
			time.Sleep(60 * time.Millisecond)
			collected, err := manager.Continue(context.Background(), ContinueOptions{SessionID: started.SessionID, OutputCursor: started.OutputCursor, YieldTimeMS: 1})
			if err != nil || collected.Running || collected.ExitCode != 0 || collected.DurationMS != exited.DurationMS {
				t.Fatalf("delayed collection changed duration: exited=%+v collected=%+v, %v", exited, collected, err)
			}
		})
	}
}

func TestRunningProcessDurationAdvances(t *testing.T) {
	entry := &session{
		stdout: newStreamBuffer(100), stderr: newStreamBuffer(100),
		started: time.Now(), done: make(chan struct{}), exitCode: -1,
	}
	first := readSessionResult(t, entry, true, OutputCursor{})
	time.Sleep(25 * time.Millisecond)
	second := readSessionResult(t, entry, true, first.OutputCursor)
	if !first.Running || !second.Running || second.DurationMS <= first.DurationMS {
		t.Fatalf("running elapsed did not advance: first=%+v second=%+v", first, second)
	}
}

type blockedCloseControl struct {
	closing chan struct{}
	resume  chan struct{}
}

func (*blockedCloseControl) Kill() error { return nil }

func (c *blockedCloseControl) Close() error {
	close(c.closing)
	<-c.resume
	return nil
}

func TestProcessDurationExcludesCleanup(t *testing.T) {
	control := &blockedCloseControl{closing: make(chan struct{}), resume: make(chan struct{})}
	entry := &session{
		stdout: newStreamBuffer(100), stderr: newStreamBuffer(100),
		started: time.Now().Add(-time.Second), done: make(chan struct{}), exitCode: -1,
		waitProcess: func() error { return nil }, control: control,
	}
	go entry.wait()
	t.Cleanup(func() {
		close(control.resume)
		select {
		case <-entry.done:
		case <-time.After(time.Second):
			t.Error("cleanup did not finish")
		}
	})
	select {
	case <-control.closing:
	case <-time.After(time.Second):
		t.Fatal("process wait did not finish")
	}
	finished := readSessionResult(t, entry, true, OutputCursor{})
	time.Sleep(25 * time.Millisecond)
	// Cleanup has not completed; concurrent calls must still return the fixed
	// process lifetime, rather than adding time spent joining resources.
	var calls sync.WaitGroup
	for range 8 {
		calls.Add(1)
		go func() {
			defer calls.Done()
			result := readSessionResult(t, entry, true, finished.OutputCursor)
			if result.DurationMS != finished.DurationMS {
				t.Errorf("cleanup changed duration: finished=%+v result=%+v", finished, result)
			}
		}()
	}
	calls.Wait()
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
	completed, err := manager.Continue(context.Background(), ContinueOptions{SessionID: started.SessionID, OutputCursor: started.OutputCursor, Stdin: "hello", CloseStdin: true, YieldTimeMS: 2000})
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
	completed, err := manager.Continue(context.Background(), ContinueOptions{SessionID: result.SessionID, OutputCursor: result.OutputCursor, Columns: 120, Rows: 40, YieldTimeMS: 2000})
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
		completed, continueErr := manager.Continue(context.Background(), ContinueOptions{SessionID: result.SessionID, OutputCursor: result.OutputCursor, YieldTimeMS: 2000})
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
	manager.mu.Lock()
	entry := manager.sessions[started.SessionID]
	manager.mu.Unlock()
	_, _ = entry.stdout.Write([]byte("preserved stdout\n"))
	_, _ = entry.stderr.Write([]byte("preserved stderr\n"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := manager.Continue(ctx, ContinueOptions{SessionID: started.SessionID, OutputCursor: started.OutputCursor, YieldTimeMS: 60000}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait cancellation = %v", err)
	}
	result, err := manager.Continue(context.Background(), ContinueOptions{SessionID: started.SessionID, OutputCursor: started.OutputCursor, YieldTimeMS: 1})
	if err != nil || !result.Running || !strings.Contains(result.Stdout, "preserved stdout") || result.Stderr != "preserved stderr\n" {
		t.Fatalf("wait cancellation killed process: %+v, %v", result, err)
	}
}

func TestConcurrentOutputReadsAreIndependent(t *testing.T) {
	entry := &session{stdout: newStreamBuffer(100), stderr: newStreamBuffer(100), done: make(chan struct{}), started: time.Now(), exitCode: -1}
	entry.stdout.Write([]byte("out"))
	entry.stderr.Write([]byte("err"))
	results := make(chan Result, 2)
	var calls sync.WaitGroup
	for range 2 {
		calls.Add(1)
		go func() { defer calls.Done(); results <- readSessionResult(t, entry, true, OutputCursor{}) }()
	}
	calls.Wait()
	close(results)
	for result := range results {
		if result.Stdout != "out" || result.Stderr != "err" || result.OutputCursor != (OutputCursor{Stdout: 3, Stderr: 3}) {
			t.Fatalf("reader consumed another reader's output: %+v", result)
		}
	}
}

func readSessionResult(t *testing.T, entry *session, running bool, cursor OutputCursor) Result {
	t.Helper()
	result, err := entry.result(running, cursor)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestStreamBufferCursorReadsAndBoundedTail(t *testing.T) {
	buffer := newStreamBuffer(5)
	if _, err := buffer.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		value, start, next, truncated, err := buffer.Read(0, false)
		if err != nil || value != "abc" || start != 0 || next != 3 || truncated {
			t.Fatalf("repeat read: %q %d..%d truncated=%v err=%v", value, start, next, truncated, err)
		}
	}
	_, _ = buffer.Write([]byte("defg"))
	for _, test := range []struct {
		cursor    int64
		value     string
		start     int64
		truncated bool
	}{{0, "cdefg", 2, true}, {2, "cdefg", 2, false}, {3, "defg", 3, false}, {7, "", 7, false}} {
		value, start, next, truncated, err := buffer.Read(test.cursor, false)
		if err != nil || value != test.value || start != test.start || next != 7 || truncated != test.truncated {
			t.Fatalf("cursor=%d: %q %d..%d truncated=%v err=%v", test.cursor, value, start, next, truncated, err)
		}
	}
	for _, cursor := range []int64{-1, 8} {
		if _, _, _, _, err := buffer.Read(cursor, false); err == nil {
			t.Fatalf("accepted invalid cursor %d", cursor)
		}
	}
	_, _ = buffer.Write([]byte("123456789"))
	value, start, next, truncated, err := buffer.Read(7, false)
	if err != nil || value != "56789" || start != 11 || next != 16 || !truncated || len(buffer.data) != 5 {
		t.Fatalf("large write: %q %d..%d truncated=%v err=%v", value, start, next, truncated, err)
	}
	// Exactly filling a fresh buffer loses no bytes.
	exact := newStreamBuffer(5)
	_, _ = exact.Write([]byte("12345"))
	if _, _, _, truncated, err := exact.Read(0, false); err != nil || truncated {
		t.Fatalf("exact fill incorrectly reports loss: truncated=%v err=%v", truncated, err)
	}
}

func TestStreamBufferPreservesUTF8AcrossWrites(t *testing.T) {
	buffer := newStreamBuffer(100)
	chinese, emoji := []byte("中"), []byte("😀")
	for _, prefix := range chinese[:2] {
		_, _ = buffer.Write([]byte{prefix})
		value, _, next, _, err := buffer.Read(0, false)
		if err != nil || value != "" || next != 0 {
			t.Fatalf("unfinished Chinese prefix was delivered: %q next=%d err=%v", value, next, err)
		}
	}
	_, _ = buffer.Write(chinese[2:])
	value, start, next, truncated, err := buffer.Read(0, false)
	if err != nil || value != "中" || start != 0 || next != 3 || truncated {
		t.Fatalf("complete Chinese rune: %q %d..%d truncated=%v err=%v", value, start, next, truncated, err)
	}
	for _, prefix := range emoji[:3] {
		_, _ = buffer.Write([]byte{prefix})
		value, _, next, _, err := buffer.Read(3, false)
		if err != nil || value != "" || next != 3 {
			t.Fatalf("unfinished emoji prefix was delivered: %q next=%d err=%v", value, next, err)
		}
	}
	_, _ = buffer.Write(emoji[3:])
	value, _, next, _, err = buffer.Read(3, false)
	if err != nil || value != "😀" || next != 7 {
		t.Fatalf("complete emoji: %q next=%d err=%v", value, next, err)
	}
	// An explicit cursor inside a complete retained rune reports its skipped
	// original bytes rather than delivering invalid continuation bytes.
	value, start, next, truncated, err = buffer.Read(1, false)
	if err != nil || value != "😀" || start != 3 || next != 7 || !truncated {
		t.Fatalf("unaligned cursor: %q %d..%d truncated=%v err=%v", value, start, next, truncated, err)
	}
	bounded := newStreamBuffer(4)
	_, _ = bounded.Write([]byte("中😀!"))
	value, start, next, truncated, err = bounded.Read(0, false)
	if err != nil || value != "!" || start != 7 || next != 8 || !truncated {
		t.Fatalf("bounded tail cut into emoji: %q %d..%d truncated=%v err=%v", value, start, next, truncated, err)
	}
}

func TestStreamBufferFlushesIncompleteAndInvalidBytesAtExit(t *testing.T) {
	buffer := newStreamBuffer(100)
	_, _ = buffer.Write([]byte{'x', 0xe4, 0xb8})
	value, _, next, _, err := buffer.Read(0, false)
	if err != nil || value != "x" || next != 1 {
		t.Fatalf("running partial suffix: %q next=%d err=%v", value, next, err)
	}
	value, start, next, truncated, err := buffer.Read(1, true)
	if err != nil || start != 1 || next != 3 || truncated {
		t.Fatalf("completed partial suffix: %q %d..%d truncated=%v err=%v", value, start, next, truncated, err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded string
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != "��" {
		t.Fatalf("completed invalid bytes should retain JSON replacement semantics: %q err=%v", decoded, err)
	}
	invalid := newStreamBuffer(100)
	_, _ = invalid.Write([]byte{0x80, 0xff, 0xf5})
	value, start, next, truncated, err = invalid.Read(0, false)
	if err != nil || len(value) != 3 || start != 0 || next != 3 || truncated {
		t.Fatalf("original invalid bytes were held or silently skipped: %q %d..%d truncated=%v err=%v", value, start, next, truncated, err)
	}
}

func TestContinueRejectsInvalidCursorBeforeInputOrTermination(t *testing.T) {
	manager := NewManager(context.Background())
	t.Cleanup(func() { _ = manager.Close() })
	started, err := manager.Run(context.Background(), Options{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "stdin"},
		Environment: map[string]string{"LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, KeepStdinOpen: true, YieldTimeMS: 1,
	})
	if err != nil || !started.Running {
		t.Fatalf("start: %+v, %v", started, err)
	}
	for _, cursor := range []OutputCursor{{Stdout: -1}, {Stderr: -1}, {Stdout: 1}, {Stderr: 1}} {
		for _, options := range []ContinueOptions{
			{Stdin: "unwanted", CloseStdin: true},
			{Terminate: true},
			{Columns: 80, Rows: 25},
		} {
			options.SessionID, options.OutputCursor = started.SessionID, cursor
			if _, err := manager.Continue(context.Background(), options); err == nil || !strings.Contains(err.Error(), "output cursor") {
				t.Fatalf("cursor=%+v options=%+v: %v", cursor, options, err)
			}
		}
	}
	if _, err := manager.Continue(context.Background(), ContinueOptions{SessionID: started.SessionID, Stdin: "unwanted", Columns: 80, Rows: 25}); err == nil || !strings.Contains(err.Error(), "PTY") {
		t.Fatalf("pipe resize should be rejected before writing stdin: %v", err)
	}
	completed, err := manager.Continue(context.Background(), ContinueOptions{
		SessionID: started.SessionID, OutputCursor: started.OutputCursor, Stdin: "wanted", CloseStdin: true, YieldTimeMS: 2000,
	})
	if err != nil || completed.Running || completed.ExitCode != 0 || completed.Stdout != "wanted" {
		t.Fatalf("invalid cursors caused an input or control side effect: %+v, %v", completed, err)
	}
}

func TestCompletedAsyncSessionCanBeReadAgain(t *testing.T) {
	manager := NewManager(context.Background())
	t.Cleanup(func() { _ = manager.Close() })
	started, err := manager.Run(context.Background(), Options{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "delayed"},
		Environment: map[string]string{"LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, YieldTimeMS: 1,
	})
	if err != nil || !started.Running {
		t.Fatalf("start: %+v, %v", started, err)
	}
	options := ContinueOptions{SessionID: started.SessionID, OutputCursor: started.OutputCursor, YieldTimeMS: 2000}
	completed, err := manager.Continue(context.Background(), options)
	if err != nil || completed.Running || !strings.Contains(completed.Stdout, "second") {
		t.Fatalf("complete: %+v, %v", completed, err)
	}
	retried, err := manager.Continue(context.Background(), options)
	if err != nil || retried.Running || retried.Stdout != completed.Stdout || retried.Stderr != completed.Stderr || retried.OutputCursor != completed.OutputCursor || retried.DurationMS != completed.DurationMS {
		t.Fatalf("completed read changed: first=%+v retried=%+v err=%v", completed, retried, err)
	}
	options.OutputCursor = completed.OutputCursor
	incremental, err := manager.Continue(context.Background(), options)
	if err != nil || incremental.Stdout != "" || incremental.Stderr != "" || incremental.StdoutTruncated || incremental.StderrTruncated {
		t.Fatalf("acknowledged output was repeated: %+v, %v", incremental, err)
	}
}

func TestRunEvictsOldestCompletedSessionAtCapacity(t *testing.T) {
	manager := NewManager(context.Background())
	t.Cleanup(func() { _ = manager.Close() })
	for index := range maxSessions {
		id := fmt.Sprintf("completed-%03d", index)
		entry := &session{id: id, done: make(chan struct{}), retired: make(chan struct{}), finished: time.Now().Add(time.Duration(index-maxSessions) * time.Second)}
		close(entry.done)
		manager.sessions[id] = entry
	}
	oldest := manager.sessions["completed-000"]
	result, err := manager.Run(context.Background(), Options{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "output"},
		Environment: map[string]string{"LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, YieldTimeMS: 2000,
	})
	if err != nil || result.Running || result.ExitCode != 0 {
		t.Fatalf("completed retention blocked admission: %+v, %v", result, err)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.sessions["completed-000"] != nil || manager.sessions["completed-001"] == nil || len(manager.sessions) != maxSessions-1 {
		t.Fatalf("incorrect eviction or synchronous retention: count=%d oldest=%v next=%v", len(manager.sessions), manager.sessions["completed-000"], manager.sessions["completed-001"])
	}
	select {
	case <-oldest.retired:
	default:
		t.Fatal("capacity eviction did not retire its retention worker")
	}
}

func TestRunCapacityDoesNotEvictActiveSessions(t *testing.T) {
	manager := &Manager{ctx: context.Background(), sessions: make(map[string]*session)}
	for index := range maxSessions {
		id := fmt.Sprintf("active-%03d", index)
		manager.sessions[id] = &session{id: id, done: make(chan struct{})}
	}
	if _, err := manager.Run(context.Background(), Options{Program: os.Args[0]}); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("active capacity should reject admission: %v", err)
	}
	if len(manager.sessions) != maxSessions {
		t.Fatalf("active sessions were evicted: %d", len(manager.sessions))
	}
}

func TestInvalidRunDoesNotEvictRetainedOutput(t *testing.T) {
	manager := &Manager{ctx: context.Background(), sessions: make(map[string]*session)}
	for index := range maxSessions {
		id := fmt.Sprintf("completed-%03d", index)
		entry := &session{id: id, done: make(chan struct{}), retired: make(chan struct{}), finished: time.Now().Add(time.Duration(index-maxSessions) * time.Second)}
		close(entry.done)
		manager.sessions[id] = entry
	}
	oldest := manager.sessions["completed-000"]
	directory := t.TempDir()
	for _, options := range []Options{
		{Program: ""},
		{Program: filepath.Join(directory, "missing-executable")},
		{Program: os.Args[0], Directory: filepath.Join(directory, "missing-directory")},
	} {
		if _, err := manager.Run(context.Background(), options); err == nil {
			t.Fatalf("accepted invalid options: %+v", options)
		}
		if len(manager.sessions) != maxSessions || manager.sessions[oldest.id] != oldest {
			t.Fatalf("invalid run evicted retained output: %+v", options)
		}
		select {
		case <-oldest.retired:
			t.Fatal("invalid run retired retained output")
		default:
		}
	}
}

func TestSynchronousRunRetiresWorkersImmediately(t *testing.T) {
	manager := NewManager(context.Background())
	t.Cleanup(func() { _ = manager.Close() })
	result, err := manager.Run(context.Background(), Options{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "output"},
		Environment: map[string]string{"LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, YieldTimeMS: 2000,
	})
	if err != nil || result.Running {
		t.Fatalf("synchronous run: %+v, %v", result, err)
	}
	joined := make(chan struct{})
	go func() { manager.workers.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("synchronous run retained workers after collection")
	}
}

func TestStartingReservationEndsWhenSessionRegisters(t *testing.T) {
	manager := NewManager(context.Background())
	t.Cleanup(func() { _ = manager.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := manager.Run(ctx, Options{
			Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "blocked_stdin"},
			Environment: map[string]string{"LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, YieldTimeMS: 60000,
		})
		finished <- err
	}()
	deadline := time.After(3 * time.Second)
	for {
		manager.mu.Lock()
		registered, reserved := len(manager.sessions), manager.startingCount
		manager.mu.Unlock()
		if registered > 0 {
			if registered != 1 || reserved != 0 {
				t.Fatalf("session counted both as registered and starting: registered=%d reserved=%d", registered, reserved)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("process did not register")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case err := <-finished:
		t.Fatalf("initial Run unexpectedly finished before handoff: %v", err)
	default:
	}
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("initial cancellation: %v", err)
	}
}

func TestDeliveredSessionSurvivesInitialContextCancellation(t *testing.T) {
	manager := NewManager(context.Background())
	t.Cleanup(func() { _ = manager.Close() })
	for iteration := range 16 {
		ctx, cancel := context.WithCancel(context.Background())
		started, err := manager.Run(ctx, Options{
			Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "stdin"},
			Environment: map[string]string{"LOCAL_RUNTIME_MCP_PROCESS_HELPER": "1"}, KeepStdinOpen: true, YieldTimeMS: 1,
		})
		cancel()
		if err != nil || !started.Running {
			t.Fatalf("iteration %d launch: %+v, %v", iteration, started, err)
		}
		completed, err := manager.Continue(context.Background(), ContinueOptions{
			SessionID: started.SessionID, OutputCursor: started.OutputCursor, Stdin: "survived", CloseStdin: true, YieldTimeMS: 2000,
		})
		if err != nil || completed.Running || completed.ExitCode != 0 || completed.Stdout != "survived" {
			t.Fatalf("iteration %d initial cancellation killed delivered session: %+v, %v", iteration, completed, err)
		}
	}
}

type recordedInput struct {
	strings.Builder
	closed bool
}

func (w *recordedInput) Close() error { w.closed = true; return nil }

func TestPTYStaticInputValidationPrecedesStdin(t *testing.T) {
	input := &recordedInput{}
	entry := &session{id: "pty-validation", terminal: ptyWriter{}, stdin: input, stdout: newStreamBuffer(100), stderr: newStreamBuffer(100), done: make(chan struct{})}
	manager := &Manager{ctx: context.Background(), sessions: map[string]*session{entry.id: entry}}
	for _, options := range []ContinueOptions{
		{Stdin: "unwanted", CloseStdin: true},
		{Stdin: "unwanted", Columns: 80},
		{Stdin: "unwanted", Columns: 1001, Rows: 25},
	} {
		options.SessionID = entry.id
		if _, err := manager.Continue(context.Background(), options); err == nil {
			t.Fatalf("accepted invalid PTY options: %+v", options)
		}
		if input.Len() != 0 || input.closed {
			t.Fatalf("invalid PTY input caused side effects: %+v input=%q closed=%v", options, input.String(), input.closed)
		}
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
