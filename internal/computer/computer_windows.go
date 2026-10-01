//go:build windows && amd64

package computer

import (
	"bufio"
	"context"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const computerWorkerVersion = "10.0.1"

//go:embed worker_windows_amd64.exe
var workerExecutable embed.FS

type workerController struct {
	ctx         context.Context
	cancel      context.CancelFunc
	gate        chan struct{}
	waiting     chan struct{}
	mu          sync.Mutex
	writeMu     sync.Mutex
	lifecycleMu sync.Mutex
	sequence    atomic.Uint64
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	directory   string
	done        chan struct{}
	pending     map[uint64]chan workerExchange
	inputs      map[string]inputJournal
	onStop      func()
}
type workerRequest struct {
	ID     uint64 `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}
type inputJournal struct {
	Key    uint16 `json:"key"`
	Scan   uint16 `json:"scan"`
	Flags  uint32 `json:"flags"`
	Button string `json:"button"`
	Down   bool   `json:"down"`
}
type workerResponse struct {
	ID          uint64          `json:"id"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       string          `json:"error,omitempty"`
	ImageBase64 string          `json:"image_base64,omitempty"`
	Event       string          `json:"event,omitempty"`
	Input       *inputJournal   `json:"input,omitempty"`
}
type workerExchange struct {
	response workerResponse
	err      error
}

func New(ctx context.Context) Controller {
	nativeContext, cancelNative := context.WithCancel(ctx)
	native := &workerController{ctx: nativeContext, cancel: cancelNative, gate: make(chan struct{}, 1), waiting: make(chan struct{}, 64), pending: make(map[uint64]chan workerExchange), inputs: make(map[string]inputJournal)}
	native.gate <- struct{}{}
	c := newCoordinator(ctx, native)
	native.onStop = c.LocalStop
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-c.closeDone:
		}
	}()
	return c
}
func (c *workerController) Targets(ctx context.Context) (TargetsResult, error) {
	var result []Window
	_, err := c.call(ctx, "targets", struct{}{}, &result)
	return TargetsResult{Windows: result}, err
}
func (c *workerController) State(ctx context.Context, options StateOptions) ([]byte, State, error) {
	var state State
	response, err := c.call(ctx, "state", options, &state)
	if err != nil {
		return nil, State{}, err
	}
	data, err := base64.StdEncoding.DecodeString(response.ImageBase64)
	return data, state, err
}
func (c *workerController) Act(ctx context.Context, action Action) (ActionResult, error) {
	_, err := c.call(ctx, "act", action, nil)
	return ActionResult{Kind: action.Kind, TargetID: action.TargetID, Success: err == nil}, err
}
func (c *workerController) ShowControl(ctx context.Context, label string) error {
	_, err := c.call(ctx, "show_control", map[string]string{"label": label}, nil)
	return err
}
func (c *workerController) Invalidate(ctx context.Context) error {
	_, err := c.call(ctx, "invalidate", nil, nil)
	return err
}
func (c *workerController) Reset(ctx context.Context) error {
	c.mu.Lock()
	started := c.cmd != nil
	c.mu.Unlock()
	if !started {
		if !c.releaseInputs() {
			return errors.New("injected input cleanup incomplete")
		}
		return nil
	}
	// Interrupt reader bypasses the operation queue and releases native inputs.
	_, err := c.interrupt(ctx, "stop", nil)
	if err != nil {
		if err.Error() == "native semantic action still settling" {
			return err
		}
		c.stop()
		if !c.releaseInputs() {
			return errors.New("injected input cleanup incomplete")
		}
		return nil
	}
	return err
}
func (c *workerController) Close() error {
	c.cancel()
	c.stop()
	if !c.releaseInputs() {
		return errors.New("injected input cleanup incomplete")
	}
	return nil
}

