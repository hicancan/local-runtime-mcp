//go:build windows

package computer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"runtime"
	"strings"
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
)

var (
	user32             = syscall.NewLazyDLL("user32.dll")
	getCursorPos       = user32.NewProc("GetCursorPos")
	setCursorPos       = user32.NewProc("SetCursorPos")
	mouseEvent         = user32.NewProc("mouse_event")
	keybdEvent         = user32.NewProc("keybd_event")
	sendInput          = user32.NewProc("SendInput")
	setProcessDPIAware = user32.NewProc("SetProcessDPIAware")
	openInputDesktop   = user32.NewProc("OpenInputDesktop")
	setThreadDesktop   = user32.NewProc("SetThreadDesktop")
	getThreadDesktop   = user32.NewProc("GetThreadDesktop")
	closeDesktop       = user32.NewProc("CloseDesktop")
	getCurrentThreadID = syscall.NewLazyDLL("kernel32.dll").NewProc("GetCurrentThreadId")
)

type nativeController struct{}

type point struct{ X, Y int32 }

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
	return &nativeController{}
}

func (*nativeController) Backend() string { return "windows-native" }

func (c *nativeController) Screenshot(context.Context) ([]byte, ScreenshotInfo, error) {
	var data []byte
	var metadata ScreenshotInfo
	err := onInputDesktop(func() error {
		var err error
		data, metadata, err = c.capture()
		return err
	})
	return data, metadata, err
}

func (c *nativeController) capture() ([]byte, ScreenshotInfo, error) {
	displays := screenshot.NumActiveDisplays()
	if displays < 1 {
		return nil, ScreenshotInfo{}, errors.New("no active display was found")
	}
	bounds := screenshot.GetDisplayBounds(0)
	for display := 1; display < displays; display++ {
		bounds = bounds.Union(screenshot.GetDisplayBounds(display))
	}
	frame, err := screenshot.CaptureRect(bounds)
	if err != nil {
		return nil, ScreenshotInfo{}, fmt.Errorf("capture virtual desktop %v: %w", bounds, err)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, frame); err != nil {
		return nil, ScreenshotInfo{}, err
	}
	cursor := point{}
	_, _, _ = getCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))
	return encoded.Bytes(), ScreenshotInfo{
		Backend: c.Backend(), OriginX: bounds.Min.X, OriginY: bounds.Min.Y,
		Width: bounds.Dx(), Height: bounds.Dy(), CursorX: int(cursor.X), CursorY: int(cursor.Y), MIMEType: "image/png",
	}, nil
}

func (c *nativeController) Act(_ context.Context, action Action) (ActionResult, error) {
	if err := Validate(action); err != nil {
		return ActionResult{}, err
	}
	if err := onInputDesktop(func() error { return c.act(action) }); err != nil {
		return ActionResult{}, err
	}
	return ActionResult{Backend: c.Backend(), Kind: action.Kind, Success: true}, nil
}

func (c *nativeController) act(action Action) error {
	switch action.Kind {
	case "move":
		if err := moveCursor(action.X, action.Y); err != nil {
			return err
		}
	case "click":
		if err := moveCursor(action.X, action.Y); err != nil {
			return err
		}
		count := action.ClickCount
		if count == 0 {
			count = 1
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
	case "type":
		if err := typeUnicode(action.Text); err != nil {
			return err
		}
	case "key":
		if err := pressCombination(action.Key); err != nil {
			return err
		}
	case "scroll":
		if action.ScrollY != 0 {
			mouseEvent.Call(mouseWheel, 0, 0, uintptr(uint32(int32(action.ScrollY))), 0)
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
	const access = 0x0001 | 0x0080 | 0x0100
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
