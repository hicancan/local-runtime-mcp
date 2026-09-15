package core

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/config"
)

func testRuntime(t *testing.T) (*Runtime, string) {
	t.Helper()
	root := t.TempDir()
	runtime := New(config.Config{Roots: map[string]config.Root{"test": {Path: root}}})
	canonical, err := runtime.Root("test")
	if err != nil {
		t.Fatal(err)
	}
	return runtime, canonical.Path
}

func TestResolveRejectsRootEscape(t *testing.T) {
	runtime, root := testRuntime(t)
	if _, err := runtime.Resolve("test", filepath.Join("..", "outside.txt")); err == nil {
		t.Fatal("expected parent traversal to fail")
	}
	if _, err := runtime.Resolve("test", filepath.VolumeName(root)+string(filepath.Separator)); err == nil {
		t.Fatal("expected absolute path to fail")
	}

	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	if _, err := runtime.Resolve("test", filepath.Join("escape", "file.txt")); err == nil {
		t.Fatal("expected symbolic-link escape to fail")
	}
}

func TestTextLifecycleAndSearch(t *testing.T) {
	runtime, _ := testRuntime(t)
	written, err := runtime.WriteText("test", "docs/note.txt", "alpha beta\nalpha gamma\n", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !written.Created || written.SHA256 == "" {
		t.Fatalf("unexpected write result: %+v", written)
	}
	if _, err := runtime.WriteText("test", "docs/note.txt", "changed", "", true); err == nil {
		t.Fatal("create-only write should reject an existing file")
	}
	read, err := runtime.ReadText("test", "docs/note.txt", 5)
	if err != nil {
		t.Fatal(err)
	}
	if read.Content != "alpha" || !read.Truncated || read.SHA256 != written.SHA256 {
		t.Fatalf("unexpected read result: %+v", read)
	}
	if _, err := runtime.EditText("test", "docs/note.txt", "alpha", "delta", written.SHA256, false); err == nil {
		t.Fatal("ambiguous edit should fail")
	}
	edited, err := runtime.EditText("test", "docs/note.txt", "alpha", "delta", written.SHA256, true)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Replacements != 2 || edited.PreviousSHA256 != written.SHA256 {
		t.Fatalf("unexpected edit: %+v", edited)
	}
	if _, err := runtime.WriteText("test", "docs/note.txt", "stale", written.SHA256, false); err == nil {
		t.Fatal("stale digest should be rejected")
	}
	matches, err := runtime.SearchText("test", "docs", `delta\s+g`, true, true, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Line != 2 || matches[0].Column != 1 {
		t.Fatalf("unexpected matches: %+v", matches)
	}
	entries, err := runtime.List("test", "", 4, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Path != "docs" || entries[1].Path != "docs/note.txt" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

func TestReadImage(t *testing.T) {
	runtime, root := testRuntime(t)
	frame := image.NewRGBA(image.Rect(0, 0, 4, 3))
	frame.Set(1, 1, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, frame); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "image.png"), encoded.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	data, info, err := runtime.ReadImage("test", "image.png", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, encoded.Bytes()) || info.Width != 4 || info.Height != 3 || info.MIMEType != "image/png" {
		t.Fatalf("unexpected image: %+v", info)
	}
	if _, _, err := runtime.ReadImage("test", "image.png", 1); err == nil {
		t.Fatal("expected max-size rejection")
	}
}

func TestRunProcess(t *testing.T) {
	runtime, root := testRuntime(t)
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base := ProcessOptions{
		Program:     executable,
		Args:        []string{"-test.run=TestProcessHelper", "--", "inspect"},
		Directory:   "sub",
		Environment: map[string]string{"LRMCP_HELPER": "inspect", "LRMCP_VALUE": "present"},
		Stdin:       "from stdin",
	}
	result, err := runtime.RunProcess(context.Background(), "test", base)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "from stdin|present|") || !strings.Contains(result.Stdout, filepath.Join(root, "sub")) {
		t.Fatalf("unexpected process result: %+v", result)
	}

	base.Environment["LRMCP_HELPER"] = "output"
	base.Args[len(base.Args)-1] = "output"
	base.MaxOutputBytes = 16
	result, err = runtime.RunProcess(context.Background(), "test", base)
	if err != nil {
		t.Fatal(err)
	}
	if !result.StdoutTruncated || len(result.Stdout) != 16 {
		t.Fatalf("output was not bounded: %+v", result)
	}

	base.Environment["LRMCP_HELPER"] = "sleep"
	base.Args[len(base.Args)-1] = "sleep"
	base.TimeoutSeconds = 1
	base.MaxOutputBytes = 0
	result, err = runtime.RunProcess(context.Background(), "test", base)
	if err != nil {
		t.Fatal(err)
	}
	if !result.TimedOut || result.ExitCode != -1 {
		t.Fatalf("timeout not reported: %+v", result)
	}
}

func TestProcessHelper(t *testing.T) {
	mode := os.Getenv("LRMCP_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "inspect":
		data, _ := os.ReadFile("/dev/stdin")
		if runtime.GOOS == "windows" {
			data, _ = ioReadAll(os.Stdin)
		}
		cwd, _ := os.Getwd()
		_, _ = os.Stdout.WriteString(string(data) + "|" + os.Getenv("LRMCP_VALUE") + "|" + cwd)
	case "output":
		_, _ = os.Stdout.WriteString(strings.Repeat("x", 1024))
	case "sleep":
		time.Sleep(3 * time.Second)
	}
	os.Exit(0)
}

func ioReadAll(reader *os.File) ([]byte, error) {
	var output bytes.Buffer
	_, err := output.ReadFrom(reader)
	return output.Bytes(), err
}
