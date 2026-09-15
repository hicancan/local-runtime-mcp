//go:build windows

package computer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/kbinani/screenshot"
)

const (
	inputMouse              = 0
	inputKeyboard           = 1
	mouseLeftDown           = 0x0002
	mouseLeftUp             = 0x0004
	mouseRightDown          = 0x0008
	mouseRightUp            = 0x0010
	mouseMiddleDown         = 0x0020
	mouseMiddleUp           = 0x0040
	mouseWheel              = 0x0800
	mouseHWheel             = 0x1000
	keyUp                   = 0x0002
	keyUnicode              = 0x0004
	perMonitorAwareV2       = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
	maximumRememberedStates = 64
)

var (
	user32                        = syscall.NewLazyDLL("user32.dll")
	kernel32                      = syscall.NewLazyDLL("kernel32.dll")
	getCursorPos                  = user32.NewProc("GetCursorPos")
	setCursorPos                  = user32.NewProc("SetCursorPos")
	sendInput                     = user32.NewProc("SendInput")
	setProcessDPIAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	openInputDesktop              = user32.NewProc("OpenInputDesktop")
	setThreadDesktop              = user32.NewProc("SetThreadDesktop")
	getThreadDesktop              = user32.NewProc("GetThreadDesktop")
	closeDesktop                  = user32.NewProc("CloseDesktop")
	enumWindows                   = user32.NewProc("EnumWindows")
	isWindow                      = user32.NewProc("IsWindow")
	isWindowVisible               = user32.NewProc("IsWindowVisible")
	getWindowTextLength           = user32.NewProc("GetWindowTextLengthW")
	getWindowText                 = user32.NewProc("GetWindowTextW")
	getWindowRect                 = user32.NewProc("GetWindowRect")
	getWindowThreadProcessID      = user32.NewProc("GetWindowThreadProcessId")
	getForegroundWindow           = user32.NewProc("GetForegroundWindow")
	setForegroundWindow           = user32.NewProc("SetForegroundWindow")
	bringWindowToTop              = user32.NewProc("BringWindowToTop")
	setFocus                      = user32.NewProc("SetFocus")
	attachThreadInput             = user32.NewProc("AttachThreadInput")
	showWindow                    = user32.NewProc("ShowWindow")
	getCurrentThreadID            = kernel32.NewProc("GetCurrentThreadId")
)

type nativeController struct {
	sequence  atomic.Uint64
	epoch     atomic.Uint64
	operation sync.Mutex
	mu        sync.Mutex
	states    map[string]stateRecord
}

type stateRecord struct {
	epoch      uint64
	windowID   int64
	pid        uint32
	foreground uintptr
	bounds     Rectangle
}

type point struct{ X, Y int32 }
type winRect struct{ Left, Top, Right, Bottom int32 }

type mouseInput struct {
	DX, DY    int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

type keyboardInput struct {
	VirtualKey uint16
	ScanCode   uint16
	Flags      uint32
	Time       uint32
	ExtraInfo  uintptr
}

// input holds the largest Win32 INPUT union member on 64-bit Windows.
type input struct {
	Type    uint32
	Padding uint32
	Data    [32]byte
}

func New() Controller {
	_, _, _ = setProcessDPIAwarenessContext.Call(perMonitorAwareV2)
	controller := &nativeController{states: make(map[string]stateRecord)}
	controller.epoch.Store(1)
	return controller
}

func (c *nativeController) Targets(context.Context) (TargetsResult, error) {
	var windows []Window
	err := onInputDesktop(func() error {
		foreground, _, _ := getForegroundWindow.Call()
		var callbackError error
		callback := syscall.NewCallback(func(handle uintptr, _ uintptr) uintptr {
			visible, _, _ := isWindowVisible.Call(handle)
			if visible == 0 {
				return 1
			}
			title := windowText(handle)
			bounds, ok := windowBounds(handle)
			if title == "" || !ok || bounds.Width <= 0 || bounds.Height <= 0 {
				return 1
			}
			pid, err := windowPID(handle)
			if err != nil {
				callbackError = err
				return 0
			}
			windows = append(windows, Window{ID: int64(handle), PID: int(pid), Title: title, Bounds: bounds, Active: handle == foreground})
			return 1
		})
		ok, _, callErr := enumWindows.Call(callback, 0)
		if ok == 0 {
			if callbackError != nil {
				return callbackError
			}
			return fmt.Errorf("EnumWindows failed: %w", callErr)
		}
		return nil
	})
	if err != nil {
		return TargetsResult{}, err
	}
	sort.SliceStable(windows, func(i, j int) bool {
		if windows[i].Active != windows[j].Active {
			return windows[i].Active
		}
		return strings.ToLower(windows[i].Title) < strings.ToLower(windows[j].Title)
	})
	return TargetsResult{Windows: windows}, nil
}

func (c *nativeController) State(_ context.Context, options StateOptions) ([]byte, State, error) {
	c.operation.Lock()
	defer c.operation.Unlock()
	var data []byte
	var state State
	err := onInputDesktop(func() error {
		bounds, title, pid, foreground, err := selectedTarget(options.WindowID)
		if err != nil {
			return err
		}
		frame, err := screenshot.CaptureRect(image.Rect(bounds.X, bounds.Y, bounds.X+bounds.Width, bounds.Y+bounds.Height))
		if err != nil {
			return fmt.Errorf("capture interactive desktop rectangle %+v: %w", bounds, err)
		}
		after, _, _ := getForegroundWindow.Call()
		if after != foreground {
			return errors.New("foreground window changed during capture; call computer_state again")
		}
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, frame); err != nil {
			return err
		}
		cursor := point{}
		_, _, _ = getCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))
		epoch := c.epoch.Load()
		stateID := fmt.Sprintf("s%d-%d", epoch, c.sequence.Add(1))
		state = State{
			StateID: stateID, WindowID: options.WindowID, Title: title,
			OriginX: bounds.X, OriginY: bounds.Y, Width: bounds.Width, Height: bounds.Height,
			CursorX: int(cursor.X) - bounds.X, CursorY: int(cursor.Y) - bounds.Y, MIMEType: "image/png",
		}
		c.remember(stateID, stateRecord{epoch: epoch, windowID: options.WindowID, pid: pid, foreground: foreground, bounds: bounds})
		data = encoded.Bytes()
		return nil
	})
	return data, state, err
}

