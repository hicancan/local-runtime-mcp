package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTextLifecycleAndCompareAndSwap(t *testing.T) {
	service := New()
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "nested", "note.txt")
	if _, err := service.WriteText(ctx, path, "one\n", WriteTextOptions{Mode: "create"}); err == nil {
		t.Fatal("write unexpectedly created missing parents")
	}
	written, err := service.WriteText(ctx, path, "one\ntwo\nthree\n", WriteTextOptions{Mode: "create", CreateParents: true})
	if err != nil || !written.Created || len(written.SHA256) != 64 {
		t.Fatalf("initial write = %+v, %v", written, err)
	}
	read, err := ReadText(path, ReadTextOptions{StartLine: 2, LineCount: 1, WithSHA256: true})
	if err != nil || read.Content != "two\n" || read.StartLine != 2 || read.EndLine != 2 || read.NextLine != 3 || !read.Truncated || read.SHA256 != written.SHA256 {
		t.Fatalf("range read = %+v, %v", read, err)
	}
	if _, err := service.WriteText(ctx, path, "bad", WriteTextOptions{Mode: "replace", ExpectedSHA256: strings.Repeat("0", 64)}); err == nil {
		t.Fatal("write with a stale hash succeeded")
	}
	unchanged, _ := os.ReadFile(path)
	if string(unchanged) != "one\ntwo\nthree\n" {
		t.Fatalf("stale write changed file: %q", unchanged)
	}
	edited, err := service.PatchText(ctx, path, PatchTextOptions{
		ExpectedSHA256: written.SHA256,
		Hunks:          []TextPatchHunk{{Old: "one", New: "ONE", After: "\n"}, {Before: "one\n", Old: "two", New: "TWO", After: "\nthree"}},
	})
	if err != nil || edited.Hunks != 2 {
		t.Fatalf("batch edit = %+v, %v", edited, err)
	}
	if _, err := service.PatchText(ctx, path, PatchTextOptions{ExpectedSHA256: edited.SHA256, Hunks: []TextPatchHunk{{Old: "ONE", New: "changed", After: "\n"}, {Old: "missing", New: "x", After: "\n"}}}); err == nil {
		t.Fatal("partially invalid batch edit succeeded")
	}
	unchanged, _ = os.ReadFile(path)
	if string(unchanged) != "ONE\nTWO\nthree\n" {
		t.Fatalf("failed batch edit was not atomic: %q", unchanged)
	}
}

func TestMutationsRejectFinalSymlink(t *testing.T) {
	service := New()
	ctx := context.Background()
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	link := filepath.Join(root, "link.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if _, err := service.WriteText(ctx, link, "changed", WriteTextOptions{Mode: "replace", ExpectedSHA256: hashBytes([]byte("original"))}); err == nil {
		t.Fatal("write followed a final symbolic link")
	}
	hash, _ := hashFile(target)
	if _, err := service.PatchText(ctx, link, PatchTextOptions{ExpectedSHA256: hash, Hunks: []TextPatchHunk{{Old: "original", New: "changed"}}}); err == nil {
		t.Fatal("edit followed a final symbolic link")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "original" {
		t.Fatalf("symlink mutation changed target: %q, %v", data, err)
	}
}

func TestPatchUsesOneImmutableBaseAndRejectsAmbiguity(t *testing.T) {
	service := New()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "patch.txt")
	data := "alpha\nbeta\ngamma\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := service.PatchText(ctx, path, PatchTextOptions{ExpectedSHA256: hashBytes([]byte(data)), Hunks: []TextPatchHunk{
		{Before: "alpha\n", Old: "beta", New: "BETA", After: "\ngamma"},
		{Before: "gamma", Old: "", New: "!", After: "\n"},
	}})
	if err != nil || result.Hunks != 2 {
		t.Fatalf("patch = %+v, %v", result, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "alpha\nBETA\ngamma!\n" {
		t.Fatalf("patched content = %q", got)
	}
	if _, err := service.PatchText(ctx, path, PatchTextOptions{ExpectedSHA256: result.SHA256, Hunks: []TextPatchHunk{{Old: "a", New: "x"}}}); err == nil {
		t.Fatal("ambiguous patch succeeded")
	}
	if _, err := service.PatchText(ctx, path, PatchTextOptions{Hunks: []TextPatchHunk{{Old: "alpha", New: "x"}}}); err == nil {
		t.Fatal("patch without expected_sha256 succeeded")
	}
}

func TestListAndSearchAreBounded(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		"a.txt": "needle here\n", "b.txt": "Needle again\n", ".hidden.txt": "needle hidden\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := List(root, 1, false, 1)
	if err != nil || len(listed.Entries) != 1 || !listed.Truncated {
		t.Fatalf("bounded list = %+v, %v", listed, err)
	}
	searched, err := SearchText(filepath.Join(root, "a.txt"), "needle", false, true, false, 10)
	if err != nil || len(searched.Matches) != 1 || searched.Matches[0].Line != 1 || searched.Matches[0].Column != 1 {
		t.Fatalf("direct-file search = %+v, %v", searched, err)
	}
	searched, err = SearchText(root, "needle", false, false, false, 1)
	if err != nil || len(searched.Matches) != 1 || !searched.Truncated {
		t.Fatalf("bounded search = %+v, %v", searched, err)
	}
}
