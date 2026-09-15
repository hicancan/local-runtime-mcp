package image

import (
	"bytes"
	"errors"
	"fmt"
	stdimage "image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	defaultImageBytes  = 10 << 20
	defaultImagePixels = 40_000_000
	maxImagePixels     = 250_000_000
	maxDimension       = 32_768
)

type ReadOptions struct {
	MaxBytes   int
	MaxPixels  int
	CropX      int
	CropY      int
	CropWidth  int
	CropHeight int
	MaxWidth   int
	MaxHeight  int
}

type ReadResult struct {
	Path         string `json:"path"`
	SourceBytes  int64  `json:"source_bytes"`
	SourceWidth  int    `json:"source_width"`
	SourceHeight int    `json:"source_height"`
	MIMEType     string `json:"mime_type"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Transformed  bool   `json:"transformed"`
}

func Read(path string, options ReadOptions) ([]byte, ReadResult, error) {
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
	if options.MaxBytes == 0 {
		options.MaxBytes = defaultImageBytes
	}
	if options.MaxBytes < 1 || options.MaxBytes > 64<<20 {
		return nil, ReadResult{}, errors.New("max_bytes must be between 1 and 67108864")
	}
	if options.MaxPixels == 0 {
		options.MaxPixels = defaultImagePixels
	}
	if options.MaxPixels < 1 || options.MaxPixels > maxImagePixels {
		return nil, ReadResult{}, fmt.Errorf("max_pixels must be between 1 and %d", maxImagePixels)
	}
	if info.Size() > int64(options.MaxBytes) {
		return nil, ReadResult{}, fmt.Errorf("image is %d bytes, exceeding max_bytes %d", info.Size(), options.MaxBytes)
	}
	if options.CropX < 0 || options.CropY < 0 || options.CropWidth < 0 || options.CropHeight < 0 {
		return nil, ReadResult{}, errors.New("crop coordinates must be non-negative")
	}
	if (options.CropWidth == 0) != (options.CropHeight == 0) {
		return nil, ReadResult{}, errors.New("crop_width and crop_height must be provided together")
	}
	if options.CropWidth == 0 && (options.CropX != 0 || options.CropY != 0) {
		return nil, ReadResult{}, errors.New("crop_x and crop_y require crop_width and crop_height")
	}
	if options.MaxWidth < 0 || options.MaxHeight < 0 || options.MaxWidth > maxDimension || options.MaxHeight > maxDimension {
		return nil, ReadResult{}, fmt.Errorf("max_width and max_height must be between 0 and %d", maxDimension)
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
	if configuration.Width <= 0 || configuration.Height <= 0 || int64(configuration.Width)*int64(configuration.Height) > int64(options.MaxPixels) {
		return nil, ReadResult{}, fmt.Errorf("image dimensions %dx%d exceed max_pixels %d", configuration.Width, configuration.Height, options.MaxPixels)
	}
	metadata := ReadResult{Path: resolved, SourceBytes: info.Size(), SourceWidth: configuration.Width, SourceHeight: configuration.Height, MIMEType: mimeType, Width: configuration.Width, Height: configuration.Height}
	projection := options.CropWidth > 0 || options.MaxWidth > 0 || options.MaxHeight > 0
	if !projection {
		return data, metadata, nil
	}

	decoded, _, err := stdimage.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ReadResult{}, fmt.Errorf("decode image: %w", err)
	}
	sourceBounds := decoded.Bounds()
	crop := sourceBounds
	if options.CropWidth > 0 {
		crop = stdimage.Rect(sourceBounds.Min.X+options.CropX, sourceBounds.Min.Y+options.CropY, sourceBounds.Min.X+options.CropX+options.CropWidth, sourceBounds.Min.Y+options.CropY+options.CropHeight)
		if !crop.In(sourceBounds) {
			return nil, ReadResult{}, errors.New("crop rectangle is outside the source image")
		}
	}
	width, height := crop.Dx(), crop.Dy()
	scale := 1.0
	if options.MaxWidth > 0 && width > options.MaxWidth {
		scale = float64(options.MaxWidth) / float64(width)
	}
	if options.MaxHeight > 0 && float64(height)*scale > float64(options.MaxHeight) {
		scale = float64(options.MaxHeight) / float64(height)
	}
	outputWidth, outputHeight := max(1, int(float64(width)*scale)), max(1, int(float64(height)*scale))
	output := stdimage.NewRGBA(stdimage.Rect(0, 0, outputWidth, outputHeight))
	draw.CatmullRom.Scale(output, output.Bounds(), decoded, crop, draw.Over, nil)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, output); err != nil {
		return nil, ReadResult{}, fmt.Errorf("encode projected image: %w", err)
	}
	if encoded.Len() > options.MaxBytes {
		return nil, ReadResult{}, fmt.Errorf("projected image is %d bytes, exceeding max_bytes %d", encoded.Len(), options.MaxBytes)
	}
	metadata.MIMEType = "image/png"
	metadata.Width = outputWidth
	metadata.Height = outputHeight
	metadata.Transformed = true
	return encoded.Bytes(), metadata, nil
}
