package browser

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/config"
)

const ExtensionVersion = "4.0.0"

type Bridge struct {
	configured bool
	address    string
	token      string
	server     *http.Server
	commands   chan command
	mu         sync.Mutex
	pending    map[string]chan response
	lastSeen   time.Time
	peer       Peer
	sequence   atomic.Uint64
}

type Peer struct {
	InstanceID       string `json:"instance_id,omitempty"`
	Browser          string `json:"browser,omitempty"`
	ExtensionVersion string `json:"extension_version,omitempty"`
}

type Status struct {
	Configured bool      `json:"configured"`
	Connected  bool      `json:"connected"`
	Address    string    `json:"address,omitempty"`
	LastSeen   time.Time `json:"last_seen,omitempty"`
	Peer       Peer      `json:"peer,omitempty"`
}

type Tab struct {
	ID       int    `json:"id"`
	WindowID int    `json:"window_id"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Active   bool   `json:"active"`
	Status   string `json:"status,omitempty"`
}

type SnapshotElement struct {
	Ref         string         `json:"ref"`
	Tag         string         `json:"tag"`
	Role        string         `json:"role,omitempty"`
	Name        string         `json:"name,omitempty"`
	Text        string         `json:"text,omitempty"`
	Href        string         `json:"href,omitempty"`
	Placeholder string         `json:"placeholder,omitempty"`
	Value       string         `json:"value,omitempty"`
	Disabled    bool           `json:"disabled,omitempty"`
	Rect        map[string]int `json:"rect,omitempty"`
}

type Snapshot struct {
	TabID             int               `json:"tab_id"`
	SnapshotID        string            `json:"snapshot_id"`
	Title             string            `json:"title"`
	URL               string            `json:"url"`
	Text              string            `json:"text"`
	TextTruncated     bool              `json:"text_truncated"`
	Elements          []SnapshotElement `json:"elements"`
	ElementsTruncated bool              `json:"elements_truncated"`
}

type ScreenshotOptions struct {
	TabID    int  `json:"tab_id" jsonschema:"positive browser tab ID"`
	FullPage bool `json:"full_page,omitempty" jsonschema:"capture the entire scrollable page instead of the viewport"`
	ClipX    int  `json:"clip_x,omitempty" jsonschema:"non-negative page X coordinate for a clipped capture"`
	ClipY    int  `json:"clip_y,omitempty" jsonschema:"non-negative page Y coordinate for a clipped capture"`
	ClipW    int  `json:"clip_width,omitempty" jsonschema:"positive clipped-capture width; requires clip_height"`
	ClipH    int  `json:"clip_height,omitempty" jsonschema:"positive clipped-capture height; requires clip_width"`
}

type ScreenshotInfo struct {
	TabID        int    `json:"tab_id"`
	ScreenshotID string `json:"screenshot_id"`
	Title        string `json:"title"`
	URL          string `json:"url"`
	X            int    `json:"x"`
	Y            int    `json:"y"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FullPage     bool   `json:"full_page"`
	MIMEType     string `json:"mime_type"`
}