func (c *nativeController) Act(_ context.Context, action Action) (ActionResult, error) {
	c.operation.Lock()
	defer c.operation.Unlock()
	if err := Validate(action); err != nil {
		return ActionResult{}, err
	}
	err := onInputDesktop(func() error {
		if action.Kind == "activate" {
			return activateWindow(uintptr(action.WindowID))
		}
		bounds, err := c.actionBounds(action)
		if err != nil {
			return err
		}
		translated, err := translateAction(action, bounds)
		if err != nil {
			return err
		}
		return c.act(translated)
	})
	if err != nil {
		return ActionResult{}, err
	}
	c.invalidate()
	return ActionResult{Kind: action.Kind, WindowID: action.WindowID, Success: true}, nil
}

func (c *nativeController) actionBounds(action Action) (Rectangle, error) {
	record, ok := c.lookup(action.StateID)
	if !ok || record.epoch != c.epoch.Load() {
		return Rectangle{}, errors.New("state_id is unknown or stale; call computer_state again")
	}
	if record.windowID != action.WindowID {
		return Rectangle{}, errors.New("window_id does not match the referenced computer state")
	}
	current, _, pid, foreground, err := selectedTarget(action.WindowID)
	if err != nil {
		return Rectangle{}, err
	}
	if record.bounds != current || record.pid != pid || record.foreground != foreground {
		return Rectangle{}, errors.New("computer state is stale; call computer_state again")
	}
	return current, nil
}

func (c *nativeController) remember(id string, record stateRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.states) >= maximumRememberedStates {
		c.states = make(map[string]stateRecord)
	}
	c.states[id] = record
}

func (c *nativeController) lookup(id string) (stateRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	record, ok := c.states[id]
	return record, ok
}

func (c *nativeController) invalidate() {
	c.epoch.Add(1)
	c.mu.Lock()
	c.states = make(map[string]stateRecord)
	c.mu.Unlock()
}

func selectedTarget(windowID int64) (Rectangle, string, uint32, uintptr, error) {
	foreground, _, _ := getForegroundWindow.Call()
	if windowID != 0 {
		handle := uintptr(windowID)
		valid, _, _ := isWindow.Call(handle)
		visible, _, _ := isWindowVisible.Call(handle)
		if valid == 0 || visible == 0 {
			return Rectangle{}, "", 0, 0, errors.New("window_id is not an available visible window")
		}
		if foreground != handle {
			return Rectangle{}, "", 0, 0, errors.New("selected window is not foreground; activate it, then call computer_state again")
		}
		bounds, ok := windowBounds(handle)
		if !ok || bounds.Width <= 0 || bounds.Height <= 0 {
			return Rectangle{}, "", 0, 0, errors.New("selected window has no capturable bounds")
		}
		pid, err := windowPID(handle)
		return bounds, windowText(handle), pid, foreground, err
	}
	displays := screenshot.NumActiveDisplays()
	if displays < 1 {
		return Rectangle{}, "", 0, 0, errors.New("no active display was found")
	}
	bounds := screenshot.GetDisplayBounds(0)
	for display := 1; display < displays; display++ {
		bounds = bounds.Union(screenshot.GetDisplayBounds(display))
	}
	return Rectangle{X: bounds.Min.X, Y: bounds.Min.Y, Width: bounds.Dx(), Height: bounds.Dy()}, "", 0, foreground, nil
}

