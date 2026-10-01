package process

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	pty "github.com/aymanbagabas/go-pty"
)

const (
	defaultProcessTimeout = 300
	defaultOutputBytes    = 1 << 20
	maxOutputBytes        = 16 << 20
	defaultYieldMS        = 10_000
	defaultContinueMS     = 1_000
	completedRetention    = 10 * time.Minute
	maxSessions           = 128
	maxInputWaiters       = 32
)

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Options struct {
	Program        string            `json:"program"`
	Args           []string          `json:"args,omitempty"`
	Directory      string            `json:"directory,omitempty"`
	Environment    map[string]string `json:"environment,omitempty"`
	Stdin          string            `json:"stdin,omitempty"`
	KeepStdinOpen  bool              `json:"keep_stdin_open,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	MaxOutputBytes int               `json:"max_output_bytes,omitempty"`
	YieldTimeMS    int               `json:"yield_time_ms,omitempty"`
	IOMode         string            `json:"io_mode,omitempty"`
	Columns        int               `json:"columns,omitempty"`
	Rows           int               `json:"rows,omitempty"`
}

type ContinueOptions struct {
	SessionID   string `json:"session_id"`
	Stdin       string `json:"stdin,omitempty"`
	CloseStdin  bool   `json:"close_stdin,omitempty"`
	Terminate   bool   `json:"terminate,omitempty"`
	YieldTimeMS int    `json:"yield_time_ms,omitempty"`
	Columns     int    `json:"columns,omitempty"`
	Rows        int    `json:"rows,omitempty"`
}

type Result struct {
	SessionID       string   `json:"session_id,omitempty"`
	Running         bool     `json:"running"`
	Program         string   `json:"program"`
	Args            []string `json:"args,omitempty"`
	Directory       string   `json:"directory"`
	ExitCode        int      `json:"exit_code"`
	Stdout          string   `json:"stdout"`
	Stderr          string   `json:"stderr"`
	StdoutTruncated bool     `json:"stdout_truncated"`
	StderrTruncated bool     `json:"stderr_truncated"`
	DurationMS      int64    `json:"duration_ms"`
	TimedOut        bool     `json:"timed_out"`
	IOMode          string   `json:"io_mode"`
}

type processControl interface {
	Kill() error
	Close() error
}

type Manager struct {
	ctx           context.Context
	cancel        context.CancelFunc
	mu            sync.Mutex
	sessions      map[string]*session
	closed        bool
	startingCount int
	starting      sync.WaitGroup
	workers       sync.WaitGroup
	closeOnce     sync.Once
}

type session struct {
	id          string
	program     string
	args        []string
	directory   string
	waitProcess func() error
	control     processControl
	stdin       io.WriteCloser
	terminal    pty.Pty
	ioMode      string
	outputDone  chan struct{}
	stdout      *streamBuffer
	stderr      *streamBuffer
	started     time.Time
	done        chan struct{}

	mu             sync.Mutex
	finished       time.Time
	exitCode       int
	timedOut       bool
	stdinDone      bool
	inputGate      chan struct{}
	inputWaiters   int
	resultMu       sync.Mutex
	controlMu      sync.Mutex
	inputWorkers   sync.WaitGroup
	terminalMu     sync.Mutex
	terminalClosed bool
}

type ptyWriter struct{ pty.Pty }

func (p ptyWriter) Close() error {
	return errors.New("PTY input cannot be half-closed; terminate the session instead")
}

func NewManager(ctx context.Context) *Manager {
	managedContext, cancel := context.WithCancel(ctx)
	manager := &Manager{ctx: managedContext, cancel: cancel, sessions: make(map[string]*session)}
	go func() {
		<-managedContext.Done()
		_ = manager.Close()
	}()
	return manager
}

func (m *Manager) Run(callContext context.Context, options Options) (Result, error) {
	if err := callContext.Err(); err != nil {
		return Result{}, err
	}
	m.mu.Lock()
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return Result{}, errors.New("process manager is closed")
	}
	if len(m.sessions)+m.startingCount >= maxSessions {
		m.mu.Unlock()
		return Result{}, errors.New("process session capacity is full")
	}
	m.starting.Add(1)
	m.startingCount++
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.startingCount--; m.mu.Unlock(); m.starting.Done() }()
	directory, timeout, outputLimit, yield, err := validateOptions(options)
	if err != nil {
		return Result{}, err
	}
	environment := mergeEnvironment(options.Environment)
	program, err := resolveProgram(options.Program, directory, environment)
	if err != nil {
		return Result{}, fmt.Errorf("resolve program %q: %w", options.Program, err)
	}

	entry := &session{
		id: newSessionID(), program: options.Program, args: append([]string(nil), options.Args...), directory: filepath.Clean(directory),
		stdout: newStreamBuffer(outputLimit), stderr: newStreamBuffer(outputLimit), done: make(chan struct{}), exitCode: -1, inputGate: make(chan struct{}, 1),
	}
	mode := options.IOMode
	if mode == "" {
		mode = "pipe"
	}
	entry.ioMode = mode
	if mode == "pty" {
		if options.Columns == 0 {
			options.Columns = 80
		}
		if options.Rows == 0 {
			options.Rows = 25
		}
		terminal, createErr := pty.New()
		if createErr != nil {
			return Result{}, fmt.Errorf("create pseudo-terminal: %w", createErr)
		}
		entry.terminal = terminal
		entry.outputDone = make(chan struct{})
		if err := terminal.Resize(options.Columns, options.Rows); err != nil {
			_ = terminal.Close()
			return Result{}, fmt.Errorf("resize pseudo-terminal: %w", err)
		}
		command := terminal.Command(program, options.Args...)
		command.Dir, command.Env = directory, environment
		preparePTY(command)
		entry.started = time.Now()
		if err := command.Start(); err != nil {
			_ = terminal.Close()
			return Result{}, fmt.Errorf("start PTY process: %w", err)
		}
		entry.waitProcess = command.Wait
		entry.control, err = attachManaged(command.Process)
		if err != nil {
			_ = command.Process.Kill()
			_ = terminal.Close()
			_ = command.Wait()
			return Result{}, fmt.Errorf("manage PTY process: %w", err)
		}
		entry.stdin = ptyWriter{terminal}
		go func() { _, _ = io.Copy(entry.stdout, terminal); close(entry.outputDone) }()
	} else {
		command := exec.Command(program, options.Args...)
		command.Dir, command.Env = directory, environment
		command.WaitDelay = 2 * time.Second
		command.Stdout, command.Stderr = entry.stdout, entry.stderr
		if options.Stdin != "" || options.KeepStdinOpen {
			entry.stdin, err = command.StdinPipe()
			if err != nil {
				return Result{}, fmt.Errorf("create process stdin: %w", err)
			}
		}
		entry.started = time.Now()
		entry.control, err = startManaged(command)
		if err != nil {
			if entry.stdin != nil {
				_ = entry.stdin.Close()
			}
			if command.Process != nil {
				_ = command.Wait()
			}
			return Result{}, fmt.Errorf("start process: %w", err)
		}
		entry.waitProcess = command.Wait
	}

	m.mu.Lock()
	m.sessions[entry.id] = entry
	m.workers.Add(3)
	m.mu.Unlock()
	go func() {
		defer m.workers.Done()
		entry.wait()
		timer := time.NewTimer(completedRetention)
		defer timer.Stop()
		select {
		case <-timer.C:
			m.remove(entry.id)
		case <-m.ctx.Done():
			m.remove(entry.id)
		}
	}()
	go func() { defer m.workers.Done(); entry.watch(m.ctx, time.Duration(timeout)*time.Second) }()
	// Cancellation must also reach a blocked initial stdin write.
	initialDone := make(chan struct{})
	go func() {
		defer m.workers.Done()
		select {
		case <-callContext.Done():
			entry.terminate(false)
		case <-m.ctx.Done():
			entry.terminate(false)
		case <-initialDone:
		}
	}()
	defer close(initialDone)

	if entry.stdin != nil && options.Stdin != "" {
		if err := entry.writeStdin(callContext, options.Stdin); err != nil {
			entry.terminate(false)
			<-entry.done
			m.remove(entry.id)
			if callContext.Err() != nil {
				return Result{}, callContext.Err()
			}
			return Result{}, fmt.Errorf("write process stdin: %w", err)
		}
	}
	if entry.stdin != nil && !options.KeepStdinOpen && mode == "pipe" {
		_ = entry.closeStdin()
	}

	timer := time.NewTimer(time.Duration(yield) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-entry.done:
		m.remove(entry.id)
		if err := callContext.Err(); err != nil {
			return Result{}, err
		}
		if err := m.ctx.Err(); err != nil {
			return Result{}, err
		}
		return entry.result(false), nil
	case <-timer.C:
		if err := callContext.Err(); err != nil {
			entry.terminate(false)
			<-entry.done
			m.remove(entry.id)
			return Result{}, err
		}
		if err := m.ctx.Err(); err != nil {
			entry.terminate(false)
			<-entry.done
			m.remove(entry.id)
			return Result{}, err
		}
		return entry.result(true), nil
	case <-callContext.Done():
		entry.terminate(false)
		<-entry.done
		m.remove(entry.id)
		return Result{}, callContext.Err()
	case <-m.ctx.Done():
		entry.terminate(false)
		<-entry.done
		m.remove(entry.id)
		return Result{}, m.ctx.Err()
	}
}

func (m *Manager) Continue(callContext context.Context, options ContinueOptions) (Result, error) {
	if err := callContext.Err(); err != nil {
		return Result{}, err
	}
	if err := m.ctx.Err(); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(options.SessionID) == "" {
		return Result{}, errors.New("session_id cannot be empty")
	}
	yield := options.YieldTimeMS
	if yield == 0 {
		yield = defaultContinueMS
	}
	if yield < 1 || yield > 60_000 {
		return Result{}, errors.New("yield_time_ms must be between 1 and 60000")
	}
	m.mu.Lock()
	entry := m.sessions[options.SessionID]
	m.mu.Unlock()
	if entry == nil {
		return Result{}, errors.New("process session was not found or has already been collected")
	}
	if options.Terminate && (options.Stdin != "" || options.CloseStdin || options.Columns != 0 || options.Rows != 0) {
		return Result{}, errors.New("terminate cannot be combined with input or resize")
	}
	if options.Stdin != "" {
		if err := entry.writeStdin(callContext, options.Stdin); err != nil {
			return Result{}, err
		}
	}
	if options.CloseStdin {
		if err := entry.closeStdin(); err != nil {
			return Result{}, err
		}
	}
	if options.Columns != 0 || options.Rows != 0 {
		if entry.terminal == nil {
			return Result{}, errors.New("columns and rows are only valid for PTY sessions")
		}
		if options.Columns < 1 || options.Columns > 1000 || options.Rows < 1 || options.Rows > 1000 {
			return Result{}, errors.New("columns and rows must both be between 1 and 1000")
		}
		entry.terminalMu.Lock()
		if entry.terminalClosed {
			entry.terminalMu.Unlock()
			return Result{}, errors.New("PTY session has exited")
		}
		err := entry.terminal.Resize(options.Columns, options.Rows)
		entry.terminalMu.Unlock()
		if err != nil {
			return Result{}, fmt.Errorf("resize pseudo-terminal: %w", err)
		}
	}
	if options.Terminate {
		entry.terminate(false)
	}

	timer := time.NewTimer(time.Duration(yield) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-entry.done:
		m.remove(entry.id)
		return entry.result(false), nil
	case <-timer.C:
		return entry.result(true), nil
	case <-callContext.Done():
		return Result{}, callContext.Err()
	case <-m.ctx.Done():
		return Result{}, m.ctx.Err()
	}
}

func (m *Manager) remove(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

// Close stops admission, terminates process trees, and joins process/output workers.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.cancel()
		m.mu.Unlock()
		m.starting.Wait()
		m.closeAll()
		m.workers.Wait()
	})
	return nil
}

func (m *Manager) closeAll() {
	m.mu.Lock()
	sessions := make([]*session, 0, len(m.sessions))
	for _, entry := range m.sessions {
		sessions = append(sessions, entry)
	}
	m.mu.Unlock()
	for _, entry := range sessions {
		entry.terminate(false)
	}
	for _, entry := range sessions {
		<-entry.done
	}
}

func (s *session) wait() {
	err := s.waitProcess()
	s.mu.Lock()
	// Freeze the process lifetime before terminal/output cleanup and delayed
	// client collection. While the process is alive, result reports elapsed time.
	s.finished = time.Now()
	if err == nil {
		s.exitCode = 0
	} else {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			s.exitCode = exitError.ExitCode()
		}
	}
	s.mu.Unlock()
	s.controlMu.Lock()
	_ = s.control.Close()
	s.control = nil
	s.controlMu.Unlock()
	_ = s.closeStdin()
	if s.terminal != nil {
		s.terminalMu.Lock()
		s.terminalClosed = true
		finishTerminal(s.terminal, s.outputDone)
		s.terminalMu.Unlock()
		s.mu.Lock()
		s.stdinDone = true
		s.mu.Unlock()
	}
	s.inputWorkers.Wait()
	close(s.done)
}

func (s *session) watch(ctx context.Context, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.done:
	case <-ctx.Done():
		s.terminate(false)
	case <-timer.C:
		s.terminate(true)
	}
}

func (s *session) terminate(timedOut bool) {
	s.mu.Lock()
	if timedOut {
		s.timedOut = true
	}
	s.mu.Unlock()
	// Do not wait for input serialization: closing the handle unblocks a writer.
	_ = s.closeStdin()
	s.controlMu.Lock()
	if s.control != nil {
		_ = s.control.Kill()
	}
	s.controlMu.Unlock()
}

func (s *session) closeStdin() error {
	s.mu.Lock()
	if s.stdin == nil || s.stdinDone {
		s.mu.Unlock()
		return nil
	}
	if s.terminal != nil {
		s.mu.Unlock()
		return errors.New("PTY input cannot be half-closed; terminate the session instead")
	}
	s.stdinDone = true
	stdin := s.stdin
	s.mu.Unlock()
	return stdin.Close()
}

func (s *session) writeStdin(ctx context.Context, text string) error {
	if len(text) > maxOutputBytes {
		return errors.New("stdin exceeds the 16 MiB limit")
	}
	s.mu.Lock()
	if s.inputWaiters >= maxInputWaiters {
		s.mu.Unlock()
		return errors.New("process input queue is full")
	}
	s.inputWaiters++
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.inputWaiters--; s.mu.Unlock() }()
	select {
	case s.inputGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errors.New("process has exited")
	}
	if err := ctx.Err(); err != nil {
		<-s.inputGate
		return err
	}
	s.mu.Lock()
	stdin, closed := s.stdin, s.stdinDone
	if stdin == nil || closed {
		s.mu.Unlock()
		<-s.inputGate
		return errors.New("process stdin is not open; start it with keep_stdin_open")
	}
	s.inputWorkers.Add(1)
	s.mu.Unlock()
	finished := make(chan error, 1)
	go func() {
		defer s.inputWorkers.Done()
		_, err := io.WriteString(stdin, text)
		<-s.inputGate
		finished <- err
	}()
	select {
	case err := <-finished:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errors.New("process has exited")
	}
}

func (s *session) result(running bool) Result {
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	select {
	case <-s.done:
		running = false
	default:
	}
	stdout, stdoutTruncated := s.stdout.Drain()
	stderr, stderrTruncated := s.stderr.Drain()
	s.mu.Lock()
	exitCode, timedOut := s.exitCode, s.timedOut
	finished := s.finished
	if finished.IsZero() {
		finished = time.Now()
	}
	s.mu.Unlock()
	result := Result{
		Running: running, Program: s.program, Args: append([]string(nil), s.args...), Directory: s.directory,
		ExitCode: exitCode, Stdout: stdout, Stderr: stderr, StdoutTruncated: stdoutTruncated,
		StderrTruncated: stderrTruncated, DurationMS: finished.Sub(s.started).Milliseconds(), TimedOut: timedOut, IOMode: s.ioMode,
	}
	if running {
		result.SessionID = s.id
	}
	return result
}

func validateOptions(options Options) (string, int, int, int, error) {
	if strings.TrimSpace(options.Program) == "" {
		return "", 0, 0, 0, errors.New("program cannot be empty")
	}
	if len(options.Stdin) > maxOutputBytes {
		return "", 0, 0, 0, errors.New("stdin exceeds the 16 MiB limit")
	}
	directory := options.Directory
	if directory == "" {
		var err error
		directory, err = os.Getwd()
		if err != nil {
			return "", 0, 0, 0, fmt.Errorf("find current working directory: %w", err)
		}
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("resolve process directory: %w", err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		return "", 0, 0, 0, err
	}
	if !info.IsDir() {
		return "", 0, 0, 0, errors.New("process directory must be a directory")
	}
	timeout := options.TimeoutSeconds
	if timeout == 0 {
		timeout = defaultProcessTimeout
	}
	if timeout < 1 || timeout > 86_400 {
		return "", 0, 0, 0, errors.New("timeout_seconds must be between 1 and 86400")
	}
	outputLimit := options.MaxOutputBytes
	if outputLimit == 0 {
		outputLimit = defaultOutputBytes
	}
	if outputLimit < 1 || outputLimit > maxOutputBytes {
		return "", 0, 0, 0, fmt.Errorf("max_output_bytes must be between 1 and %d", maxOutputBytes)
	}
	yield := options.YieldTimeMS
	if yield == 0 {
		yield = defaultYieldMS
	}
	if yield < 1 || yield > 60_000 {
		return "", 0, 0, 0, errors.New("yield_time_ms must be between 1 and 60000")
	}
	for name := range options.Environment {
		if !environmentName.MatchString(name) {
			return "", 0, 0, 0, fmt.Errorf("invalid environment variable name %q", name)
		}
	}
	if options.IOMode != "" && options.IOMode != "pipe" && options.IOMode != "pty" {
		return "", 0, 0, 0, errors.New("io_mode must be pipe or pty")
	}
	if options.IOMode == "pty" {
		if options.Columns == 0 {
			options.Columns = 80
		}
		if options.Rows == 0 {
			options.Rows = 25
		}
		if options.Columns < 1 || options.Columns > 1000 || options.Rows < 1 || options.Rows > 1000 {
			return "", 0, 0, 0, errors.New("columns and rows must be between 1 and 1000")
		}
	} else if options.Columns != 0 || options.Rows != 0 {
		return "", 0, 0, 0, errors.New("columns and rows are only valid when io_mode is pty")
	}
	return directory, timeout, outputLimit, yield, nil
}

func mergeEnvironment(overrides map[string]string) []string {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		if index := strings.IndexByte(entry, '='); index >= 0 {
			values[environmentKey(entry[:index])] = entry
		}
	}
	for name, value := range overrides {
		values[environmentKey(name)] = name + "=" + value
	}
	result := make([]string, 0, len(values))
	for _, entry := range values {
		result = append(result, entry)
	}
	sort.Strings(result)
	return result
}

func environmentKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

func environmentValue(environment []string, name string) string {
	for _, entry := range environment {
		if index := strings.IndexByte(entry, '='); index >= 0 && environmentKey(entry[:index]) == environmentKey(name) {
			return entry[index+1:]
		}
	}
	return ""
}

func resolveProgram(program, directory string, environment []string) (string, error) {
	if strings.ContainsAny(program, `/\\`) || filepath.IsAbs(program) {
		if !filepath.IsAbs(program) {
			program = filepath.Join(directory, program)
		}
		return findExecutable(program, environment)
	}
	for _, component := range filepath.SplitList(environmentValue(environment, "PATH")) {
		component = strings.Trim(component, `"`)
		if component == "" {
			component = directory
		}
		if !filepath.IsAbs(component) {
			component = filepath.Join(directory, component)
		}
		if resolved, err := findExecutable(filepath.Join(component, program), environment); err == nil {
			return resolved, nil
		}
	}
	return "", exec.ErrNotFound
}

