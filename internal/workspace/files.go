package workspace

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	defaultReadLimit  = 1024 * 1024
	defaultTreeLimit  = 5000
	defaultSearchMax  = 200
	defaultSearchSize = 2 * 1024 * 1024
	defaultImageLimit = 10 * 1024 * 1024
)

func (m *Manager) Tree(name, subpath string, maxDepth int, includeHidden bool) ([]TreeEntry, error) {
	workspaceRoot, err := m.Root(name)
	if err != nil {
		return nil, err
	}
	root, err := m.Path(name, subpath)
	if err != nil {
		return nil, err
	}
	if maxDepth <= 0 {
		maxDepth = 4
	}
	baseDepth := pathDepth(root)
	entries := make([]TreeEntry, 0)
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		depth := pathDepth(path) - baseDepth
		if depth > maxDepth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !includeHidden && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(workspaceRoot, path)
		typeName := "file"
		if d.IsDir() {
			typeName = "directory"
		} else if info.Mode()&os.ModeSymlink != 0 {
			typeName = "symlink"
		}
		entries = append(entries, TreeEntry{
			Path: filepath.ToSlash(rel), Type: typeName, Size: info.Size(),
			ModTime: info.ModTime().Format("2006-01-02T15:04:05Z07:00"),
		})
		if len(entries) >= defaultTreeLimit {
			return errors.New("tree result exceeded 5000 entries; choose a narrower path")
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk tree: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func (m *Manager) Read(name, path string, maxBytes int) (ReadResult, error) {
	full, err := m.Path(name, path)
	if err != nil {
		return ReadResult{}, err
	}
	if maxBytes <= 0 {
		maxBytes = defaultReadLimit
	}
	f, err := os.Open(full)
	if err != nil {
		return ReadResult{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ReadResult{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return ReadResult{}, fmt.Errorf("read %s: %w", path, err)
	}
	truncated := len(b) > maxBytes
	if truncated {
		b = b[:maxBytes]
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return ReadResult{}, fmt.Errorf("%s is not a UTF-8 text file; use image_read for supported images or exec with a suitable CLI", path)
	}
	if truncated {
		for removed := 0; removed < utf8.UTFMax-1 && !utf8.Valid(b) && len(b) > 0; removed++ {
			b = b[:len(b)-1]
		}
	}
	if !utf8.Valid(b) {
		return ReadResult{}, fmt.Errorf("%s is not a UTF-8 text file; use image_read for supported images or exec with a suitable CLI", path)
	}
	digest, err := fileSHA256(full)
	if err != nil {
		return ReadResult{}, err
	}
	return ReadResult{Path: filepath.ToSlash(path), Content: string(b), Size: info.Size(), SHA256: digest, Truncated: truncated}, nil
}

// Inspect returns metadata for a file or directory without returning its content.
func (m *Manager) Inspect(name, path string) (FileInfo, error) {
	full, err := m.Path(name, path)
	if err != nil {
		return FileInfo{}, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return FileInfo{}, fmt.Errorf("stat %s: %w", path, err)
	}
	result := FileInfo{
		Path: filepath.ToSlash(path), Type: "file", Size: info.Size(),
		ModTime: info.ModTime().Format("2006-01-02T15:04:05Z07:00"),
	}
	if info.IsDir() {
		result.Type = "directory"
		result.Size = 0
		return result, nil
	}
	b, err := readPrefix(full, 512)
	if err != nil {
		return FileInfo{}, err
	}
	result.MIMEType = detectMIME(path, b)
	result.SHA256, err = fileSHA256(full)
	return result, err
}

// ReadImage returns image bytes in a format accepted by MCP image content.
func (m *Manager) ReadImage(name, path string, maxBytes int) ([]byte, ImageReadResult, error) {
	full, err := m.Path(name, path)
	if err != nil {
		return nil, ImageReadResult{}, err
	}
	b, info, err := readWholeFile(full, path, maxBytes)
	if err != nil {
		return nil, ImageReadResult{}, err
	}
	digest, err := fileSHA256(full)
	if err != nil {
		return nil, ImageReadResult{}, err
	}
	result := ImageReadResult{
		Path: filepath.ToSlash(path), Size: info.Size(), MIMEType: detectMIME(path, b), SHA256: digest,
	}
	switch result.MIMEType {
	case "image/png", "image/jpeg", "image/gif":
		cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
		if err != nil {
			return nil, ImageReadResult{}, fmt.Errorf("decode image %s: %w", path, err)
		}
		result.Width = cfg.Width
		result.Height = cfg.Height
	case "image/webp":
		if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
			return nil, ImageReadResult{}, fmt.Errorf("%s is not a valid WebP image", path)
		}
	default:
		return nil, ImageReadResult{}, fmt.Errorf("%s has MIME type %s; image_read supports PNG, JPEG, GIF, and WebP", path, result.MIMEType)
	}
	return b, result, nil
}

func (m *Manager) Write(name, path, content string) (WriteResult, error) {
	full, err := m.Path(name, path)
	if err != nil {
		return WriteResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return WriteResult{}, fmt.Errorf("create parent directories: %w", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return WriteResult{}, fmt.Errorf("write %s: %w", path, err)
	}
	digest, err := fileSHA256(full)
	if err != nil {
		return WriteResult{}, err
	}
	return WriteResult{Path: filepath.ToSlash(path), Bytes: len([]byte(content)), SHA256: digest}, nil
}

func (m *Manager) Search(name, query string, useRegex, caseSensitive bool, maxResults int) ([]SearchMatch, error) {
	root, err := m.Root(name)
	if err != nil {
		return nil, err
	}
	if query == "" {
		return nil, errors.New("search query cannot be empty")
	}
	if maxResults <= 0 {
		maxResults = defaultSearchMax
	}
	pattern := query
	if !useRegex {
		pattern = regexp.QuoteMeta(pattern)
	}
	if !caseSensitive {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile search expression: %w", err)
	}
	var matches []SearchMatch
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if strings.EqualFold(d.Name(), ".git") {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > defaultSearchSize {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		peek := make([]byte, 8000)
		n, _ := f.Read(peek)
		if bytes.IndexByte(peek[:n], 0) >= 0 {
			return nil
		}
		_, _ = f.Seek(0, io.SeekStart)
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		line := 0
		for scanner.Scan() {
			line++
			loc := re.FindStringIndex(scanner.Text())
			if loc == nil {
				continue
			}
			rel, _ := filepath.Rel(root, path)
			matches = append(matches, SearchMatch{Path: filepath.ToSlash(rel), Line: line, Column: loc[0] + 1, Text: scanner.Text()})
			if len(matches) >= maxResults {
				return io.EOF
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("search workspace: %w", err)
	}
	return matches, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func readWholeFile(full, displayPath string, maxBytes int) ([]byte, os.FileInfo, error) {
	if maxBytes <= 0 {
		maxBytes = defaultImageLimit
	}
	info, err := os.Stat(full)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", displayPath, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", displayPath)
	}
	if info.Size() > int64(maxBytes) {
		return nil, nil, fmt.Errorf("%s is %d bytes, exceeding max_bytes=%d; resize it or request a larger limit", displayPath, info.Size(), maxBytes)
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", displayPath, err)
	}
	return b, info, nil
}

func readPrefix(path string, size int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, size)
	n, err := f.Read(b)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return b[:n], nil
}

func detectMIME(path string, data []byte) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	}
	if byExt := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); byExt != "" {
		return strings.Split(byExt, ";")[0]
	}
	if len(data) == 0 {
		return "application/octet-stream"
	}
	return http.DetectContentType(data)
}

func pathDepth(path string) int {
	clean := filepath.Clean(path)
	return strings.Count(clean, string(os.PathSeparator))
}