func windowBounds(handle uintptr) (Rectangle, bool) {
	value := winRect{}
	ok, _, _ := getWindowRect.Call(handle, uintptr(unsafe.Pointer(&value)))
	if ok == 0 {
		return Rectangle{}, false
	}
	return Rectangle{X: int(value.Left), Y: int(value.Top), Width: int(value.Right - value.Left), Height: int(value.Bottom - value.Top)}, true
}

func windowPID(handle uintptr) (uint32, error) {
	var pid uint32
	_, _, callErr := getWindowThreadProcessID.Call(handle, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return 0, fmt.Errorf("GetWindowThreadProcessId failed: %w", callErr)
	}
	return pid, nil
}

func windowText(handle uintptr) string {
	length, _, _ := getWindowTextLength.Call(handle)
	if length == 0 {
		return ""
	}
	buffer := make([]uint16, int(length)+1)
	written, _, _ := getWindowText.Call(handle, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	return syscall.UTF16ToString(buffer[:written])
}

func activateWindow(handle uintptr) error {
	const restore = 9
	valid, _, _ := isWindow.Call(handle)
	if valid == 0 {
		return errors.New("window_id is not an available window")
	}
	currentThread, _, _ := getCurrentThreadID.Call()
	foreground, _, _ := getForegroundWindow.Call()
	foregroundThread, _, _ := getWindowThreadProcessID.Call(foreground, 0)
	attached := foregroundThread != 0 && foregroundThread != currentThread
	if attached {
		ok, _, callErr := attachThreadInput.Call(currentThread, foregroundThread, 1)
		if ok == 0 {
			return fmt.Errorf("AttachThreadInput failed: %w", callErr)
		}
		defer attachThreadInput.Call(currentThread, foregroundThread, 0)
	}
	_, _, _ = showWindow.Call(handle, restore)
	_, _, _ = bringWindowToTop.Call(handle)
	ok, _, callErr := setForegroundWindow.Call(handle)
	_, _, _ = setFocus.Call(handle)
	active, _, _ := getForegroundWindow.Call()
	if ok == 0 && active != handle {
		return fmt.Errorf("SetForegroundWindow failed: %w", callErr)
	}
	if active != handle {
		return errors.New("window did not become foreground")
	}
	return nil
}

func translateAction(action Action, bounds Rectangle) (Action, error) {
	translate := func(valueX, valueY *int, label string) (*int, *int, error) {
		if valueX == nil && valueY == nil {
			return nil, nil, nil
		}
		if valueX == nil || valueY == nil || *valueX < 0 || *valueY < 0 || *valueX >= bounds.Width || *valueY >= bounds.Height {
			return nil, nil, fmt.Errorf("%s coordinates are outside the referenced state", label)
		}
		x, y := bounds.X+*valueX, bounds.Y+*valueY
		return &x, &y, nil
	}
	var err error
	action.X, action.Y, err = translate(action.X, action.Y, "action")
	if err != nil {
		return Action{}, err
	}
	action.ToX, action.ToY, err = translate(action.ToX, action.ToY, "drag destination")
	if err != nil {
		return Action{}, err
	}
	return action, nil
}

func (c *nativeController) act(action Action) error {
	switch action.Kind {
	case "move":
		return moveCursor(*action.X, *action.Y)
	case "click", "double_click":
		if err := moveCursor(*action.X, *action.Y); err != nil {
			return err
		}
		count := 1
		if action.Kind == "double_click" {
			count = 2
		}
		down, up := uint32(mouseLeftDown), uint32(mouseLeftUp)
		switch action.Button {
		case "right":
			down, up = mouseRightDown, mouseRightUp
		case "middle":
			down, up = mouseMiddleDown, mouseMiddleUp
		}
		for range count {
			if err := sendMouse(down, 0); err != nil {
				return err
			}
			if err := sendMouse(up, 0); err != nil {
				return err
			}
		}
	case "drag":
		if err := moveCursor(*action.X, *action.Y); err != nil {
			return err
		}
		down, up := uint32(mouseLeftDown), uint32(mouseLeftUp)
		if action.Button == "right" {
			down, up = mouseRightDown, mouseRightUp
		} else if action.Button == "middle" {
			down, up = mouseMiddleDown, mouseMiddleUp
		}
		if err := sendMouse(down, 0); err != nil {
			return err
		}
		for step := 1; step <= 20; step++ {
			x := *action.X + (*action.ToX-*action.X)*step/20
			y := *action.Y + (*action.ToY-*action.Y)*step/20
			if err := moveCursor(x, y); err != nil {
				_ = sendMouse(up, 0)
				return err
			}
			time.Sleep(10 * time.Millisecond)
		}
		return sendMouse(up, 0)
	case "type_text":
		return typeUnicode(action.Text)
	case "set_value":
		if err := pressCombination("CTRL+A"); err != nil {
			return err
		}
		return typeUnicode(action.Text)
	case "press_key":
		return pressCombination(action.Key)
	case "scroll":
		if action.X != nil {
			if err := moveCursor(*action.X, *action.Y); err != nil {
				return err
			}
		}
		if action.ScrollY != 0 {
			if err := sendMouse(mouseWheel, int32(-action.ScrollY)); err != nil {
				return err
			}
		}
		if action.ScrollX != 0 {
			return sendMouse(mouseHWheel, int32(action.ScrollX))
		}
	}
	return nil
}

func onInputDesktop(action func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	threadID, _, _ := getCurrentThreadID.Call()
	original, _, _ := getThreadDesktop.Call(threadID)
	const access = 0x0001 | 0x0080
	inputDesktop, _, callErr := openInputDesktop.Call(0, 0, access)
	if inputDesktop == 0 {
		return fmt.Errorf("OpenInputDesktop failed; lrmcp must run in the current interactive Windows session: %w", callErr)
	}
	defer closeDesktop.Call(inputDesktop)
	if inputDesktop != original {
		ok, _, callErr := setThreadDesktop.Call(inputDesktop)
		if ok == 0 {
			return fmt.Errorf("SetThreadDesktop failed: %w", callErr)
		}
		defer setThreadDesktop.Call(original)
	}
	return action()
}

func moveCursor(x, y int) error {
	ok, _, callErr := setCursorPos.Call(uintptr(x), uintptr(y))
	if ok == 0 {
		return fmt.Errorf("SetCursorPos failed: %w", callErr)
	}
	return nil
}

func sendMouse(flags uint32, data int32) error {
	value := mouseInput{MouseData: uint32(data), Flags: flags}
	return sendNativeInput(inputMouse, unsafe.Pointer(&value), unsafe.Sizeof(value))
}

func typeUnicode(text string) error {
	for _, code := range utf16.Encode([]rune(text)) {
		if err := sendKeyboard(0, code, keyUnicode); err != nil {
			return err
		}
		if err := sendKeyboard(0, code, keyUnicode|keyUp); err != nil {
			return err
		}
	}
	return nil
}

func sendKeyboard(virtualKey, scanCode uint16, flags uint32) error {
	value := keyboardInput{VirtualKey: virtualKey, ScanCode: scanCode, Flags: flags}
	return sendNativeInput(inputKeyboard, unsafe.Pointer(&value), unsafe.Sizeof(value))
}

func sendNativeInput(kind uint32, data unsafe.Pointer, size uintptr) error {
	entry := input{Type: kind}
	raw := unsafe.Slice((*byte)(data), int(size))
	copy(entry.Data[:], raw)
	written, _, callErr := sendInput.Call(1, uintptr(unsafe.Pointer(&entry)), unsafe.Sizeof(entry))
	if written != 1 {
		return fmt.Errorf("SendInput failed: %w", callErr)
	}
	return nil
}

func pressCombination(value string) error {
	parts := strings.Split(strings.ToUpper(strings.TrimSpace(value)), "+")
	keys := make([]uint16, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		key, ok := virtualKeys[part]
		if !ok && len(part) == 1 {
			character := part[0]
			if (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') {
				key, ok = uint16(character), true
			}
		}
		if !ok {
			return fmt.Errorf("unsupported key %q", part)
		}
		keys = append(keys, key)
	}
	for _, key := range keys {
		if err := sendKeyboard(key, 0, 0); err != nil {
			return err
		}
	}
	for index := len(keys) - 1; index >= 0; index-- {
		if err := sendKeyboard(keys[index], 0, keyUp); err != nil {
			return err
		}
	}
	return nil
}

var virtualKeys = map[string]uint16{
	"BACKSPACE": 0x08, "TAB": 0x09, "ENTER": 0x0D, "SHIFT": 0x10, "CTRL": 0x11,
	"CONTROL": 0x11, "ALT": 0x12, "ESC": 0x1B, "ESCAPE": 0x1B, "SPACE": 0x20,
	"PAGEUP": 0x21, "PAGEDOWN": 0x22, "END": 0x23, "HOME": 0x24, "LEFT": 0x25,
	"UP": 0x26, "RIGHT": 0x27, "DOWN": 0x28, "DELETE": 0x2E, "META": 0x5B,
	"WIN": 0x5B, "F1": 0x70, "F2": 0x71, "F3": 0x72, "F4": 0x73, "F5": 0x74,
	"F6": 0x75, "F7": 0x76, "F8": 0x77, "F9": 0x78, "F10": 0x79, "F11": 0x7A, "F12": 0x7B,
}
