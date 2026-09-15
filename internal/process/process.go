package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	defaultProcessTimeout = 300
	defaultOutputBytes    = 1 << 20
	maxOutputBytes        = 16 << 20
)

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Options struct {
	Program        string            `json:"program"`
	Args           []string          `json:"args,omitempty"`
	Directory      string            `json:"directory,omitempty"`
	Environment    map[string]string `json:"environment,omitempty"`
	Stdin          string            `json:"stdin,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	MaxOutputBytes int               `json:"max_output_bytes,omitempty"`
}

type Result struct {
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
}

func Run(ctx context.Context, options Options) (Result, error) {
	if strings.TrimSpace(options.Program) == "" {
		return Result{}, errors.New("program cannot be empty")
	}
	directory := options.Directory
	if directory == "" {
		var err error
		directory, err = os.Getwd()
		if err != nil {
			return Result{}, fmt.Errorf("find current working directory: %w", err)
		}
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return Result{}, fmt.Errorf("resolve process directory: %w", err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		return Result{}, err
	}
	if !info.IsDir() {
		return Result{}, errors.New("process directory must be a directory")
	}
	timeout := options.TimeoutSeconds
	if timeout == 0 {
		timeout = defaultProcessTimeout
	}
	if timeout < 1 || timeout > 86_400 {
		return Result{}, errors.New("timeout_seconds must be between 1 and 86400")
	}
	outputLimit := options.MaxOutputBytes
	if outputLimit == 0 {
		outputLimit = defaultOutputBytes
	}
	if outputLimit < 1 || outputLimit > maxOutputBytes {
		return Result{}, fmt.Errorf("max_output_bytes must be between 1 and %d", maxOutputBytes)
	}
	for name := range options.Environment {
		if !environmentName.MatchString(name) {
			return Result{}, fmt.Errorf("invalid environment variable name %q", name)
		}
	}

	processContext, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	command := exec.CommandContext(processContext, options.Program, options.Args...)
	command.Dir = directory
	command.Env = append([]string{}, os.Environ()...)
	for name, value := range options.Environment {
		command.Env = append(command.Env, name+"="+value)
	}
	command.Stdin = strings.NewReader(options.Stdin)
	stdout := newLimitedBuffer(outputLimit)
	stderr := newLimitedBuffer(outputLimit)
	command.Stdout = stdout
	command.Stderr = stderr

	started := time.Now()
	runErr := command.Run()
	result := Result{
		Program: options.Program, Args: options.Args, Directory: filepath.Clean(directory),
		Stdout: stdout.String(), Stderr: stderr.String(), StdoutTruncated: stdout.Truncated(),
		StderrTruncated: stderr.Truncated(), DurationMS: time.Since(started).Milliseconds(),
	}
	if processContext.Err() == context.DeadlineExceeded {
		result.ExitCode, result.TimedOut = -1, true
		return result, nil
	}
	if ctx.Err() != nil {
		result.ExitCode = -1
		return result, ctx.Err()
	}
	if runErr == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, fmt.Errorf("start process: %w", runErr)
}

type limitedBuffer struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func newLimitedBuffer(limit int) *limitedBuffer {
	return &limitedBuffer{data: make([]byte, 0, min(limit, 64*1024)), limit: limit}
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - len(b.data)
	if remaining > 0 {
		amount := min(len(data), remaining)
		b.data = append(b.data, data[:amount]...)
	}
	if len(data) > remaining {
		b.truncated = true
	}
	return len(data), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

func (b *limitedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

var _ io.Writer = (*limitedBuffer)(nil)
