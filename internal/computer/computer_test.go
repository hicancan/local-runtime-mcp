package computer

import "testing"

func TestValidate(t *testing.T) {
	valid := []Action{{Kind: "move", X: 1, Y: 2}, {Kind: "click", Button: "left", ClickCount: 2}, {Kind: "drag", X: 1, Y: 2, ToX: 3, ToY: 4}, {Kind: "type", Text: "hello"}, {Kind: "key", Key: "CTRL+L"}, {Kind: "scroll", ScrollY: 120}}
	for _, action := range valid {
		if err := Validate(action); err != nil {
			t.Errorf("%+v: %v", action, err)
		}
	}
	invalid := []Action{{Kind: "click", Button: "fourth"}, {Kind: "type"}, {Kind: "key"}, {Kind: "scroll"}, {Kind: "unknown"}}
	for _, action := range invalid {
		if err := Validate(action); err == nil {
			t.Errorf("expected %+v to fail", action)
		}
	}
}
