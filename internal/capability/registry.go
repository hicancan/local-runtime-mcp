// Package capability loads owner-authored browser and local app tools from one file tree.
package capability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/hicancan/local-runtime-mcp/internal/browser"
)

// Browser is implemented by the existing browser Bridge. Tests may supply a
// protocol peer without launching a browser or running page JavaScript.
type Browser interface {
	Tabs(context.Context, string) ([]browser.Tab, error)
	Act(context.Context, browser.Action) (browser.ActionResult, error)
}

type Annotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint"`
	DestructiveHint bool `json:"destructiveHint"`
	IdempotentHint  bool `json:"idempotentHint"`
	OpenWorldHint   bool `json:"openWorldHint"`
}

// Definition is the public tool definition. Name comes from the relative file
// path: deepseek/read.json is registered as deepseek.read.
type Definition struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
	Annotations  Annotations     `json:"annotations"`
	Origins      []string        `json:"origins"`
}

type entry struct {
	definition Definition
	script     string
	command    *appCommand
	input      *jsonschema.Resolved
	output     *jsonschema.Resolved
}

type Registry struct {
	entries map[string]entry
	browser Browser
}

var validName = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$`)

// Load reads and validates the complete tree before returning it. An empty
// directory setting disables capabilities; an invalid configured tree fails
// rather than partially publishing a tool catalog. No remote schema is fetched.
func Load(directory string, executor Browser) (*Registry, error) {
	r := &Registry{entries: make(map[string]entry), browser: executor}
	if directory == "" {
		return r, nil
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("capability directory: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, errors.New("capability directory does not exist or is not a directory")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve capability directory: %w", err)
	}
	err = filepath.WalkDir(root, func(path string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if item.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := strings.ReplaceAll(filepath.ToSlash(strings.TrimSuffix(relative, ".json")), "/", ".")
		if filepath.Dir(relative) == "." || len(name) > 128 || !validName.MatchString(name) {
			return fmt.Errorf("capability %s: use a namespace/action file path with a valid tool name", relative)
		}
		if _, exists := r.entries[name]; exists {
			return fmt.Errorf("capability %s: duplicate tool name", relative)
		}
		loaded, err := loadEntry(path, name)
		if err != nil {
			return fmt.Errorf("capability %s: %w", relative, err)
		}
		r.entries[name] = loaded
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

func loadEntry(path, name string) (entry, error) {
	data, err := readRegularFile(path, 256<<10)
	if err != nil {
		return entry{}, fmt.Errorf("read definition: %w", err)
	}
	// Name is derived, not accepted as a second source of identity in the file.
	var file struct {
		Title        string          `json:"title"`
		Description  string          `json:"description"`
		InputSchema  json.RawMessage `json:"inputSchema"`
		OutputSchema json.RawMessage `json:"outputSchema"`
		Annotations  json.RawMessage `json:"annotations"`
		Origins      []string        `json:"origins"`
		Command      *appCommand     `json:"command"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return entry{}, errors.New("invalid definition JSON or unknown definition field")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return entry{}, errors.New("definition must contain one JSON object")
	}
	if strings.TrimSpace(file.Description) == "" {
		return entry{}, errors.New("description is required")
	}
	var declarations map[string]*bool
	if err := json.Unmarshal(file.Annotations, &declarations); err != nil || len(declarations) != 4 {
		return entry{}, errors.New("annotations must explicitly declare all four MCP hints")
	}
	for _, key := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
		if declarations[key] == nil {
			return entry{}, errors.New("annotations must explicitly declare all four MCP hints")
		}
	}
	var annotations Annotations
	_ = json.Unmarshal(file.Annotations, &annotations)
	if annotations.ReadOnlyHint && annotations.DestructiveHint {
		return entry{}, errors.New("a read-only capability cannot declare destructive effects")
	}
	input, err := resolveSchema(file.InputSchema)
	if err != nil {
		return entry{}, fmt.Errorf("invalid inputSchema: %w", err)
	}
	output, err := resolveSchema(file.OutputSchema)
	if err != nil {
		return entry{}, fmt.Errorf("invalid outputSchema: %w", err)
	}
	loaded := entry{
		definition: Definition{Name: name, Title: file.Title, Description: file.Description,
			InputSchema: file.InputSchema, OutputSchema: file.OutputSchema, Annotations: annotations},
		input: input, output: output,
	}
	scriptPath := strings.TrimSuffix(path, ".json") + ".js"
	if file.Command != nil {
		if len(file.Origins) > 0 || !annotations.ReadOnlyHint || annotations.DestructiveHint || annotations.OpenWorldHint {
			return entry{}, errors.New("local app tools require a read-only, closed-world declaration without browser origins")
		}
		if _, err := os.Lstat(scriptPath); !errors.Is(err, os.ErrNotExist) {
			return entry{}, errors.New("local app definitions cannot also supply a browser script")
		}
		if err := file.Command.validate(file.InputSchema); err != nil {
			return entry{}, err
		}
		loaded.command = file.Command
		return loaded, nil
	}
	if len(file.Origins) == 0 {
		return entry{}, errors.New("at least one allowed target origin is required")
	}
	origins := make([]string, 0, len(file.Origins))
	for _, raw := range file.Origins {
		u, err := url.Parse(raw)
		if err != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
			return entry{}, errors.New("origins must be exact HTTP(S) origins without paths, queries or fragments")
		}
		origin, err := originOf(raw)
		if err != nil {
			return entry{}, errors.New("origins must be exact HTTP(S) origins")
		}
		if !contains(origins, origin) {
			origins = append(origins, origin)
		}
	}
	script, err := readRegularFile(scriptPath, 1<<20)
	if err != nil || strings.TrimSpace(string(script)) == "" {
		return entry{}, errors.New("a nonempty same-named JavaScript implementation is required")
	}
	loaded.definition.Origins = origins
	loaded.script = string(script)
	return loaded, nil
}