type Action struct {
	Kind         string   `json:"kind" jsonschema:"browser operation to perform"`
	TabID        int      `json:"tab_id" jsonschema:"positive browser tab ID"`
	Selector     string   `json:"selector,omitempty" jsonschema:"CSS selector in the top document; prefer ref after a snapshot"`
	Ref          string   `json:"ref,omitempty" jsonschema:"versioned element reference from the latest browser_snapshot"`
	ScreenshotID string   `json:"screenshot_id,omitempty" jsonschema:"viewport screenshot ID required for coordinate targeting"`
	X            int      `json:"x,omitempty" jsonschema:"viewport X coordinate associated with screenshot_id"`
	Y            int      `json:"y,omitempty" jsonschema:"viewport Y coordinate associated with screenshot_id"`
	ToX          int      `json:"to_x,omitempty" jsonschema:"drag destination viewport X coordinate"`
	ToY          int      `json:"to_y,omitempty" jsonschema:"drag destination viewport Y coordinate"`
	Button       string   `json:"button,omitempty" jsonschema:"mouse button; defaults to left"`
	Text         string   `json:"text,omitempty" jsonschema:"text for type_text or set_value"`
	Key          string   `json:"key,omitempty" jsonschema:"key or modifier combination such as Control+L"`
	ScrollX      int      `json:"scroll_x,omitempty" jsonschema:"horizontal wheel delta; positive scrolls right"`
	ScrollY      int      `json:"scroll_y,omitempty" jsonschema:"vertical wheel delta; positive scrolls down"`
	Option       string   `json:"option,omitempty" jsonschema:"select option value, visible text, or label"`
	Checked      *bool    `json:"checked,omitempty" jsonschema:"desired checkbox or radio state"`
	Files        []string `json:"files,omitempty" jsonschema:"absolute machine paths assigned to a file input"`
	Accept       *bool    `json:"accept,omitempty" jsonschema:"whether to accept an open JavaScript dialog"`
	PromptText   string   `json:"prompt_text,omitempty" jsonschema:"text supplied to an accepted prompt dialog"`
	Script       string   `json:"script,omitempty" jsonschema:"JavaScript expression evaluated in the page main world"`
}

type ActionResult struct {
	TabID   int    `json:"tab_id"`
	Kind    string `json:"kind"`
	Success bool   `json:"success"`
	Value   any    `json:"value,omitempty"`
}

