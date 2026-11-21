package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hicancan/local-runtime-mcp/internal/config"
)

func TestLocalAndClientSetupPersistWithoutRotatingOrLeaking(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "lrmcp.exe")
	path := filepath.Join(root, config.FileName)

	var out, errBuf bytes.Buffer
	if err := run(context.Background(), []string{"setup", "local"}, strings.NewReader(""), &out, &errBuf, executable); err != nil {
		t.Fatal(err)
	}
	stored, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Local.Token) != 64 || stored.Local.Listen != config.DefaultLocal {
		t.Fatalf("unexpected local setup config: %+v", stored.Local)
	}
	if strings.Contains(out.String(), stored.Local.Token) {
		t.Fatal("setup local leaked local token to stdout")
	}
	// Running setup local again keeps the existing token.
	out.Reset()
	if err := run(context.Background(), []string{"setup", "local"}, strings.NewReader(""), &out, &errBuf, executable); err != nil {
		t.Fatal(err)
	}
	again, _ := config.Load(path)
	if again.Local.Token != stored.Local.Token {
		t.Fatal("setup local silently rotated token")
	}

	invoke := func(args ...string) map[string]any {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if err := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr, executable); err != nil {
			t.Fatalf("%v: %v (stderr=%s)", args, err, stderr.String())
		}
		var value map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}

	first := invoke("setup", "client", "--id", "studev-win", "--model", "deepseek-web")
	if first["client_id"] != "studev-win" || len(first["token"].(string)) != 64 || first["local_url"] != "http://"+config.DefaultLocal {
		t.Fatalf("unexpected client setup output: %+v", first)
	}
	second := invoke("setup", "client", "--id", "studev-win", "--model", "deepseek-web", "--browser-id", "edge-main")
	if first["token"] != second["token"] || second["browser_id"] != "edge-main" {
		t.Fatalf("setup client rotated token or lost browser_id: %+v", second)
	}
	before, _ := os.ReadFile(path)
	shown := invoke("setup", "client", "--show", "--id", "studev-win")
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || !reflect.DeepEqual(shown, second) {
		t.Fatalf("setup client --show mutated file or differed: %+v vs %+v", shown, second)
	}
}