func findExecutable(path string, environment []string) (string, error) {
	candidates := []string{path}
	if runtime.GOOS == "windows" && filepath.Ext(path) == "" {
		extensions := environmentValue(environment, "PATHEXT")
		if extensions == "" {
			extensions = ".COM;.EXE;.BAT;.CMD"
		}
		candidates = nil
		for _, extension := range strings.Split(extensions, ";") {
			if extension != "" {
				candidates = append(candidates, path+extension)
			}
		}
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode()&0o111 != 0) {
			return filepath.Abs(candidate)
		}
	}
	return "", exec.ErrNotFound
}

func newSessionID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

type streamBuffer struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func newStreamBuffer(limit int) *streamBuffer {
	return &streamBuffer{data: make([]byte, 0, min(limit, 64*1024)), limit: limit}
}

func (b *streamBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(data) >= b.limit {
		b.data = append(b.data[:0], data[len(data)-b.limit:]...)
		b.truncated = true
		return len(data), nil
	}
	if overflow := len(b.data) + len(data) - b.limit; overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
		b.truncated = true
	}
	b.data = append(b.data, data...)
	return len(data), nil
}

func (b *streamBuffer) Drain() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, truncated := string(b.data), b.truncated
	b.data = b.data[:0]
	b.truncated = false
	return value, truncated
}

var _ io.Writer = (*streamBuffer)(nil)
