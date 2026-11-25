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

const instructions = `Local Runtime MCP (v18) exposes this machine through 4 orthogonal primitives:
1. run(title, code, done_title?, yield_ms?, max_output_bytes?): Execute stateful async JavaScript in your session's persistent VM. Top-level variables & functions persist across turns. Parallelize independent I/O with await Promise.allSettled([...]). Always pass a concise human-readable title (and optional done_title) describing what this step does.
   Built-in async SDK inside run:
   - proc.run({ cmd, shell?, cwd?, max_output_tokens?, timeout_seconds? }) or proc.run(program, args, opts); proc.continue(opts); proc.kill(session_id)
   - fs.list / fs.stat / fs.read / fs.write / fs.patch / fs.search (path-locked atomic writes with expected_sha256)
   - browser.status / browser.connect / browser.tabs / browser.open(url, { browser_id?, active?, session_name? }) / browser.tab(id) / browser.cdp(method, params, browser_id?) / browser.traceStart / browser.traceStop / browser.cleanup
     TabHandle: tab.goto / reload / back / forward / close / snapshot({ diff: true }) / screenshot / eval / cdp(method, params) / getByRole / getByText / getByLabel / getByPlaceholder / locator (with .click/.fill/.type/.press/.select/.check/.uploadFiles/.waitFor) / network / networkBody / console / emulate / css / heapSnapshot / logpoint / screencastStart / screencastStop / markHandoff()
   - desktop.targets / desktop.state / desktop.wait / desktop.action / desktop.control('acquire'|'status'|'release')
   - img.read(path, opts); emitImage(imgOrShotOrPath, label?) returns native MCP ImageContent
   - caps.list() / caps.call(name, args, browser_id?, tab_id?); print(...); sleep(ms)
2. wait(cell_id, yield_ms?, terminate?): Continue awaiting incremental output/images or cancel a running cell.
3. session(action='start'|'update'|'done', title?, message?, status?, session_id?, agent_id?): Open a fresh named task thread (action='start'), post major milestones ('update'), or finish and reclaim temporary browser tabs ('done').
4. capability_propose(name, definition, script): Propose a reusable owner-approved site tool.`

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

	mu         sync.Mutex
	wire       string
	readOnly   bool
	filter     ToolFilter
	registered map[string]string
	static     []toolSpec
	registry   *capability.Registry
	runtime    *jsrt.Engine

	taskMu      sync.RWMutex
	activeTasks map[string]string
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
	if engine == nil {
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
		activeTasks: map[string]string{},
	}

	static := []toolSpec{
		runToolSpec(managed, deps.Wire),
		waitToolSpec(managed),
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
			"Execute async JavaScript in this session's persistent VM with full concurrent access to proc, fs, browser (full browser-level CDP + Tracing + DevTools + AX diff + semantic locators), desktop (Rust UIA + control), img, caps, emitImage, print, and sleep. Parallelize independent calls with await Promise.allSettled([...]).",
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
				sid, _, _, _, _ := resolveCaller(ctx, m, req.Session, defaultWire, "")
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

func waitToolSpec(m *Managed) toolSpec {
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
			mcp.AddTool(s, waitTool, func(ctx context.Context, _ *mcp.CallToolRequest, in waitInput) (*mcp.CallToolResult, jsrt.CellResult, error) {
				res, err := m.runtime.Wait(ctx, jsrt.WaitOptions{
					CellID:    strings.TrimSpace(in.CellID),
					YieldMS:   in.YieldMS,
					Terminate: in.Terminate,
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
