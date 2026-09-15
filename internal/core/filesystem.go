package core

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	defaultTextBytes = 1 << 20
	maxTextBytes     = 8 << 20
	defaultResults   = 200
)

func (r *Runtime) List(rootName, path string, maxDepth int, includeHidden bool) ([]FileEntry, error) {
	root, err := r.Root(rootName)
	if err != nil {
		return nil, err
	}
	start, err := r.Resolve(rootName, path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(start)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("list path must be a directory")
	}
	if maxDepth == 0 {
		maxDepth = 4
	}
	if maxDepth < 1 || maxDepth > 64 {
		return nil, errors.New("max_depth must be between 1 and 64")
	}
	startDepth := depth(relativeSlash(root.Path, start))
	entries := make([]FileEntry, 0)
	err = filepath.WalkDir(start, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == start {
			return nil
		}
		rel := relativeSlash(root.Path, current)
		currentDepth := depth(rel) - startDepth
		if currentDepth > maxDepth {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !includeHidden && strings.HasPrefix(entry.Name(), ".") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		entries = append(entries, FileEntry{Path: rel, Type: fileType(info), Size: info.Size(), ModTime: info.ModTime()})
		if info.Mode()&os.ModeSymlink != 0 && entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	return entries, err
}

func (r *Runtime) Stat(rootName, path string) (FileInfo, error) {
	root, err := r.Root(rootName)
	if err != nil {
		return FileInfo{}, err
	}
	resolved, err := r.Resolve(rootName, path)
	if err != nil {
		return FileInfo{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return FileInfo{}, err
	}
	result := FileInfo{Path: relativeSlash(root.Path, resolved), Type: fileType(info), Size: info.Size(), ModTime: info.ModTime()}
	if !info.Mode().IsRegular() {
		return result, nil
	}
	file, err := os.Open(resolved)
	if err != nil {
		return FileInfo{}, err
	}
	defer file.Close()
	prefix := make([]byte, 512)
	read, err := file.Read(prefix)
	if err != nil && !errors.Is(err, io.EOF) {
		return FileInfo{}, err
	}
	prefix = prefix[:read]
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return FileInfo{}, err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return FileInfo{}, err
	}
	result.SHA256 = hex.EncodeToString(hash.Sum(nil))
	result.MIMEType = detectMIME(resolved, prefix)
	return result, nil
}

func (r *Runtime) ReadText(rootName, path string, maxBytes int) (TextReadResult, error) {
	root, err := r.Root(rootName)
	if err != nil {
		return TextReadResult{}, err
	}
	resolved, err := r.Resolve(rootName, path)
	if err != nil {
		return TextReadResult{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return TextReadResult{}, err
	}
	if !info.Mode().IsRegular() {
		return TextReadResult{}, errors.New("read path must be a regular file")
	}
	if maxBytes == 0 {
		maxBytes = defaultTextBytes
	}
	if maxBytes < 1 || maxBytes > maxTextBytes {
		return TextReadResult{}, fmt.Errorf("max_bytes must be between 1 and %d", maxTextBytes)
	}
	file, err := os.Open(resolved)
	if err != nil {
		return TextReadResult{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		return TextReadResult{}, err
	}
	truncated := len(data) > maxBytes
	if truncated {
		data = data[:maxBytes]
		for trimmed := 0; trimmed < 3 && len(data) > 0 && !utf8.Valid(data); trimmed++ {
			data = data[:len(data)-1]
		}
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return TextReadResult{}, errors.New("file is not UTF-8 text")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return TextReadResult{}, err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return TextReadResult{}, err
	}
	return TextReadResult{Path: relativeSlash(root.Path, resolved), Content: string(data), Size: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)), Truncated: truncated}, nil
}

func (r *Runtime) WriteText(rootName, path, content, expectedSHA string, createOnly bool) (TextWriteResult, error) {
	root, err := r.Root(rootName)
	if err != nil {
		return TextWriteResult{}, err
	}
	resolved, err := r.Resolve(rootName, path)
	if err != nil {
		return TextWriteResult{}, err
	}
	if strings.TrimSpace(path) == "" {
		return TextWriteResult{}, errors.New("path cannot be empty")
	}
	data := []byte(content)
	if !utf8.Valid(data) {
		return TextWriteResult{}, errors.New("content must be valid UTF-8")
	}
	created := false
	mode := os.FileMode(0o644)
	existing, readErr := os.ReadFile(resolved)
	switch {
	case readErr == nil:
		if createOnly {
			return TextWriteResult{}, errors.New("file already exists")
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return TextWriteResult{}, err
		}
		if !info.Mode().IsRegular() {
			return TextWriteResult{}, errors.New("write path must be a regular file")
		}
		mode = info.Mode().Perm()
		if expectedSHA != "" && !strings.EqualFold(expectedSHA, digest(existing)) {
			return TextWriteResult{}, errors.New("file changed: expected_sha256 does not match")
		}
	case errors.Is(readErr, os.ErrNotExist):
		created = true
		if expectedSHA != "" {
			return TextWriteResult{}, errors.New("file does not exist for expected_sha256 check")
		}
	default:
		return TextWriteResult{}, readErr
	}
	if err := atomicWrite(resolved, data, mode); err != nil {
		return TextWriteResult{}, err
	}
	return TextWriteResult{Path: relativeSlash(root.Path, resolved), Bytes: len(data), SHA256: digest(data), Created: created}, nil
}

func (r *Runtime) EditText(rootName, path, oldText, newText, expectedSHA string, replaceAll bool) (TextEditResult, error) {
	root, err := r.Root(rootName)
	if err != nil {
		return TextEditResult{}, err
	}
	resolved, err := r.Resolve(rootName, path)
	if err != nil {
		return TextEditResult{}, err
	}
	if oldText == "" {
		return TextEditResult{}, errors.New("old_text cannot be empty")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return TextEditResult{}, err
	}
	if !info.Mode().IsRegular() {
		return TextEditResult{}, errors.New("edit path must be a regular file")
	}
	if info.Size() > maxTextBytes {
		return TextEditResult{}, fmt.Errorf("file exceeds edit limit of %d bytes", maxTextBytes)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return TextEditResult{}, err
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return TextEditResult{}, errors.New("file is not UTF-8 text")
	}
	previousSHA := digest(data)
	if expectedSHA != "" && !strings.EqualFold(expectedSHA, previousSHA) {
		return TextEditResult{}, errors.New("file changed: expected_sha256 does not match")
	}
	count := strings.Count(string(data), oldText)
	if count == 0 {
		return TextEditResult{}, errors.New("old_text was not found")
	}
	if !replaceAll && count != 1 {
		return TextEditResult{}, fmt.Errorf("old_text matched %d times; provide a unique value or set replace_all", count)
	}
	replacements := count
	limit := -1
	if !replaceAll {
		replacements = 1
		limit = 1
	}
	updated := []byte(strings.Replace(string(data), oldText, newText, limit))
	if err := atomicWrite(resolved, updated, info.Mode().Perm()); err != nil {
		return TextEditResult{}, err
	}
	return TextEditResult{Path: relativeSlash(root.Path, resolved), Bytes: len(updated), SHA256: digest(updated), PreviousSHA256: previousSHA, Replacements: replacements}, nil
}

func (r *Runtime) SearchText(rootName, path, query string, regex, caseSensitive, includeHidden bool, maxResults int) ([]SearchMatch, error) {
	root, err := r.Root(rootName)
	if err != nil {
		return nil, err
	}
	start, err := r.Resolve(rootName, path)
	if err != nil {
		return nil, err
	}
	if query == "" {
		return nil, errors.New("query cannot be empty")
	}
	if maxResults == 0 {
		maxResults = defaultResults
	}
	if maxResults < 1 || maxResults > 10_000 {
		return nil, errors.New("max_results must be between 1 and 10000")
	}
	pattern := query
	if !regex {
		pattern = regexp.QuoteMeta(pattern)
	}
	if !caseSensitive {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile query: %w", err)
	}
	matches := make([]SearchMatch, 0)
	err = filepath.WalkDir(start, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == start && !entry.IsDir() {
			return searchFile(root.Path, current, re, maxResults, &matches)
		}
		if entry.IsDir() {
			if current != start && ((!includeHidden && strings.HasPrefix(entry.Name(), ".")) || entry.Name() == ".git") {
				return filepath.SkipDir
			}
			return nil
		}
		if len(matches) >= maxResults || entry.Type()&os.ModeSymlink != 0 || (!includeHidden && strings.HasPrefix(entry.Name(), ".")) {
			return nil
		}
		return searchFile(root.Path, current, re, maxResults, &matches)
	})
	return matches, err
}

func searchFile(root, path string, re *regexp.Regexp, limit int, matches *[]SearchMatch) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxTextBytes {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		data := scanner.Bytes()
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		locations := re.FindAllIndex(data, -1)
		for _, location := range locations {
			*matches = append(*matches, SearchMatch{Path: relativeSlash(root, path), Line: line, Column: utf8.RuneCount(data[:location[0]]) + 1, Text: scanner.Text()})
			if len(*matches) >= limit {
				return nil
			}
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return nil
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".lrmcp-write-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return err
	}
	committed = true
	return nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func detectMIME(path string, data []byte) string {
	if value := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); value != "" {
		return value
	}
	return http.DetectContentType(data)
}

func fileType(info os.FileInfo) string {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return "symlink"
	case info.IsDir():
		return "directory"
	case info.Mode().IsRegular():
		return "file"
	default:
		return "other"
	}
}

func depth(path string) int {
	if path == "" {
		return 0
	}
	return strings.Count(filepath.ToSlash(path), "/") + 1
}