func (c *workerController) call(ctx context.Context, method string, params, output any) (workerResponse, error) {
	select {
	case c.waiting <- struct{}{}:
	default:
		return workerResponse{}, errors.New("native computer operation queue is full")
	}
	defer func() { <-c.waiting }()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	select {
	case <-ctx.Done():
		return workerResponse{}, ctx.Err()
	case <-c.ctx.Done():
		return workerResponse{}, errors.New("computer controller closed")
	case <-c.gate:
	}
	defer func() { c.gate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return workerResponse{}, err
	}
	if err := c.ctx.Err(); err != nil {
		return workerResponse{}, err
	}
	if !c.releaseInputs() {
		return workerResponse{}, errors.New("injected input cleanup incomplete")
	}
	c.mu.Lock()
	started := c.cmd != nil
	c.mu.Unlock()
	if !started {
		if err := c.start(ctx); err != nil {
			return workerResponse{}, err
		}
	}
	response, err := c.request(ctx, method, params, true)
	if err != nil {
		return workerResponse{}, err
	}
	if output != nil && len(response.Result) > 0 {
		if err := json.Unmarshal(response.Result, output); err != nil {
			return response, fmt.Errorf("decode Rust computer result: %w", err)
		}
	}
	return response, nil
}
func (c *workerController) interrupt(ctx context.Context, method string, params any) (workerResponse, error) {
	return c.request(ctx, method, params, false)
}
func (c *workerController) request(ctx context.Context, method string, params any, cancelOnDeadline bool) (workerResponse, error) {
	id := c.sequence.Add(1)
	completed := make(chan workerExchange, 1)
	c.mu.Lock()
	if c.stdin == nil {
		c.mu.Unlock()
		return workerResponse{}, errors.New("Rust computer worker is not running")
	}
	c.pending[id] = completed
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err := c.write(workerRequest{ID: id, Method: method, Params: params}); err != nil {
		c.stop()
		return workerResponse{}, err
	}
	select {
	case result := <-completed:
		if result.err != nil {
			return workerResponse{}, result.err
		}
		if result.response.Error != "" {
			return result.response, errors.New(result.response.Error)
		}
		return result.response, nil
	case <-ctx.Done():
		if cancelOnDeadline {
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := c.interrupt(cleanup, "cancel", map[string]uint64{"request_id": id})
			if err != nil {
				c.stop()
			}
			select {
			case <-completed:
			case <-cleanup.Done():
				c.stop()
			}
		}
		return workerResponse{}, ctx.Err()
	}
}
func (c *workerController) write(request workerRequest) error {
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return errors.New("computer request exceeds 16 MiB")
	}
	var header [4]byte
	binary.LittleEndian.PutUint32(header[:], uint32(len(data)))
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		return errors.New("Rust computer worker disconnected")
	}
	_, err = stdin.Write(append(header[:], data...))
	return err
}
func (c *workerController) start(ctx context.Context) error {
	c.lifecycleMu.Lock()
	locked := true
	defer func() {
		if locked {
			c.lifecycleMu.Unlock()
		}
	}()
	directory, err := os.MkdirTemp("", "local-runtime-mcp-computer-")
	if err != nil {
		return err
	}
	data, err := workerExecutable.ReadFile("worker_windows_amd64.exe")
	if err != nil {
		_ = os.RemoveAll(directory)
		return err
	}
	path := filepath.Join(directory, "computer-worker.exe")
	if err := os.WriteFile(path, data, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return err
	}
	cmd := exec.CommandContext(c.ctx, path)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = os.RemoveAll(directory)
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		_ = os.RemoveAll(directory)
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(directory)
		return err
	}
	done := make(chan struct{})
	c.mu.Lock()
	c.cmd, c.stdin, c.directory, c.done = cmd, stdin, directory, done
	c.mu.Unlock()
	c.lifecycleMu.Unlock()
	locked = false
	go func() { _ = cmd.Wait(); close(done) }()
	go c.read(bufio.NewReaderSize(stdout, 64*1024), cmd)
	response, err := c.request(ctx, "hello", nil, false)
	if err != nil {
		c.stop()
		return err
	}
	var hello struct {
		Version string `json:"version"`
		Backend string `json:"backend"`
	}
	if err := json.Unmarshal(response.Result, &hello); err != nil || hello.Version != computerWorkerVersion || hello.Backend != "windows-rust-wgc-uia" {
		c.stop()
		return fmt.Errorf("Rust computer worker identity mismatch: version=%q backend=%q", hello.Version, hello.Backend)
	}
	return nil
}
func (c *workerController) read(reader *bufio.Reader, cmd *exec.Cmd) {
	for {
		var header [4]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			c.disconnected(cmd, err)
			return
		}
		length := binary.LittleEndian.Uint32(header[:])
		if length == 0 || length > 96<<20 {
			c.disconnected(cmd, errors.New("invalid computer response frame"))
			return
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(reader, data); err != nil {
			c.disconnected(cmd, err)
			return
		}
		var response workerResponse
		if err := json.Unmarshal(data, &response); err != nil {
			c.disconnected(cmd, err)
			return
		}
		if response.Event != "" {
			switch response.Event {
			case "local_stop":
				if c.onStop != nil {
					c.onStop()
				}
			case "input":
				if response.Input != nil {
					journal := *response.Input
					key := fmt.Sprintf("%d:%d:%d:%s", journal.Key, journal.Scan, journal.Flags&4, journal.Button)
					c.mu.Lock()
					if journal.Down {
						c.inputs[key] = journal
					} else {
						delete(c.inputs, key)
					}
					c.mu.Unlock()
				}
			}
			continue
		}
		c.mu.Lock()
		channel := c.pending[response.ID]
		c.mu.Unlock()
		if channel != nil {
			select {
			case channel <- workerExchange{response: response}:
			default:
			}
		}
	}
}
func (c *workerController) disconnected(cmd *exec.Cmd, err error) {
	c.mu.Lock()
	if c.cmd != cmd {
		c.mu.Unlock()
		return
	}
	for _, pending := range c.pending {
		select {
		case pending <- workerExchange{err: fmt.Errorf("Rust computer worker disconnected: %w", err)}:
		default:
		}
	}
	c.mu.Unlock()
	c.stopCommand(cmd)
}
func (c *workerController) stop() {
	c.stopCommand(nil)
}
func (c *workerController) stopCommand(expected *exec.Cmd) {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	c.mu.Lock()
	if expected != nil && c.cmd != expected {
		c.mu.Unlock()
		return
	}
	cmd, stdin, directory, done := c.cmd, c.stdin, c.directory, c.done
	c.cmd, c.stdin, c.directory, c.done = nil, nil, "", nil
	for _, pending := range c.pending {
		select {
		case pending <- workerExchange{err: errors.New("Rust computer worker stopped")}:
		default:
		}
	}
	c.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	c.releaseInputs()
	if directory != "" {
		_ = os.RemoveAll(directory)
	}
	if cmd != nil && c.onStop != nil {
		c.onStop()
	}
}

