package filesystem

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type virtualLineReader struct {
	remaining int64
	read      int64
}

func (r *virtualLineReader) Read(output []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	size := min(int64(len(output)), r.remaining)
	for index := range int(size) {
		output[index] = 'x'
	}
	r.remaining -= size
	r.read += size
	return int(size), nil
}

func TestSelectedHugeLineReadStopsAtBudget(t *testing.T) {
	source := &virtualLineReader{remaining: 1 << 30}
	reader := bufio.NewReaderSize(source, 4096)
	const limit = 8192
	data, present, err := readTextLine(reader, limit, true)
	if !present || !errors.Is(err, errTextLineLimit) {
		t.Fatalf("large line = %d bytes, present=%v, %v", len(data), present, err)
	}
	if len(data) > limit || cap(data) > limit || source.read > limit+int64(reader.Size()) {
		t.Fatalf("budget exceeded: len=%d cap=%d read=%d", len(data), cap(data), source.read)
	}
}

func TestSkippedHugeLineDoesNotRetainContent(t *testing.T) {
	source := &virtualLineReader{remaining: 4 << 20}
	reader := bufio.NewReaderSize(io.MultiReader(source, bytes.NewBufferString("\nnext\n")), 4096)
	data, present, err := readTextLine(reader, 32, false)
	if err != nil || !present || len(data) != 0 || cap(data) != 0 {
		t.Fatalf("skipped line retained %d bytes, %v", cap(data), err)
	}
	data, present, err = readTextLine(reader, 32, true)
	if err != nil || !present || string(data) != "next\n" {
		t.Fatalf("next line = %q, %v", data, err)
	}
}

func TestReadTextCompleteLinesAndWholeFileHashWithOversizedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large-line.txt")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("first\n")
	_, _ = io.Copy(file, &virtualLineReader{remaining: 4 << 20})
	_, _ = file.WriteString("\nlast\n")
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	hash, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	read, err := ReadText(path, ReadTextOptions{MaxBytes: 32, WithSHA256: true})
	if err != nil || read.Content != "first\n" || !read.Truncated || read.NextLine != 2 || read.EndLine != 1 || read.SHA256 != hash {
		t.Fatalf("bounded complete read = %+v, %v", read, err)
	}
	if _, err := ReadText(path, ReadTextOptions{StartLine: 2, MaxBytes: 32}); err == nil {
		t.Fatal("oversized first selected line accepted")
	}
	last, err := ReadText(path, ReadTextOptions{StartLine: 3, MaxBytes: 32, WithSHA256: true})
	if err != nil || last.Content != "last\n" || last.SHA256 != hash || last.Truncated {
		t.Fatalf("skip read = %+v, %v", last, err)
	}
}

func TestReadTextUTF8BoundaryAndExactBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "utf8.txt")
	text := []byte("中文\n下一行\n")
	if err := os.WriteFile(path, text, 0o644); err != nil {
		t.Fatal(err)
	}
	read, err := ReadText(path, ReadTextOptions{MaxBytes: len([]byte("中文\n"))})
	if err != nil || read.Content != "中文\n" || !read.Truncated || read.NextLine != 2 {
		t.Fatalf("UTF8 complete line = %+v, %v", read, err)
	}
	if _, err := ReadText(path, ReadTextOptions{MaxBytes: 5}); err == nil {
		t.Fatal("partial UTF8 line returned")
	}
}
