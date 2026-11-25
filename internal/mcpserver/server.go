package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/hicancan/local-runtime-mcp/internal/audit"
	"github.com/hicancan/local-runtime-mcp/internal/browser"
	"github.com/hicancan/local-runtime-mcp/internal/capability"
	"github.com/hicancan/local-runtime-mcp/internal/computer"
	"github.com/hicancan/local-runtime-mcp/internal/filesystem"
	"github.com/hicancan/local-runtime-mcp/internal/jsrt"
	runtimeprocess "github.com/hicancan/local-runtime-mcp/internal/process"
	"github.com/hicancan/local-runtime-mcp/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const Version = version.Version

const instructions = `Local Runtime MCP (lrmcp) exposes this machine via 4 orthogonal primitives:
- run(title, code, ...): stateful async JS runtime with concurrent proc, fs, browser (native browser-level CDP + DevTools + Tracing), desktop, img, caps, emitImage, print, sleep, and help(topic?). See run's description for exact SDK signatures.
- Call run directly for one-off queries. Use session(action='start'...'done') for multi-step tasks or whenever opening temporary browser tabs so 'done' reclaims them. Use wait(cell_id) to continue or cancel a running cell.`

const runToolDescription = `Execute async JavaScript in this session's persistent VM (top-level let/const/var/function persist across turns; bare final expressions auto-return in cell.value; parallelize with await Promise.allSettled([...]); call help(topic?) anytime for full docs). All SDK options use strict snake_case and fail fast on unknown fields.

SDK signatures (all async unless noted):
- proc.run(program, args?, { cwd?, env?, stdin?, keep_stdin_open?, timeout_seconds?, max_output_bytes?, max_output_tokens?, yield_time_ms?, io_mode?, columns?, rows? })
  or proc.run({ cmd, shell?: "pwsh"|"cmd"|"bash"|"sh", cwd?, env?, stdin?, keep_stdin_open?, timeout_seconds?, max_output_bytes?, max_output_tokens?, yield_time_ms?, io_mode?, columns?, rows? })
  -> { session_id?, program, directory, exit_code, running, timed_out, duration_ms, stdout, stderr, output_cursor: { stdout, stderr } }
- proc.continue({ session_id, output_cursor?, stdin?, close_stdin?, terminate?, yield_time_ms?, columns?, rows? }); proc.kill(session_id)
- fs.list(path, { max_depth?, include_hidden?, max_entries? }?) -> { entries: [{ path, type, size?, modified_at }], truncated, skipped }
- fs.stat(path, { with_sha256? }? | bool?) -> { path, type, size, modified_at, mime_type?, symlink_target?, sha256? }
- fs.read(path, { start_line?, end_line?, line_count?, max_bytes?, with_sha256? }?) -> { path, content, size, start_line, end_line, next_line?, truncated, sha256? }
- fs.write(path, content, { mode?: "create"|"replace", expected_sha256?, create_parents? }?) -> { path, bytes, created, sha256 }
- fs.patch(path, [{ before?, old, new, after? }], expected_sha256) -> { path, bytes, hunks, sha256 }
- fs.search(path, query, { regex?, case_sensitive?, include_hidden?, max_results? }?) -> { matches: [{ path, line, column, text }], truncated, skipped }
- browser.status() -> { connected, extension_connected, instances: [{ browser_id, label, state, cdp_url, connected, extension_version }] }
- browser.connect(browser_id?); browser.tabs(browser_id? | { browser_id?, include_workers? }?) -> TabHandle[]
- browser.open(url, { browser_id?, active?, control_id?, session_name?, group? }?) -> TabHandle; browser.tab(id) (sync); browser.cleanup()
- browser.cdp(method, params?, browser_id?); browser.traceStart({ browser_id?, tab_id?, reload?, ignore_cache?, categories? }?); browser.traceStop(file_path? | { file_path?, browser_id? }?, browser_id?)
- TabHandle (.id/.title/.url/.browser_id): .goto(url) | .reload() | .back() | .forward() | .close() | .markHandoff() (sync) | .snapshot({ diff?, max_elements?, max_text? }?) | .screenshot({ full_page?, clip_x?, clip_y?, clip_width?, clip_height? }?) | .eval(jsOrFn, ...args) | .cdp(method, params?) | .getByRole(role, nameOrOpts?)/.getByText/.getByLabel/.getByPlaceholder/.locator(css) -> .click()/.fill(t)/.type(t)/.press(k)/.select(o)/.check(b)/.uploadFiles(paths)/.waitFor({ timeout_ms? })/.nth(i) | .network({ resource_types?, url_contains?, include_preserved?, failed_only?, limit? }?) | .networkBody(req_id) | .console({ levels?, limit? }?) | .emulate({ width?, height?, mobile?, cpu_throttling_rate?, network_preset?, color_scheme?, ... }) | .css(selectorOrLoc) | .heapSnapshot(path?) | .logpoint(url, line1Based, expr, col0Based?) | .screencastStart(opts?) | .screencastStop()
- desktop.targets({ pid?, title?, process? }?) -> { displays, windows: [{ target_id, pid, process, title, bounds, state, active }] }; desktop.state({ target_id?, display_id?, control_id?, image?, elements?: "all"|"interactive"|"none", max_elements?, offset?, max_width?, max_height? }?); desktop.wait({ target_id, name?, role?, automation_id?, until?: "present"|"absent", timeout_ms?, image? }); desktop.action({ kind, control_id?, target_id?, state_id?, element_ref?, ... }); desktop.control("status"|"acquire"|"release", { control_id?, label? }?)
- img.read(path, { max_bytes?, max_pixels?, max_width?, max_height?, crop_x?, crop_y?, crop_width?, crop_height? }?); emitImage(imgOrShotOrPath, label?) (sync)
- caps.list() -> Capability[]; caps.call(name, args?, browser_id?, tab_id?); help(topic?) (sync); print(...); sleep(ms)`