type command struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type response struct {
	ID         string          `json:"id"`
	InstanceID string          `json:"instance_id"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
}

func Start(ctx context.Context, configuration config.Browser) (*Bridge, error) {
	bridge := &Bridge{configured: configuration.Token != "", token: configuration.Token, commands: make(chan command), pending: make(map[string]chan response)}
	if !bridge.configured {
		return bridge, nil
	}
	if configuration.Listen == "" {
		configuration.Listen = config.DefaultListen
	}
	listener, err := net.Listen("tcp", configuration.Listen)
	if err != nil {
		return nil, fmt.Errorf("start browser extension bridge: %w", err)
	}
	bridge.address = listener.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/poll", bridge.poll)
	mux.HandleFunc("/v1/result", bridge.receiveResult)
	bridge.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = bridge.server.Shutdown(shutdownContext)
	}()
	go func() { _ = bridge.server.Serve(listener) }()
	return bridge, nil
}

func (b *Bridge) Close(ctx context.Context) error {
	if b.server == nil {
		return nil
	}
	shutdownContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err := b.server.Shutdown(shutdownContext)
	if errors.Is(err, context.DeadlineExceeded) {
		return b.server.Close()
	}
	return err
}

func (b *Bridge) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Status{Configured: b.configured, Connected: b.connectedLocked(), Address: b.address, LastSeen: b.lastSeen, Peer: b.peer}
}

func (b *Bridge) Tabs(ctx context.Context) ([]Tab, error) {
	var result []Tab
	err := b.call(ctx, "tabs.list", struct{}{}, &result)
	return result, err
}

func (b *Bridge) Open(ctx context.Context, url string, active bool) (Tab, error) {
	var result Tab
	err := b.call(ctx, "tabs.open", map[string]any{"url": url, "active": active}, &result)
	return result, err
}

func (b *Bridge) CloseTab(ctx context.Context, tabID int) error {
	return b.call(ctx, "tabs.close", map[string]any{"tab_id": tabID}, &struct{}{})
}

func (b *Bridge) Navigate(ctx context.Context, tabID int, url string) (Tab, error) {
	var result Tab
	err := b.call(ctx, "page.navigate", map[string]any{"tab_id": tabID, "url": url}, &result)
	return result, err
}

func (b *Bridge) Snapshot(ctx context.Context, tabID, maxElements, maxText int) (Snapshot, error) {
	var result Snapshot
	err := b.call(ctx, "page.snapshot", map[string]any{"tab_id": tabID, "max_elements": maxElements, "max_text": maxText}, &result)
	return result, err
}

func (b *Bridge) Screenshot(ctx context.Context, options ScreenshotOptions) ([]byte, ScreenshotInfo, error) {
	var result struct {
		Data string         `json:"data_base64"`
		Info ScreenshotInfo `json:"info"`
	}
	if options.TabID <= 0 {
		return nil, ScreenshotInfo{}, errors.New("tab_id must be positive")
	}
	if options.FullPage && (options.ClipW != 0 || options.ClipH != 0 || options.ClipX != 0 || options.ClipY != 0) {
		return nil, ScreenshotInfo{}, errors.New("full_page and clip fields are mutually exclusive")
	}
	if (options.ClipW == 0) != (options.ClipH == 0) || options.ClipW < 0 || options.ClipH < 0 || options.ClipX < 0 || options.ClipY < 0 {
		return nil, ScreenshotInfo{}, errors.New("clip requires positive clip_width and clip_height with non-negative coordinates")
	}
	if err := b.call(ctx, "page.screenshot", options, &result); err != nil {
		return nil, ScreenshotInfo{}, err
	}
	data, err := decodeBase64(result.Data)
	return data, result.Info, err
}

func (b *Bridge) Act(ctx context.Context, action Action) (ActionResult, error) {
	if err := validateAction(action); err != nil {
		return ActionResult{}, err
	}
	var result ActionResult
	err := b.call(ctx, "page.action", action, &result)
	return result, err
}

func (b *Bridge) call(ctx context.Context, method string, params, output any) error {
	if !b.configured {
		return errors.New("browser is not configured; run lrmcp browser-setup")
	}
	b.mu.Lock()
	if !b.connectedLocked() {
		b.mu.Unlock()
		return errors.New("browser extension is not connected")
	}
	id := fmt.Sprintf("%d", b.sequence.Add(1))
	resultChannel := make(chan response, 1)
	b.pending[id] = resultChannel
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
	}()
	payload, err := json.Marshal(params)
	if err != nil {
		return err
	}
	select {
	case b.commands <- command{ID: id, Method: method, Params: payload}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case response := <-resultChannel:
		if response.Error != "" {
			return errors.New(response.Error)
		}
		if output == nil || len(response.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(response.Result, output); err != nil {
			return fmt.Errorf("decode browser extension response: %w", err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Bridge) poll(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !b.authorized(request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	var peer Peer
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4096)).Decode(&peer); err != nil && !errors.Is(err, io.EOF) {
		http.Error(writer, "invalid peer metadata", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(peer.InstanceID) == "" {
		http.Error(writer, "instance_id is required", http.StatusBadRequest)
		return
	}
	if peer.ExtensionVersion != ExtensionVersion {
		http.Error(writer, "browser extension version does not match lrmcp; run browser-setup and reload the extension", http.StatusUpgradeRequired)
		return
	}
	b.mu.Lock()
	if b.connectedLocked() && b.peer.InstanceID != "" && b.peer.InstanceID != peer.InstanceID {
		b.mu.Unlock()
		http.Error(writer, "another browser extension instance is connected", http.StatusConflict)
		return
	}
	b.lastSeen = time.Now()
	b.peer = peer
	b.mu.Unlock()
	timer := time.NewTimer(25 * time.Second)
	defer timer.Stop()
	select {
	case item := <-b.commands:
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(item)
	case <-timer.C:
		writer.WriteHeader(http.StatusNoContent)
	case <-request.Context().Done():
	}
}

func (b *Bridge) receiveResult(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !b.authorized(request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	var result response
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<20)).Decode(&result); err != nil || result.ID == "" || result.InstanceID == "" {
		http.Error(writer, "invalid result", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	if b.peer.InstanceID == "" || result.InstanceID != b.peer.InstanceID {
		b.mu.Unlock()
		http.Error(writer, "browser extension instance does not own this bridge", http.StatusConflict)
		return
	}
	channel := b.pending[result.ID]
	b.lastSeen = time.Now()
	b.mu.Unlock()
	if channel == nil {
		http.Error(writer, "unknown command", http.StatusNotFound)
		return
	}
	select {
	case channel <- result:
		writer.WriteHeader(http.StatusNoContent)
	case <-request.Context().Done():
	}
}

func (b *Bridge) authorized(request *http.Request) bool {
	provided := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	return len(provided) == len(b.token) && subtle.ConstantTimeCompare([]byte(provided), []byte(b.token)) == 1
}

func (b *Bridge) connectedLocked() bool {
	return !b.lastSeen.IsZero() && time.Since(b.lastSeen) < 35*time.Second
}

func validateAction(action Action) error {
	if action.TabID <= 0 {
		return errors.New("tab_id must be positive")
	}
	targeted := func() bool { return action.Selector != "" || action.Ref != "" || action.ScreenshotID != "" }
	if action.Button != "" && action.Button != "left" && action.Button != "middle" && action.Button != "right" {
		return errors.New("button must be left, middle, or right")
	}
	switch action.Kind {
	case "click", "double_click", "hover", "drag":
		if !targeted() {
			return fmt.Errorf("%s requires selector, ref, or screenshot_id coordinates", action.Kind)
		}
		if action.ScreenshotID != "" && (action.X < 0 || action.Y < 0) {
			return errors.New("screenshot coordinates cannot be negative")
		}
	case "type_text", "set_value":
		if action.Selector == "" && action.Ref == "" {
			return fmt.Errorf("%s requires selector or ref", action.Kind)
		}
		if action.Text == "" {
			return fmt.Errorf("%s requires text", action.Kind)
		}
	case "press_key":
		if action.Key == "" {
			return errors.New("press_key requires key")
		}
	case "scroll":
		if action.ScrollX == 0 && action.ScrollY == 0 {
			return errors.New("scroll requires scroll_x or scroll_y")
		}
	case "select":
		if (action.Selector == "" && action.Ref == "") || action.Option == "" {
			return errors.New("select requires selector or ref and option")
		}
	case "check":
		if (action.Selector == "" && action.Ref == "") || action.Checked == nil {
			return errors.New("check requires selector or ref and checked")
		}
	case "upload_files":
		if (action.Selector == "" && action.Ref == "") || len(action.Files) == 0 {
			return errors.New("upload_files requires selector or ref and files")
		}
	case "handle_dialog":
		if action.Accept == nil {
			return errors.New("handle_dialog requires accept")
		}
	case "evaluate":
		if action.Script == "" {
			return errors.New("evaluate requires script")
		}
	case "back", "forward", "reload":
	default:
		return fmt.Errorf("unsupported browser action %q", action.Kind)
	}
	return nil
}

//go:embed extension/*
var extensionFiles embed.FS

func InstallExtension(directory string) (string, error) {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	entries, err := extensionFiles.ReadDir("extension")
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := extensionFiles.ReadFile("extension/" + entry.Name())
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(abs, entry.Name()), data, 0o644); err != nil {
			return "", err
		}
	}
	return abs, nil
}

func ConfigureExtension(directory, address, token string) error {
	if address == "" || len(token) < 32 {
		return errors.New("extension configuration requires a bridge address and a token of at least 32 characters")
	}
	if !strings.HasPrefix(address, "127.0.0.1:") && !strings.HasPrefix(address, "localhost:") && !strings.HasPrefix(address, "[::1]:") {
		return errors.New("extension bridge address must use a loopback host")
	}
	payload := fmt.Sprintf("export const packagedConfig = { address: %q, token: %q };\n", address, token)
	path := filepath.Join(directory, "runtime_config.js")
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func DefaultExtensionDirectory() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "local-runtime-mcp", "browser-extension"), nil
}

func decodeBase64(value string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode browser screenshot: %w", err)
	}
	return data, nil
}
