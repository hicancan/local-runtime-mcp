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
	mouseLeftDown   = 0x0002
	mouseLeftUp     = 0x0004
	mouseRightDown  = 0x0008
	mouseRightUp    = 0x0010
	mouseMiddleDown = 0x0020
	mouseMiddleUp   = 0x0040
	mouseWheel      = 0x0800
	mouseHWheel     = 0x1000
	keyUp           = 0x0002
	keyUnicode      = 0x0004
	inputKeyboard   = 1
	maxElements     = 2000
)

var (
	user32                   = syscall.NewLazyDLL("user32.dll")
	getCursorPos             = user32.NewProc("GetCursorPos")
	setCursorPos             = user32.NewProc("SetCursorPos")
	mouseEvent               = user32.NewProc("mouse_event")
	keybdEvent               = user32.NewProc("keybd_event")
	sendInput                = user32.NewProc("SendInput")
	setProcessDPIAware       = user32.NewProc("SetProcessDPIAware")
	openInputDesktop         = user32.NewProc("OpenInputDesktop")
	setThreadDesktop         = user32.NewProc("SetThreadDesktop")
	getThreadDesktop         = user32.NewProc("GetThreadDesktop")
	closeDesktop             = user32.NewProc("CloseDesktop")
	enumWindows              = user32.NewProc("EnumWindows")
	enumChildWindows         = user32.NewProc("EnumChildWindows")
	isWindowVisible          = user32.NewProc("IsWindowVisible")
	isWindowEnabled          = user32.NewProc("IsWindowEnabled")
	getWindowTextLength      = user32.NewProc("GetWindowTextLengthW")
	getWindowText            = user32.NewProc("GetWindowTextW")
	getClassName             = user32.NewProc("GetClassNameW")
	getWindowRect            = user32.NewProc("GetWindowRect")
	getWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	getForegroundWindow      = user32.NewProc("GetForegroundWindow")
	setForegroundWindow      = user32.NewProc("SetForegroundWindow")
	bringWindowToTop         = user32.NewProc("BringWindowToTop")
	setFocus                 = user32.NewProc("SetFocus")
	attachThreadInput        = user32.NewProc("AttachThreadInput")
	showWindow               = user32.NewProc("ShowWindow")
	getCurrentThreadID       = syscall.NewLazyDLL("kernel32.dll").NewProc("GetCurrentThreadId")
)

type nativeController struct {
	sequence atomic.Uint64
	mu       sync.Mutex
	states   map[string]stateRecord
}

type stateRecord struct {
	windowID int64
	bounds   Rectangle
	elements map[string]Rectangle
}

type point struct{ X, Y int32 }
type winRect struct{ Left, Top, Right, Bottom int32 }

type keyboardInput struct {
	VirtualKey uint16
	ScanCode   uint16
	Flags      uint32
	Time       uint32
	ExtraInfo  uintptr
}

type input struct {
	Type    uint32
	Padding uint32
	Data    [32]byte
}

func New() Controller {
	_, _, _ = setProcessDPIAware.Call()
	return &nativeController{states: make(map[string]stateRecord)}
}

func (*nativeController) Backend() string { return "windows-native" }