func resolveSchema(data []byte) (*jsonschema.Resolved, error) {
	var schema jsonschema.Schema
	if err := json.Unmarshal(data, &schema); err != nil || schema.Type != "object" {
		return nil, errors.New("schema must have type object")
	}
	return schema.Resolve(nil)
}

func readRegularFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("capability file must be a bounded regular file")
	}
	return os.ReadFile(path)
}

// List returns browser definitions only, including for the web and model routes.
func (r *Registry) List() []Definition {
	return r.definitions(false)
}

// AppDefinitions returns tools for owner-started local stdio sessions.
func (r *Registry) AppDefinitions() []Definition {
	return r.definitions(true)
}

func (r *Registry) definitions(apps bool) []Definition {
	result := make([]Definition, 0)
	if r == nil {
		return result
	}
	for _, e := range r.entries {
		if (e.command != nil) != apps {
			continue
		}
		d := e.definition
		d.InputSchema = append(json.RawMessage(nil), d.InputSchema...)
		d.OutputSchema = append(json.RawMessage(nil), d.OutputSchema...)
		d.Origins = append([]string(nil), d.Origins...)
		result = append(result, d)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// Call validates parameters and the selected target before using the existing
// short browser evaluate operation. Scripts are trusted owner code, not a sandbox.
func (r *Registry) Call(ctx context.Context, name, browserID, tabID string, args map[string]any) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errors.New("capability is not registered")
	}
	e, ok := r.entries[name]
	if !ok || e.command != nil {
		return nil, errors.New("capability is not registered")
	}
	if r.browser == nil || browserID == "" || tabID == "" {
		return nil, errors.New("a browser instance and tab are required")
	}
	if args == nil {
		args = map[string]any{}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, errors.New("capability arguments must be JSON values")
	}
	var input map[string]any
	if err := json.Unmarshal(encoded, &input); err != nil || e.input.Validate(input) != nil {
		return nil, errors.New("capability arguments do not match inputSchema")
	}
	tabs, err := r.browser.Tabs(ctx, browserID)
	if err != nil {
		return nil, err
	}
	found := false
	for _, tab := range tabs {
		if tab.ID != tabID {
			continue
		}
		if tab.BrowserID != "" && tab.BrowserID != browserID {
			return nil, errors.New("capability tab belongs to another browser instance")
		}
		origin, err := originOf(tab.URL)
		if err != nil || !contains(e.definition.Origins, origin) {
			return nil, errors.New("target origin is not allowed for capability")
		}
		found = true
		break
	}
	if !found {
		return nil, errors.New("capability tab is not present in the selected browser instance")
	}
	origins, _ := json.Marshal(e.definition.Origins)
	// A navigation between tab discovery and execution must not run the script
	// on an unintended origin. Arguments are JSON literals, never script source.
	script := fmt.Sprintf("(async () => { if (!%s.includes(location.origin)) throw new Error('capability target origin changed'); const execute = %s\n; if (typeof execute !== 'function') throw new Error('capability implementation must be a function'); return await execute(%s); })()", origins, e.script, encoded)
	result, err := r.browser.Act(ctx, browser.Action{TabID: tabID, Kind: "evaluate", Script: script})
	if err != nil {
		return nil, err
	}
	if !result.Success {
		return nil, errors.New("capability browser execution failed")
	}
	data, err := json.Marshal(result.Value)
	if err != nil {
		return nil, errors.New("capability result is not JSON")
	}
	var output map[string]any
	if err := json.Unmarshal(data, &output); err != nil || e.output.Validate(output) != nil {
		return nil, errors.New("capability result does not match outputSchema")
	}
	return output, nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func originOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Hostname() == "" || u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("invalid target URL")
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if strings.ContainsAny(host, "* \\\t\r\n") {
		return "", errors.New("invalid target host")
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 0 || number > 65535 {
			return "", errors.New("invalid target port")
		}
	}
	if u.Scheme == "http" && port == "80" || u.Scheme == "https" && port == "443" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return u.Scheme + "://" + host, nil
}
