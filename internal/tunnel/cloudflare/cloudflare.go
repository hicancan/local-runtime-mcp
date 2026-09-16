// Package cloudflare runs the cloudflared companion for a managed tunnel.
package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type Config struct {
	Binary                    string
	Token                     string
	SensitiveEnvironmentNames []string
	Stdout                    io.Writer
	Stderr                    io.Writer
}

func Run(ctx context.Context, cfg Config) error {
	binary, err := FindBinary(cfg.Binary)
	if err != nil {
		return err
	}
	tokenFile, cleanup, err := prepareTokenFile(cfg.Token)
	if err != nil {
		return err
	}
	defer cleanup()

	command := exec.CommandContext(ctx, binary, Arguments(tokenFile)...)
	command.Env = sanitizedEnvironment(os.Environ(), cfg.SensitiveEnvironmentNames)
	command.Stdout = cfg.Stdout
	command.Stderr = cfg.Stderr
	configureCommand(command)
	if err := command.Run(); err != nil && !errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("cloudflared: %w", err)
	}
	return nil
}

func Arguments(tokenFile string) []string {
	return []string{"tunnel", "--no-autoupdate", "--loglevel", "error", "run", "--token-file", tokenFile}
}

func sanitizedEnvironment(values, explicit []string) []string {
	remove := make(map[string]struct{}, len(explicit))
	for _, name := range explicit {
		remove[strings.ToUpper(strings.TrimSpace(name))] = struct{}{}
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		name, _, ok := strings.Cut(value, "=")
		upper := strings.ToUpper(name)
		_, explicitlyRemoved := remove[upper]
		if !ok || explicitlyRemoved || strings.HasPrefix(upper, "LOCAL_RUNTIME_MCP_") || sensitiveEnvironmentName(upper) {
			continue
		}
		result = append(result, value)
	}
	return result
}

func sensitiveEnvironmentName(name string) bool {
	for _, suffix := range []string{"_TOKEN", "_TOKEN_FILE", "_API_KEY", "_SECRET", "_PASSWORD"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func FindBinary(explicit string) (string, error) {
	name := "cloudflared"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if explicit != "" {
		resolved, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}
		if info, err := os.Stat(resolved); err != nil || info.IsDir() {
			return "", fmt.Errorf("cloudflared binary %q is not a regular file", resolved)
		}
		return resolved, nil
	}
	if executable, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(executable), name)
		if info, err := os.Stat(sibling); err == nil && !info.IsDir() {
			return sibling, nil
		}
	}
	resolved, err := exec.LookPath(name)
	if err != nil {
		return "", errors.New("cloudflared companion was not found beside lrmcp or on PATH")
	}
	return resolved, nil
}

func prepareTokenFile(token string) (string, func(), error) {
	if strings.TrimSpace(token) == "" {
		return "", func() {}, errors.New("Cloudflare tunnel token is required")
	}
	directory, err := os.MkdirTemp("", "lrmcp-cloudflare-token-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	if err := os.Chmod(directory, 0o700); err != nil {
		cleanup()
		return "", func() {}, err
	}
	file := filepath.Join(directory, "token")
	if err := os.WriteFile(file, []byte(strings.TrimSpace(token)), 0o600); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return file, cleanup, nil
}