func (c *nativeController) Targets(context.Context) (TargetsResult, error) {
	var windows []Window
	err := onInputDesktop(func() error {
		var callbackError error
		callback := syscall.NewCallback(func(handle uintptr, _ uintptr) uintptr {
			if visible, _, _ := isWindowVisible.Call(handle); visible == 0 {
				return 1
			}
			title := windowText(handle)
			bounds, ok := windowBounds(handle)
			if title == "" || !ok || bounds.Width <= 0 || bounds.Height <= 0 {
				return 1
			}
			var pid uint32
			_, _, callErr := getWindowThreadProcessID.Call(handle, uintptr(unsafe.Pointer(&pid)))
			if callErr != syscall.Errno(0) && pid == 0 {
				callbackError = callErr
				return 0
			}
			foreground, _, _ := getForegroundWindow.Call()
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
	return TargetsResult{Backend: c.Backend(), Windows: windows}, nil
}

func (c *nativeController) State(_ context.Context, options StateOptions) ([]byte, State, error) {
	var data []byte
	var state State
	err := onInputDesktop(func() error {
		bounds, title, err := selectedBounds(options.WindowID)
		if err != nil {
			return err
		}
		frame, err := screenshot.CaptureRect(image.Rect(bounds.X, bounds.Y, bounds.X+bounds.Width, bounds.Y+bounds.Height))
		if err != nil {
			return fmt.Errorf("capture desktop rectangle %+v: %w", bounds, err)
		}
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, frame); err != nil {
			return err
		}
		cursor := point{}
		_, _, _ = getCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))
		stateID := fmt.Sprintf("s%d", c.sequence.Add(1))
		state = State{
			Backend: c.Backend(), StateID: stateID, WindowID: options.WindowID, Title: title,
			OriginX: bounds.X, OriginY: bounds.Y, Width: bounds.Width, Height: bounds.Height,
			CursorX: int(cursor.X) - bounds.X, CursorY: int(cursor.Y) - bounds.Y, MIMEType: "image/png",
		}
		elementBounds := make(map[string]Rectangle)
		if options.IncludeAccessibility {
			accessibility := enumerateControls(uintptr(options.WindowID), bounds)
			state.Accessibility = &accessibility
			for _, element := range accessibility.Elements {
				elementBounds[element.Ref] = element.Bounds
			}
		}
		c.remember(stateID, stateRecord{windowID: options.WindowID, bounds: bounds, elements: elementBounds})
		data = encoded.Bytes()
		return nil
	})
	return data, state, err
}

func (c *nativeController) Act(_ context.Context, action Action) (ActionResult, error) {
	if err := Validate(action); err != nil {
		return ActionResult{}, err
	}
	err := onInputDesktop(func() error {
		bounds, err := c.actionBounds(action)
		if err != nil {
			return err
		}
		if action.Kind == "activate" {
			if action.WindowID == 0 {
				return errors.New("activate requires window_id")
			}
			return activateWindow(uintptr(action.WindowID))
		}
		if action.WindowID != 0 {
			if err := activateWindow(uintptr(action.WindowID)); err != nil {
				return err
			}
		}
		if action.ElementRef != "" {
			record, _ := c.lookup(action.StateID)
			element, ok := record.elements[action.ElementRef]
			if !ok {
				return errors.New("element_ref is not present in the referenced state")
			}
			action.X = bounds.X + element.X + element.Width/2
			action.Y = bounds.Y + element.Y + element.Height/2
			action.ToX += bounds.X
			action.ToY += bounds.Y
			if action.Kind == "type_text" || action.Kind == "set_value" || action.Kind == "press_key" {
				if err := c.act(Action{Kind: "click", X: action.X, Y: action.Y}); err != nil {
					return err
				}
			}
		} else {
			action.X += bounds.X
			action.Y += bounds.Y
			action.ToX += bounds.X
			action.ToY += bounds.Y
		}
		return c.act(action)
	})
	if err != nil {
		return ActionResult{}, err
	}
	return ActionResult{Backend: c.Backend(), Kind: action.Kind, WindowID: action.WindowID, Success: true}, nil
}

func (c *nativeController) actionBounds(action Action) (Rectangle, error) {
	current, _, err := selectedBounds(action.WindowID)
	if err != nil {
		return Rectangle{}, err
	}
	if action.StateID == "" {
		return current, nil
	}
	record, ok := c.lookup(action.StateID)
	if !ok {
		return Rectangle{}, errors.New("state_id is unknown or expired; call computer_state again")
	}
	if record.windowID != action.WindowID || record.bounds != current {
		return Rectangle{}, errors.New("computer state is stale; call computer_state again")
	}
	return current, nil
}

func (c *nativeController) remember(id string, record stateRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.states) >= 64 {
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

func selectedBounds(windowID int64) (Rectangle, string, error) {
	if windowID != 0 {
		bounds, ok := windowBounds(uintptr(windowID))
		if !ok {
			return Rectangle{}, "", errors.New("window_id is not an available window")
		}
		return bounds, windowText(uintptr(windowID)), nil
	}
	displays := screenshot.NumActiveDisplays()
	if displays < 1 {
		return Rectangle{}, "", errors.New("no active display was found")
	}
	bounds := screenshot.GetDisplayBounds(0)
	for display := 1; display < displays; display++ {
		bounds = bounds.Union(screenshot.GetDisplayBounds(display))
	}
	return Rectangle{X: bounds.Min.X, Y: bounds.Min.Y, Width: bounds.Dx(), Height: bounds.Dy()}, "", nil
}

func windowBounds(handle uintptr) (Rectangle, bool) {
	value := winRect{}
	ok, _, _ := getWindowRect.Call(handle, uintptr(unsafe.Pointer(&value)))
	if ok == 0 {
		return Rectangle{}, false
	}
	return Rectangle{X: int(value.Left), Y: int(value.Top), Width: int(value.Right - value.Left), Height: int(value.Bottom - value.Top)}, true
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

func className(handle uintptr) string {
	buffer := make([]uint16, 256)
	written, _, _ := getClassName.Call(handle, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	if written == 0 {
		return "control"
	}
	return strings.ToLower(syscall.UTF16ToString(buffer[:written]))
}

func enumerateControls(windowID uintptr, parent Rectangle) AccessibilityState {
	result := AccessibilityState{Elements: []Element{}}
	if windowID == 0 {
		return result
	}
	callback := syscall.NewCallback(func(handle uintptr, _ uintptr) uintptr {
		if len(result.Elements) >= maxElements {
			result.Truncated = true
			return 0
		}
		bounds, ok := windowBounds(handle)
		if !ok || bounds.Width <= 0 || bounds.Height <= 0 {
			return 1
		}
		visible, _, _ := isWindowVisible.Call(handle)
		if visible == 0 {
			return 1
		}
		enabled, _, _ := isWindowEnabled.Call(handle)
		ref := fmt.Sprintf("e%d", len(result.Elements)+1)
		result.Elements = append(result.Elements, Element{
			Ref: ref, Role: className(handle), Name: windowText(handle), Enabled: enabled != 0,
			Bounds: Rectangle{X: bounds.X - parent.X, Y: bounds.Y - parent.Y, Width: bounds.Width, Height: bounds.Height},
		})
		return 1
	})
	_, _, _ = enumChildWindows.Call(windowID, callback, 0)
	return result
}

func activateWindow(handle uintptr) error {
	const restore = 9
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
	return nil
}

func (c *nativeController) act(action Action) error {
	switch action.Kind {
	case "move":
		return moveCursor(action.X, action.Y)
	case "click", "double_click":
		if err := moveCursor(action.X, action.Y); err != nil {
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
			mouseEvent.Call(uintptr(down), 0, 0, 0, 0)
			mouseEvent.Call(uintptr(up), 0, 0, 0, 0)
		}
	case "drag":
		if err := moveCursor(action.X, action.Y); err != nil {
			return err
		}
		mouseEvent.Call(mouseLeftDown, 0, 0, 0, 0)
		for step := 1; step <= 20; step++ {
			x := action.X + (action.ToX-action.X)*step/20
			y := action.Y + (action.ToY-action.Y)*step/20
			if err := moveCursor(x, y); err != nil {
				mouseEvent.Call(mouseLeftUp, 0, 0, 0, 0)
				return err
			}
			time.Sleep(10 * time.Millisecond)
		}
		mouseEvent.Call(mouseLeftUp, 0, 0, 0, 0)
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
		if action.X != 0 || action.Y != 0 {
			if err := moveCursor(action.X, action.Y); err != nil {
				return err
			}
		}
		if action.ScrollY != 0 {
			mouseEvent.Call(mouseWheel, 0, 0, uintptr(uint32(int32(-action.ScrollY))), 0)
		}
		if action.ScrollX != 0 {
			mouseEvent.Call(mouseHWheel, 0, 0, uintptr(uint32(int32(action.ScrollX))), 0)
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

func typeUnicode(text string) error {
	for _, code := range utf16.Encode([]rune(text)) {
		if err := sendUnicode(code, false); err != nil {
			return err
		}
		if err := sendUnicode(code, true); err != nil {
			return err
		}
	}
	return nil
}

func sendUnicode(code uint16, release bool) error {
	flags := uint32(keyUnicode)
	if release {
		flags |= keyUp
	}
	keyboard := keyboardInput{ScanCode: code, Flags: flags}
	entry := input{Type: inputKeyboard}
	raw := unsafe.Slice((*byte)(unsafe.Pointer(&keyboard)), int(unsafe.Sizeof(keyboard)))
	copy(entry.Data[:], raw)
	written, _, callErr := sendInput.Call(1, uintptr(unsafe.Pointer(&entry)), unsafe.Sizeof(entry))
	if written != 1 {
		return fmt.Errorf("SendInput failed: %w", callErr)
	}
	return nil
}

func pressCombination(value string) error {
	parts := strings.Split(strings.ToUpper(strings.TrimSpace(value)), "+")
	keys := make([]byte, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		key, ok := virtualKeys[part]
		if !ok && len(part) == 1 {
			character := part[0]
			if (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') {
				key, ok = character, true
			}
		}
		if !ok {
			return fmt.Errorf("unsupported key %q", part)
		}
		keys = append(keys, key)
	}
	for _, key := range keys {
		keybdEvent.Call(uintptr(key), 0, 0, 0)
	}
	for index := len(keys) - 1; index >= 0; index-- {
		keybdEvent.Call(uintptr(keys[index]), 0, keyUp, 0)
	}
	return nil
}

var virtualKeys = map[string]byte{
	"BACKSPACE": 0x08, "TAB": 0x09, "ENTER": 0x0D, "SHIFT": 0x10, "CTRL": 0x11,
	"CONTROL": 0x11, "ALT": 0x12, "ESC": 0x1B, "ESCAPE": 0x1B, "SPACE": 0x20,
	"PAGEUP": 0x21, "PAGEDOWN": 0x22, "END": 0x23, "HOME": 0x24, "LEFT": 0x25,
	"UP": 0x26, "RIGHT": 0x27, "DOWN": 0x28, "DELETE": 0x2E, "META": 0x5B,
	"WIN": 0x5B, "F1": 0x70, "F2": 0x71, "F3": 0x72, "F4": 0x73, "F5": 0x74,
	"F6": 0x75, "F7": 0x76, "F8": 0x77, "F9": 0x78, "F10": 0x79, "F11": 0x7A, "F12": 0x7B,
}