// ToolFilter reports whether a tool with the given name and side-effect hints
// should be visible and callable on this MCP server.
type ToolFilter func(name string, readOnly, destructive bool) bool

// Dependencies are owned by the Runtime Host, never by a client connection.
type Dependencies struct {
	Runtime      *jsrt.Engine
	Process      *runtimeprocess.Manager
	Filesystem   *filesystem.Service
	Browser      *browser.Bridge
	Computer     computer.Controller
	Capabilities *capability.Registry
	Audit        *audit.Store
	Wire         string
	LocalApps    bool
	ReadOnly     bool
	Filter       ToolFilter
}

// Managed wraps an MCP server whose active tool catalog can be dynamically
// resynchronized when owner grants or approved webpage capabilities change.
type Managed struct {
	Server *mcp.Server

	mu          sync.Mutex
	wire        string
	readOnly    bool
	filter      ToolFilter
	registered  map[string]string
	static      []toolSpec
	registry    *capability.Registry
	runtime     *jsrt.Engine
	ownsRuntime bool

	taskMu      sync.RWMutex
	activeTasks map[string]string
	taskOrder   []string
}

type toolSpec struct {
	name        string
	readOnly    bool
	destructive bool
	register    func(*mcp.Server)
}

func New(deps Dependencies) *mcp.Server {
	return NewManaged(deps).Server
}

func NewManaged(deps Dependencies) *Managed {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "local-runtime-mcp", Version: Version},
		&mcp.ServerOptions{Instructions: instructions, Capabilities: &mcp.ServerCapabilities{}},
	)

	engine := deps.Runtime
	ownsRuntime := false
	if engine == nil {
		ownsRuntime = true
		engine = jsrt.NewEngine(context.Background(), jsrt.Dependencies{
			Process:      deps.Process,
			Filesystem:   deps.Filesystem,
			Browser:      deps.Browser,
			Computer:     deps.Computer,
			Capabilities: deps.Capabilities,
			LocalApps:    deps.LocalApps,
		})
	}

	managed := &Managed{
		Server:      server,
		wire:        deps.Wire,
		readOnly:    deps.ReadOnly,
		filter:      deps.Filter,
		registered:  map[string]string{},
		registry:    deps.Capabilities,
		runtime:     engine,
		ownsRuntime: ownsRuntime,
		activeTasks: map[string]string{},
	}

	static := []toolSpec{
		runToolSpec(managed, deps.Wire),
		waitToolSpec(managed, deps.Wire),
		sessionToolSpec(managed, deps.Audit, deps.Wire),
	}
	if deps.Capabilities.Proposes() {
		static = append(static, proposeToolSpec(deps.Capabilities))
	}
	if deps.LocalApps {
		registerLocalAppTools(server, deps.Capabilities)
	}
	managed.static = static

	managed.installMiddleware(deps.Audit, deps.Wire)
	managed.Sync()
	if deps.Capabilities != nil {
		deps.Capabilities.Watch(managed.Sync)
	}
	return managed
}

