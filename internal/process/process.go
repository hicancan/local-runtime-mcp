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
	ctx      context.Context
	mu       sync.Mutex
	sessions map[string]*session
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

	mu        sync.Mutex
	exitCode  int
	timedOut  bool
	stdinDone bool
}

type ptyWriter struct{ pty.Pty }

func (p ptyWriter) Close() error {
	return errors.New("PTY input cannot be half-closed; terminate the session instead")
}

func NewManager(ctx context.Context) *Manager {
	manager := &Manager{ctx: ctx, sessions: make(map[string]*session)}
	go func() {
		<-ctx.Done()
		manager.closeAll()
	}()
	return manager
}

func (m *Manager) Run(callContext context.Context, options Options) (Result, error) {
	directory, timeout, outputLimit, yield, err := validateOptions(options)
	if err != nil {
		return Result{}, err
	}

	entry := &session{
		id: newSessionID(), program: options.Program, args: append([]string(nil), options.Args...), directory: filepath.Clean(directory),
		stdout: newStreamBuffer(outputLimit), stderr: newStreamBuffer(outputLimit), started: time.Now(), done: make(chan struct{}), exitCode: -1,
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
		command := terminal.Command(options.Program, options.Args...)
		command.Dir, command.Env = directory, mergeEnvironment(options.Environment)
		preparePTY(command)
		if err := command.Start(); err != nil {
			_ = terminal.Close()
			return Result{}, fmt.Errorf("start PTY process: %w", err)
		}
		entry.waitProcess = command.Wait
		entry.control, err = attachManaged(command.Process)
		if err != nil {
			_ = command.Process.Kill()
			_ = terminal.Close()
			return Result{}, fmt.Errorf("manage PTY process: %w", err)
		}
		entry.stdin = ptyWriter{terminal}
		go func() { _, _ = io.Copy(entry.stdout, terminal); close(entry.outputDone) }()
	} else {
		command := exec.Command(options.Program, options.Args...)
		command.Dir, command.Env = directory, mergeEnvironment(options.Environment)
		command.Stdout, command.Stderr = entry.stdout, entry.stderr
		if options.Stdin != "" || options.KeepStdinOpen {
			entry.stdin, err = command.StdinPipe()
			if err != nil {
				return Result{}, fmt.Errorf("create process stdin: %w", err)
			}
		}
		entry.control, err = startManaged(command)
		if err != nil {
			return Result{}, fmt.Errorf("start process: %w", err)
		}
		entry.waitProcess = command.Wait
	}

	m.mu.Lock()
	m.sessions[entry.id] = entry
	m.mu.Unlock()
	go func() {
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
	go entry.watch(m.ctx, time.Duration(timeout)*time.Second)

	if entry.stdin != nil && options.Stdin != "" {
		if _, err := io.WriteString(entry.stdin, options.Stdin); err != nil {
			entry.terminate(false)
			<-entry.done
			m.remove(entry.id)
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
		return entry.result(false), nil
	case <-timer.C:
		return entry.result(true), nil
	case <-callContext.Done():
		entry.terminate(false)
		<-entry.done
		m.remove(entry.id)
		return Result{}, callContext.Err()
	}
}

func (m *Manager) Continue(callContext context.Context, options ContinueOptions) (Result, error) {
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
	if options.Stdin != "" {
		entry.mu.Lock()
		stdin, closed := entry.stdin, entry.stdinDone
		if stdin == nil || closed {
			entry.mu.Unlock()
			return Result{}, errors.New("process stdin is not open; start it with keep_stdin_open")
		}
		_, writeErr := io.WriteString(stdin, options.Stdin)
		entry.mu.Unlock()
		if writeErr != nil {
			return Result{}, fmt.Errorf("write process stdin: %w", writeErr)
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
		if err := entry.terminal.Resize(options.Columns, options.Rows); err != nil {
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
	}
}

func (m *Manager) remove(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
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
}

func (s *session) wait() {
	err := s.waitProcess()
	s.mu.Lock()
	if err == nil {
		s.exitCode = 0
	} else {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			s.exitCode = exitError.ExitCode()
		}
	}
	s.mu.Unlock()
	_ = s.control.Close()
	if s.terminal != nil {
		_ = s.terminal.Close()
		<-s.outputDone
	}
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
	_ = s.control.Kill()
}

func (s *session) closeStdin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stdin == nil || s.stdinDone {
		return nil
	}
	s.stdinDone = true
	return s.stdin.Close()
}

func (s *session) result(running bool) Result {
	stdout, stdoutTruncated := s.stdout.Drain()
	stderr, stderrTruncated := s.stderr.Drain()
	s.mu.Lock()
	exitCode, timedOut := s.exitCode, s.timedOut
	s.mu.Unlock()
	result := Result{
		Running: running, Program: s.program, Args: append([]string(nil), s.args...), Directory: s.directory,
		ExitCode: exitCode, Stdout: stdout, Stderr: stderr, StdoutTruncated: stdoutTruncated,
		StderrTruncated: stderrTruncated, DurationMS: time.Since(s.started).Milliseconds(), TimedOut: timedOut, IOMode: s.ioMode,
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
			values[strings.ToUpper(entry[:index])] = entry
		}
	}
	for name, value := range overrides {
		values[strings.ToUpper(name)] = name + "=" + value
	}
	result := make([]string, 0, len(values))
	for _, entry := range values {
		result = append(result, entry)
	}
	sort.Strings(result)
	return result
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
