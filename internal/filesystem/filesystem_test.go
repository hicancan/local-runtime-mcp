package filesystem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTextLifecycleAndCompareAndSwap(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "note.txt")
	if _, err := WriteText(path, "one\n", WriteTextOptions{CreateOnly: true}); err == nil {
		t.Fatal("write unexpectedly created missing parents")
	}
	written, err := WriteText(path, "one\ntwo\nthree\n", WriteTextOptions{CreateOnly: true, CreateParents: true})
	if err != nil || !written.Created || len(written.SHA256) != 64 {
		t.Fatalf("initial write = %+v, %v", written, err)
	}
	read, err := ReadText(path, ReadTextOptions{StartLine: 2, LineCount: 1, WithSHA256: true})
	if err != nil || read.Content != "two\n" || read.StartLine != 2 || read.EndLine != 2 || read.NextLine != 3 || !read.Truncated || read.SHA256 != written.SHA256 {
		t.Fatalf("range read = %+v, %v", read, err)
	}
	if _, err := WriteText(path, "bad", WriteTextOptions{ExpectedSHA256: strings.Repeat("0", 64)}); err == nil {
		t.Fatal("write with a stale hash succeeded")
	}
	unchanged, _ := os.ReadFile(path)
	if string(unchanged) != "one\ntwo\nthree\n" {
		t.Fatalf("stale write changed file: %q", unchanged)
	}
	edited, err := EditText(path, EditTextOptions{
		ExpectedSHA256: written.SHA256,
		Edits:          []TextEdit{{OldText: "one", NewText: "ONE"}, {OldText: "two", NewText: "TWO"}},
	})
	if err != nil || len(edited.Replacements) != 2 || edited.Replacements[0] != 1 || edited.Replacements[1] != 1 {
		t.Fatalf("batch edit = %+v, %v", edited, err)
	}
	if _, err := EditText(path, EditTextOptions{Edits: []TextEdit{{OldText: "ONE", NewText: "changed"}, {OldText: "missing", NewText: "x"}}}); err == nil {
		t.Fatal("partially invalid batch edit succeeded")
	}
	unchanged, _ = os.ReadFile(path)
	if string(unchanged) != "ONE\nTWO\nthree\n" {
		t.Fatalf("failed batch edit was not atomic: %q", unchanged)
	}
}

func TestMutationsRejectFinalSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	link := filepath.Join(root, "link.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if _, err := WriteText(link, "changed", WriteTextOptions{}); err == nil {
		t.Fatal("write followed a final symbolic link")
	}
	if _, err := EditText(link, EditTextOptions{Edits: []TextEdit{{OldText: "original", NewText: "changed"}}}); err == nil {
		t.Fatal("edit followed a final symbolic link")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "original" {
		t.Fatalf("symlink mutation changed target: %q, %v", data, err)
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
