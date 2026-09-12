package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const defaultTimeout = 300 * time.Second

func (m *Manager) Exec(ctx context.Context, name, command string, args []string, env map[string]string, timeoutSeconds int) (CommandResult, error) {
	root, err := m.Root(name)
	if err != nil {
		return CommandResult{}, err
	}
	if strings.TrimSpace(command) == "" {
		return CommandResult{}, errors.New("command cannot be empty")
	}
	timeout := defaultTimeout
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, command, args...)
	cmd.Dir = root
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	started := time.Now()
	runErr := cmd.Run()
	result := CommandResult{
		Command: commandLine(command, args),
		Stdout:  stdout.String(), Stderr: stderr.String(),
		DurationMS: time.Since(started).Milliseconds(),
	}
	if runErr == nil {
		return result, nil
	}
	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		result.ExitCode = -1
		result.TimedOut = true
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, fmt.Errorf("start command: %w", runErr)
}

func (m *Manager) Git(ctx context.Context, name string, args ...string) (CommandResult, error) {
	return m.Exec(ctx, name, "git", args, map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_PAGER":           "cat",
	}, 300)
}

func commandLine(command string, args []string) string {
	parts := append([]string{command}, args...)
	if runtime.GOOS == "windows" {
		return strings.Join(parts, " ")
	}
	return strings.Join(parts, " ")
}