// SetFilter replaces the active tool filter and immediately reconciles the
// server's published tool catalog, pushing tools/list_changed to connected clients.
func (m *Managed) SetFilter(filter ToolFilter, readOnly bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.filter = filter
	m.readOnly = readOnly
	m.syncLocked()
}

// Sync reconciles registered tools against the current filter and capability catalog.
func (m *Managed) Sync() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncLocked()
}

func (m *Managed) syncLocked() {
	allow := func(name string, readOnly, destructive bool) bool {
		return m.filter == nil || m.filter(name, readOnly, destructive)
	}
	current := map[string]bool{}
	for _, spec := range m.static {
		if !allow(spec.name, spec.readOnly, spec.destructive) {
			continue
		}
		current[spec.name] = true
		if m.registered[spec.name] == "static" {
			continue
		}
		m.registered[spec.name] = "static"
		spec.register(m.Server)
	}
	if m.registry != nil {
		for _, definition := range m.registry.List() {
			if !allow(definition.Name, definition.Annotations.ReadOnlyHint, definition.Annotations.DestructiveHint) {
				continue
			}
			current[definition.Name] = true
			fingerprint, _ := json.Marshal(definition)
			if m.registered[definition.Name] == string(fingerprint) {
				continue
			}
			m.registered[definition.Name] = string(fingerprint)
			addCapabilityTool(m.Server, m.registry, definition)
		}
	}
	var gone []string
	for name := range m.registered {
		if !current[name] {
			gone = append(gone, name)
			delete(m.registered, name)
		}
	}
	if len(gone) > 0 {
		m.Server.RemoveTools(gone...)
	}
}

func (m *Managed) isReadOnlyCall(ctx context.Context) bool {
	m.mu.Lock()
	ro := m.readOnly
	m.mu.Unlock()
	if ro {
		return true
	}
	return audit.CallMetaFromContext(ctx).ReadOnly
}

type runInput struct {
	Title          string `json:"title" jsonschema:"required concise 4-16 word description of what this step is doing (shown live in lrmcp ui)"`
	DoneTitle      string `json:"done_title,omitempty" jsonschema:"optional completed past-tense summary replacing title once finished"`
	Code           string `json:"code" jsonschema:"async JavaScript executed in this session's persistent VM with proc, fs, browser, desktop, img, caps, emitImage, print, sleep"`
	SessionID      string `json:"session_id,omitempty" jsonschema:"optional explicit task session_id returned by session(action='start')"`
	YieldMS        int    `json:"yield_ms,omitempty" jsonschema:"milliseconds to wait before returning a running cell_id (1-60000; default 15000)"`
	MaxOutputBytes int    `json:"max_output_bytes,omitempty" jsonschema:"maximum retained print/console output bytes (default 262144)"`
}

type waitInput struct {
	CellID    string `json:"cell_id" jsonschema:"cell_id returned by a running run call"`
	YieldMS   int    `json:"yield_ms,omitempty" jsonschema:"milliseconds to wait for completion or incremental output (1-60000; default 15000)"`
	Terminate bool   `json:"terminate,omitempty" jsonschema:"when true, immediately cancel the running cell and all its in-flight sub-operations"`
}

