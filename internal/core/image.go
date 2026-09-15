package core

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"strings"

	_ "golang.org/x/image/webp"
)

const defaultImageBytes = 10 << 20

func (r *Runtime) ReadImage(rootName, path string, maxBytes int) ([]byte, ImageReadResult, error) {
	root, err := r.Root(rootName)
	if err != nil {
		return nil, ImageReadResult{}, err
	}
	resolved, err := r.Resolve(rootName, path)
	if err != nil {
		return nil, ImageReadResult{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, ImageReadResult{}, err
	}
	if !info.Mode().IsRegular() {
		return nil, ImageReadResult{}, errors.New("image path must be a regular file")
	}
	if maxBytes == 0 {
		maxBytes = defaultImageBytes
	}
	if maxBytes < 1 || maxBytes > 64<<20 {
		return nil, ImageReadResult{}, errors.New("max_bytes must be between 1 and 67108864")
	}
	if info.Size() > int64(maxBytes) {
		return nil, ImageReadResult{}, fmt.Errorf("image is %d bytes, exceeding max_bytes %d", info.Size(), maxBytes)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, ImageReadResult{}, err
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, ImageReadResult{}, errors.New("file is not a supported PNG, JPEG, GIF, or WebP image")
	}
	mimeTypes := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp"}
	mimeType, ok := mimeTypes[strings.ToLower(format)]
	if !ok {
		return nil, ImageReadResult{}, fmt.Errorf("unsupported image format %q", format)
	}
	return data, ImageReadResult{Path: relativeSlash(root.Path, resolved), Size: info.Size(), MIMEType: mimeType, Width: config.Width, Height: config.Height, SHA256: digest(data)}, nil
}
