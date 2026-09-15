package computer

import "testing"

func TestValidate(t *testing.T) {
	valid := []Action{{Kind: "activate", WindowID: 1}, {Kind: "move", X: 1, Y: 2}, {Kind: "click", Button: "left"}, {Kind: "double_click"}, {Kind: "drag", X: 1, Y: 2, ToX: 3, ToY: 4}, {Kind: "type_text", Text: "hello"}, {Kind: "set_value", Text: "hello"}, {Kind: "press_key", Key: "CTRL+L"}, {Kind: "scroll", ScrollY: 120}, {Kind: "click", StateID: "s1", ElementRef: "e1"}}
	for _, action := range valid {
		if err := Validate(action); err != nil {
			t.Errorf("%+v: %v", action, err)
		}
	}
	invalid := []Action{{Kind: "click", Button: "fourth"}, {Kind: "type_text"}, {Kind: "set_value"}, {Kind: "press_key"}, {Kind: "scroll"}, {Kind: "click", ElementRef: "e1"}, {Kind: "move", WindowID: -1}, {Kind: "unknown"}}
	for _, action := range invalid {
		if err := Validate(action); err == nil {
			t.Errorf("expected %+v to fail", action)
		}
	}
}
