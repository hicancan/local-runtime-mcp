package image

import (
	"bytes"
	"context"
	"errors"
	stdimage "image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadImageBounds(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, stdimage.NewRGBA(stdimage.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(path, encoded.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	data, result, err := Read(context.Background(), path, ReadOptions{MaxPixels: 12})
	if err != nil || !bytes.Equal(data, encoded.Bytes()) || result.Width != 4 || result.Height != 3 || result.MIMEType != "image/png" {
		t.Fatalf("read = %+v bytes=%d err=%v", result, len(data), err)
	}
	if _, _, err := Read(context.Background(), path, ReadOptions{MaxPixels: 11}); err == nil {
		t.Fatal("pixel limit was not enforced")
	}
	if _, _, err := Read(context.Background(), path, ReadOptions{MaxBytes: len(encoded.Bytes()) - 1}); err == nil {
		t.Fatal("byte limit was not enforced")
	}
	projected, projectedResult, err := Read(context.Background(), path, ReadOptions{MaxPixels: 12, CropX: 1, CropY: 1, CropWidth: 2, CropHeight: 2, MaxWidth: 1})
	if err != nil || !projectedResult.Transformed || projectedResult.Width != 1 || projectedResult.Height != 1 || projectedResult.MIMEType != "image/png" {
		t.Fatalf("projection = %+v bytes=%d err=%v", projectedResult, len(projected), err)
	}
}

func TestBoundedReaderRejectsGrowingSource(t *testing.T) {
	// A reader that was initially tiny later supplies more bytes. The read itself,
	// rather than a prior size check, must enforce the configured budget.
	reader := io.MultiReader(bytes.NewReader([]byte("small")), bytes.NewReader(bytes.Repeat([]byte("x"), 4096)))
	if data, err := readBounded(reader, 32); err == nil || data != nil {
		t.Fatalf("growing read returned %d bytes, %v", len(data), err)
	}
}

func TestCanceledImageQueueDoesNotOpenFile(t *testing.T) {
	imageReaders <- struct{}{}
	imageReaders <- struct{}{}
	defer func() { <-imageReaders; <-imageReaders }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, _, err := Read(ctx, "missing.png", ReadOptions{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled read = %v", err)
	}
}

func TestProjectionBufferBoundedWhileEncoding(t *testing.T) {
	buffer := boundedBuffer{limit: 3}
	if _, err := buffer.Write([]byte("abcd")); err == nil || buffer.Len() != 0 {
		t.Fatalf("encoded buffer exceeded budget: %d, %v", buffer.Len(), err)
	}
}
