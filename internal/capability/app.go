package capability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// appCommand is private to the owner-authored capability definition.
type appCommand struct {
	Executable  string            `json:"executable"`
	Args        []string          `json:"args"`
	Environment map[string]string `json:"environment"`
}

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

func (a *appCommand) validate(input json.RawMessage) error {
	if !isLocalExecutable(a.Executable) {
		return errors.New("local app tools require an absolute executable path")
	}
	if len(a.Args) > 32 || len(strings.Join(a.Args, "")) > 4096 {
		return errors.New("local app command arguments are too large")
	}
	for _, arg := range a.Args {
		if strings.ContainsRune(arg, 0) {
			return errors.New("local app command arguments cannot contain a null character")
		}
	}
	if len(a.Environment) > 16 {
		return errors.New("local app command environment is too large")
	}
	for key, value := range a.Environment {
		if !environmentName.MatchString(key) || strings.HasPrefix(strings.ToUpper(key), "LOCAL_RUNTIME_MCP_") ||
			len(value) > 4096 || strings.ContainsRune(value, 0) {
			return errors.New("invalid local app command environment")
		}
	}
	var schema struct {
		Type                 string                     `json:"type"`
		Properties           map[string]json.RawMessage `json:"properties"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&schema) != nil || schema.Type != "object" || len(schema.Properties) != 0 ||
		schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		return errors.New("local app inputSchema must describe an empty object")
	}
	return nil
}

func isLocalExecutable(path string) bool {
	return !strings.ContainsRune(path, 0) && filepath.IsAbs(path) &&
		(runtime.GOOS != "windows" || len(filepath.VolumeName(path)) == 2 && strings.EqualFold(filepath.Ext(path), ".exe"))
}

// CallApp executes only the fixed local binding. No invocation parameters reach
// the command, and only the validated JSON result is returned.
func (r *Registry) CallApp(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	if r == nil {
		return nil, errors.New("local app capability is not registered")
	}
	r.mu.RLock()
	e, ok := r.entries[name]
	r.mu.RUnlock()
	if !ok || e.command == nil {
		return nil, errors.New("local app capability is not registered")
	}
	if args == nil || len(args) != 0 || e.input.Validate(args) != nil {
		return nil, errors.New("local app arguments must be an empty object")
	}
	path, err := filepath.EvalSymlinks(e.command.Executable)
	if err != nil || !isLocalExecutable(path) {
		return nil, errors.New("local app executable is unavailable")
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("local app executable is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, e.command.Args...)
	configureCommand(command)
	command.Dir = filepath.Dir(path)
	command.WaitDelay = 250 * time.Millisecond
	command.Stderr = io.Discard
	command.Env = []string{}
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SystemRoot", "WINDIR", "TMP", "TEMP", "LANG"} {
		if value, ok := os.LookupEnv(key); ok {
			command.Env = append(command.Env, key+"="+value)
		}
	}
	keys := make([]string, 0, len(e.command.Environment))
	for key := range e.command.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		command.Env = append(command.Env, key+"="+e.command.Environment[key])
	}
	var stdout appOutput
	command.Stdout = &stdout
	err = command.Run()
	if stdout.exceeded {
		return nil, errors.New("local app output exceeded its size limit")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, errors.New("local app command failed")
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.buffer.Bytes(), &output); err != nil || output == nil || e.output.Validate(output) != nil {
		return nil, errors.New("local app result does not match outputSchema")
	}
	return output, nil
}

type appOutput struct {
	buffer   bytes.Buffer
	exceeded bool
}

func (b *appOutput) Write(p []byte) (int, error) {
	if len(p) > (64<<10)-b.buffer.Len() {
		b.exceeded = true
		return 0, errors.New("local app output exceeded its size limit")
	}
	return b.buffer.Write(p)
}
