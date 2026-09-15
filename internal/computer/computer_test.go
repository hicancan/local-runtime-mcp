package computer

import "testing"

func TestValidate(t *testing.T) {
	x1, y2, x3, y4 := 1, 2, 3, 4
	valid := []Action{{Kind: "activate", WindowID: 1}, {Kind: "move", StateID: "s1", X: &x1, Y: &y2}, {Kind: "click", StateID: "s1", X: &x1, Y: &y2, Button: "left"}, {Kind: "double_click", StateID: "s1", X: &x1, Y: &y2}, {Kind: "drag", StateID: "s1", X: &x1, Y: &y2, ToX: &x3, ToY: &y4}, {Kind: "type_text", StateID: "s1", Text: "hello"}, {Kind: "set_value", StateID: "s1", Text: "hello"}, {Kind: "press_key", StateID: "s1", Key: "CTRL+L"}, {Kind: "scroll", StateID: "s1", ScrollY: 120}}
	for _, action := range valid {
		if err := Validate(action); err != nil {
			t.Errorf("%+v: %v", action, err)
		}
	}
	invalid := []Action{{Kind: "activate"}, {Kind: "activate", WindowID: 1, StateID: "s1"}, {Kind: "click", StateID: "s1", X: &x1, Y: &y2, Button: "fourth"}, {Kind: "click", StateID: "s1"}, {Kind: "type_text", StateID: "s1"}, {Kind: "set_value", StateID: "s1"}, {Kind: "press_key", StateID: "s1"}, {Kind: "scroll", StateID: "s1"}, {Kind: "move", StateID: "s1", WindowID: -1, X: &x1, Y: &y2}, {Kind: "unknown", StateID: "s1"}}
	for _, action := range invalid {
		if err := Validate(action); err == nil {
			t.Errorf("expected %+v to fail", action)
		}
	}
}
