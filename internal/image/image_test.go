package image

import (
	"bytes"
	stdimage "image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
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
	data, result, err := Read(path, ReadOptions{MaxPixels: 12})
	if err != nil || !bytes.Equal(data, encoded.Bytes()) || result.Width != 4 || result.Height != 3 || result.MIMEType != "image/png" {
		t.Fatalf("read = %+v bytes=%d err=%v", result, len(data), err)
	}
	if _, _, err := Read(path, ReadOptions{MaxPixels: 11}); err == nil {
		t.Fatal("pixel limit was not enforced")
	}
	if _, _, err := Read(path, ReadOptions{MaxBytes: len(encoded.Bytes()) - 1}); err == nil {
		t.Fatal("byte limit was not enforced")
	}
	projected, projectedResult, err := Read(path, ReadOptions{MaxPixels: 12, CropX: 1, CropY: 1, CropWidth: 2, CropHeight: 2, MaxWidth: 1})
	if err != nil || !projectedResult.Transformed || projectedResult.Width != 1 || projectedResult.Height != 1 || projectedResult.MIMEType != "image/png" {
		t.Fatalf("projection = %+v bytes=%d err=%v", projectedResult, len(projected), err)
	}
}
