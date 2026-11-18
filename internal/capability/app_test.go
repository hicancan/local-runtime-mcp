package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalAppFixtureProgram(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "report":
		fmt.Print(`{"state":"found","name":"Fixture class"}`)
	case "invalid":
		fmt.Print(`{"state":"found","name":"Fixture class","extra":true}`)
	case "large":
		fmt.Print(strings.Repeat("X", 64<<11))
	case "slow":
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func TestAppCallsOnlyFixedCommandAndValidatesItsOutput(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, mode := range []string{"report", "invalid", "large", "slow"} {
		path := filepath.Join(root, "app", mode+".json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		binding, _ := json.Marshal(appCommand{Executable: executable, Args: []string{"-test.run=^TestLocalAppFixtureProgram$", "--", mode}})
		body := fmt.Sprintf(`{
			"description":"Read an app fixture",
			"command":%s,
			"inputSchema":{"type":"object","additionalProperties":false},
			"outputSchema":{"type":"object","properties":{"state":{"const":"found"},"name":{"type":"string"}},"required":["state","name"],"additionalProperties":false},
			"annotations":{"readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false}
		}`, binding)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := Load(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.AppDefinitions()) != 4 || len(registry.List()) != 0 {
		t.Fatal("app definitions appeared in the browser catalog")
	}
	if _, err := registry.Call(context.Background(), "app.report", "browser", "tab", map[string]any{}); err == nil {
		t.Fatal("browser route accepted an app capability")
	}
	if _, err := registry.CallApp(context.Background(), "app.report", map[string]any{"args": []string{"injected"}}); err == nil {
		t.Fatal("caller-supplied process arguments were accepted")
	}
	value, err := registry.CallApp(context.Background(), "app.report", map[string]any{})
	if err != nil || value["name"] != "Fixture class" {
		t.Fatalf("fixed executable failed: %v", err)
	}
	for _, mode := range []string{"invalid", "large"} {
		if _, err := registry.CallApp(context.Background(), "app."+mode, map[string]any{}); err == nil {
			t.Fatalf("%s app output was accepted", mode)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, err := registry.CallApp(ctx, "app.slow", map[string]any{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call did not honor deadline: %v", err)
	}
}
