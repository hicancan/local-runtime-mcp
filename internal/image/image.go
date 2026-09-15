package image

import (
	"bytes"
	"errors"
	"fmt"
	stdimage "image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"

	_ "golang.org/x/image/webp"
)

const defaultImageBytes = 10 << 20

type ReadResult struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	MIMEType string `json:"mime_type"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

func Read(path string, maxBytes int) ([]byte, ReadResult, error) {
	if strings.TrimSpace(path) == "" {
		return nil, ReadResult{}, errors.New("path cannot be empty")
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return nil, ReadResult{}, err
	}
	resolved = filepath.Clean(resolved)
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, ReadResult{}, err
	}
	if !info.Mode().IsRegular() {
		return nil, ReadResult{}, errors.New("image path must be a regular file")
	}
	if maxBytes == 0 {
		maxBytes = defaultImageBytes
	}
	if maxBytes < 1 || maxBytes > 64<<20 {
		return nil, ReadResult{}, errors.New("max_bytes must be between 1 and 67108864")
	}
	if info.Size() > int64(maxBytes) {
		return nil, ReadResult{}, fmt.Errorf("image is %d bytes, exceeding max_bytes %d", info.Size(), maxBytes)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, ReadResult{}, err
	}
	configuration, format, err := stdimage.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, ReadResult{}, errors.New("file is not a supported PNG, JPEG, GIF, or WebP image")
	}
	mimeTypes := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp"}
	mimeType, ok := mimeTypes[strings.ToLower(format)]
	if !ok {
		return nil, ReadResult{}, fmt.Errorf("unsupported image format %q", format)
	}
	return data, ReadResult{Path: resolved, Size: info.Size(), MIMEType: mimeType, Width: configuration.Width, Height: configuration.Height}, nil
}
