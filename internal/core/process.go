package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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

func (r *Runtime) RunProcess(ctx context.Context, rootName string, options ProcessOptions) (ProcessResult, error) {
	if strings.TrimSpace(options.Program) == "" {
		return ProcessResult{}, errors.New("program cannot be empty")
	}
	directory, err := r.Resolve(rootName, options.Directory)
	if err != nil {
		return ProcessResult{}, err
	}
	info, err := os.Stat(directory)
	if err != nil {
		return ProcessResult{}, err
	}
	if !info.IsDir() {
		return ProcessResult{}, errors.New("process directory must be a directory")
	}
	timeout := options.TimeoutSeconds
	if timeout == 0 {
		timeout = defaultProcessTimeout
	}
	if timeout < 1 || timeout > 86_400 {
		return ProcessResult{}, errors.New("timeout_seconds must be between 1 and 86400")
	}
	outputLimit := options.MaxOutputBytes
	if outputLimit == 0 {
		outputLimit = defaultOutputBytes
	}
	if outputLimit < 1 || outputLimit > maxOutputBytes {
		return ProcessResult{}, fmt.Errorf("max_output_bytes must be between 1 and %d", maxOutputBytes)
	}
	for name := range options.Environment {
		if !environmentName.MatchString(name) {
			return ProcessResult{}, fmt.Errorf("invalid environment variable name %q", name)
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
	duration := time.Since(started).Milliseconds()
	result := ProcessResult{
		Program: options.Program, Args: options.Args, Root: rootName,
		Directory: options.Directory, Stdout: stdout.String(), Stderr: stderr.String(),
		StdoutTruncated: stdout.Truncated(), StderrTruncated: stderr.Truncated(), DurationMS: duration,
	}
	if result.Directory == "" {
		result.Directory = "."
	}
	if processContext.Err() == context.DeadlineExceeded {
		result.ExitCode = -1
		result.TimedOut = true
		return result, nil
	}
	if ctx.Err() != nil {
		result.ExitCode = -1
		return result, ctx.Err()
	}
	if runErr == nil {
		result.ExitCode = 0
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