func runToolSpec(m *Managed, defaultWire string) toolSpec {
	runTool := inputTool[runInput](
		tool(
			"run",
			"Run stateful runtime script",
			runToolDescription,
			true, false, false, true,
		),
		func(schema *jsonschema.Schema) {
			schema.Required = []string{"title", "code"}
		},
	)
	return toolSpec{
		name:     "run",
		readOnly: true, // read-only remote grants remain allowed and enforce SDK-level read-only inside jsrt
		register: func(s *mcp.Server) {
			mcp.AddTool(s, runTool, func(ctx context.Context, req *mcp.CallToolRequest, in runInput) (*mcp.CallToolResult, jsrt.CellResult, error) {
				if strings.TrimSpace(in.Title) == "" {
					return nil, jsrt.CellResult{}, errors.New("title is required")
				}
				if strings.TrimSpace(in.Code) == "" {
					return nil, jsrt.CellResult{}, errors.New("code is required")
				}
				sid, _, _, wire, _ := resolveCaller(ctx, m, req.Session, defaultWire, "")
				if explicit := audit.SanitizeID(in.SessionID); explicit != "" {
					if strings.Contains(explicit, ":") {
						sid = explicit
					} else {
						sid = wire + ":" + explicit
					}
				}
				res, err := m.runtime.Run(ctx, jsrt.RunOptions{
					SessionID:      sid,
					Title:          strings.TrimSpace(in.Title),
					DoneTitle:      strings.TrimSpace(in.DoneTitle),
					Code:           in.Code,
					YieldMS:        in.YieldMS,
					MaxOutputBytes: in.MaxOutputBytes,
					ReadOnly:       m.isReadOnlyCall(ctx),
				})
				if err != nil {
					return nil, res, err
				}
				return cellResultToMCP(res), res, nil
			})
		},
	}
}

// Close releases any JS runtime engine owned directly by this Managed server.
func (m *Managed) Close() {
	if m != nil && m.ownsRuntime && m.runtime != nil {
		m.runtime.Close()
	}
}

func waitToolSpec(m *Managed, defaultWire string) toolSpec {
	waitTool := inputTool[waitInput](
		tool(
			"wait",
			"Wait for or cancel running cell",
			"Await completion, collect incremental output/images, or cancel a running cell_id returned by run.",
			true, false, false, true,
		),
		func(schema *jsonschema.Schema) {
			schema.Required = []string{"cell_id"}
		},
	)
	return toolSpec{
		name:     "wait",
		readOnly: true,
		register: func(s *mcp.Server) {
			mcp.AddTool(s, waitTool, func(ctx context.Context, req *mcp.CallToolRequest, in waitInput) (*mcp.CallToolResult, jsrt.CellResult, error) {
				sid, _, _, _, _ := resolveCaller(ctx, m, req.Session, defaultWire, "")
				meta := audit.CallMetaFromContext(ctx)
				enforce := meta.Policy != nil || meta.ReadOnly
				res, err := m.runtime.Wait(ctx, jsrt.WaitOptions{
					CellID:         strings.TrimSpace(in.CellID),
					YieldMS:        in.YieldMS,
					Terminate:      in.Terminate,
					SessionID:      sid,
					EnforceSession: enforce,
				})
				if err != nil {
					return nil, res, err
				}
				return cellResultToMCP(res), res, nil
			})
		},
	}
}

func cellResultToMCP(res jsrt.CellResult) *mcp.CallToolResult {
	var content []mcp.Content
	payload, _ := json.Marshal(res)
	content = append(content, &mcp.TextContent{Text: string(payload)})
	for _, img := range res.Images {
		if len(img.Data) > 0 && img.MIMEType != "" {
			content = append(content, &mcp.ImageContent{Data: img.Data, MIMEType: img.MIMEType})
		}
	}
	return &mcp.CallToolResult{
		IsError:           res.Status == "error",
		Content:           content,
		StructuredContent: res,
	}
}

func tool(name, title, description string, readOnly, destructive, idempotent, openWorld bool) *mcp.Tool {
	return &mcp.Tool{
		Name: name, Title: title, Description: description,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: readOnly, DestructiveHint: pointer(destructive),
			IdempotentHint: idempotent, OpenWorldHint: pointer(openWorld),
		},
	}
}

func pointer(value bool) *bool { return &value }

func inputTool[Input any](value *mcp.Tool, configure func(*jsonschema.Schema)) *mcp.Tool {
	schema, err := jsonschema.For[Input](nil)
	if err != nil {
		panic(fmt.Sprintf("create input schema for %s: %v", value.Name, err))
	}
	configure(schema)
	value.InputSchema = schema
	return value
}

func enum(values ...string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}