// Keep a supervisor copy of native input-down events so worker crashes also
// release keys and buttons. INPUT is 40 bytes on Windows amd64.
func (c *workerController) releaseInputs() bool {
	c.mu.Lock()
	inputs := c.inputs
	c.inputs = make(map[string]inputJournal)
	c.mu.Unlock()
	proc := windows.NewLazySystemDLL("user32.dll").NewProc("SendInput")
	for key, input := range inputs {
		var raw [40]byte
		if input.Button != "" {
			binary.LittleEndian.PutUint32(raw[0:], 0)
			flags := uint32(4)
			if input.Button == "right" {
				flags = 16
			}
			if input.Button == "middle" {
				flags = 64
			}
			binary.LittleEndian.PutUint32(raw[20:], flags)
		} else {
			binary.LittleEndian.PutUint32(raw[0:], 1)
			binary.LittleEndian.PutUint16(raw[8:], input.Key)
			binary.LittleEndian.PutUint16(raw[10:], input.Scan)
			binary.LittleEndian.PutUint32(raw[12:], input.Flags|2)
		}
		count, _, _ := proc.Call(1, uintptr(unsafe.Pointer(&raw[0])), 40)
		if count != 1 {
			c.mu.Lock()
			c.inputs[key] = input
			c.mu.Unlock()
		}
	}
	c.mu.Lock()
	clear := len(c.inputs) == 0
	c.mu.Unlock()
	return clear
}
