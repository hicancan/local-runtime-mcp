package filesystem

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
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	defaultTextBytes = 1 << 20
	maxTextBytes     = 8 << 20
	defaultResults   = 200
	defaultEntries   = 1_000
)

func resolve(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path cannot be empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path: %w", err)
	}
	return filepath.Clean(abs), nil
}

func List(path string, maxDepth int, includeHidden bool, maxEntries int) (ListResult, error) {
	start, err := resolve(path)
	if err != nil {
		return ListResult{}, err
	}
	info, err := os.Lstat(start)
	if err != nil {
		return ListResult{}, err
	}
	if !info.IsDir() {
		return ListResult{}, errors.New("list path must be a directory")
	}
	if maxDepth == 0 {
		maxDepth = 1
	}
	if maxDepth < 1 || maxDepth > 64 {
		return ListResult{}, errors.New("max_depth must be between 1 and 64")
	}
	if maxEntries == 0 {
		maxEntries = defaultEntries
	}
	if maxEntries < 1 || maxEntries > 100_000 {
		return ListResult{}, errors.New("max_entries must be between 1 and 100000")
	}
	result := ListResult{Entries: make([]FileEntry, 0, min(maxEntries, defaultEntries))}
	err = filepath.WalkDir(start, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			result.Skipped++
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if current == start && entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(start, current)
		if err != nil {
			return err
		}
		currentDepth := strings.Count(filepath.ToSlash(rel), "/") + 1
		if currentDepth > maxDepth {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !includeHidden && isHiddenName(entry.Name()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			result.Skipped++
			return nil
		}
		if len(result.Entries) >= maxEntries {
			result.Truncated = true
			return filepath.SkipAll
		}
		result.Entries = append(result.Entries, FileEntry{Path: filepath.Clean(current), Type: fileType(info), Size: info.Size(), ModTime: info.ModTime()})
		return nil
	})
	return result, err
}

func Stat(path string, withSHA256 bool) (FileInfo, error) {
	resolved, err := resolve(path)
	if err != nil {
		return FileInfo{}, err
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return FileInfo{}, err
	}
	result := FileInfo{Path: resolved, Type: fileType(info), Size: info.Size(), ModTime: info.ModTime()}
	if info.Mode()&os.ModeSymlink != 0 {
		result.SymlinkTarget, err = os.Readlink(resolved)
		return result, err
	}
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
	result.MIMEType = detectMIME(resolved, prefix[:read])
	if withSHA256 {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return FileInfo{}, err
		}
		result.SHA256, err = hashReader(file)
		if err != nil {
			return FileInfo{}, err
		}
	}
	return result, nil
}

func ReadText(path string, options ReadTextOptions) (TextReadResult, error) {
	resolved, err := resolve(path)
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
	if options.StartLine == 0 {
		options.StartLine = 1
	}
	if options.StartLine < 1 {
		return TextReadResult{}, errors.New("start_line must be positive")
	}
	if options.LineCount < 0 || options.LineCount > 1_000_000 {
		return TextReadResult{}, errors.New("line_count must be between 1 and 1000000 when provided")
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = defaultTextBytes
	}
	if options.MaxBytes < 1 || options.MaxBytes > maxTextBytes {
		return TextReadResult{}, fmt.Errorf("max_bytes must be between 1 and %d", maxTextBytes)
	}
	file, err := os.Open(resolved)
	if err != nil {
		return TextReadResult{}, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	var data []byte
	line, endLine, nextLine := 0, 0, 0
	truncated := false
	for {
		part, readErr := reader.ReadBytes('\n')
		if len(part) > 0 {
			line++
			if line >= options.StartLine {
				if options.LineCount > 0 && line >= options.StartLine+options.LineCount {
					truncated, nextLine = true, line
					break
				}
				if !utf8.Valid(part) || bytes.IndexByte(part, 0) >= 0 {
					return TextReadResult{}, fmt.Errorf("file is not UTF-8 text at line %d", line)
				}
				if len(data)+len(part) > options.MaxBytes {
					if len(data) == 0 {
						return TextReadResult{}, fmt.Errorf("line %d exceeds max_bytes %d", line, options.MaxBytes)
					}
					truncated, nextLine = true, line
					break
				}
				data = append(data, part...)
				endLine = line
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return TextReadResult{}, readErr
		}
	}
	result := TextReadResult{Path: resolved, Content: string(data), Size: info.Size(), StartLine: options.StartLine, EndLine: endLine, NextLine: nextLine, Truncated: truncated}
	if options.WithSHA256 {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return TextReadResult{}, err
		}
		result.SHA256, err = hashReader(file)
		if err != nil {
			return TextReadResult{}, err
		}
	}
	return result, nil
}

func WriteText(path, content string, options WriteTextOptions) (TextWriteResult, error) {
	resolved, err := resolve(path)
	if err != nil {
		return TextWriteResult{}, err
	}
	data := []byte(content)
	if !utf8.Valid(data) {
		return TextWriteResult{}, errors.New("content must be valid UTF-8")
	}
	created := false
	mode := os.FileMode(0o644)
	info, statErr := os.Lstat(resolved)
	switch {
	case statErr == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return TextWriteResult{}, errors.New("write path cannot be a symbolic link")
		}
		if options.CreateOnly {
			return TextWriteResult{}, errors.New("file already exists")
		}
		if !info.Mode().IsRegular() {
			return TextWriteResult{}, errors.New("write path must be a regular file")
		}
		mode = info.Mode().Perm()
		if options.ExpectedSHA256 != "" {
			current, err := hashFile(resolved)
			if err != nil {
				return TextWriteResult{}, err
			}
			if !strings.EqualFold(current, options.ExpectedSHA256) {
				return TextWriteResult{}, errors.New("file content does not match expected_sha256")
			}
		}
	case errors.Is(statErr, os.ErrNotExist):
		if options.ExpectedSHA256 != "" {
			return TextWriteResult{}, errors.New("expected_sha256 requires an existing file")
		}
		created = true
	default:
		return TextWriteResult{}, statErr
	}
	if err := ensureParent(resolved, options.CreateParents); err != nil {
		return TextWriteResult{}, err
	}
	if err := atomicWrite(resolved, data, mode); err != nil {
		return TextWriteResult{}, err
	}
	return TextWriteResult{Path: resolved, Bytes: len(data), Created: created, SHA256: hashBytes(data)}, nil
}

func PatchText(path string, options PatchTextOptions) (TextPatchResult, error) {
	resolved, err := resolve(path)
	if err != nil {
		return TextPatchResult{}, err
	}
	if len(options.Hunks) == 0 {
		return TextPatchResult{}, errors.New("hunks cannot be empty")
	}
	if len(options.Hunks) > 1_000 {
		return TextPatchResult{}, errors.New("hunks cannot contain more than 1000 items")
	}
	if len(options.ExpectedSHA256) != 64 {
		return TextPatchResult{}, errors.New("expected_sha256 is required and must contain 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(options.ExpectedSHA256); err != nil {
		return TextPatchResult{}, errors.New("expected_sha256 is required and must contain 64 hexadecimal characters")
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return TextPatchResult{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return TextPatchResult{}, errors.New("patch path cannot be a symbolic link")
	}
	if !info.Mode().IsRegular() {
		return TextPatchResult{}, errors.New("patch path must be a regular file")
	}
	if info.Size() > maxTextBytes {
		return TextPatchResult{}, fmt.Errorf("file exceeds patch limit of %d bytes", maxTextBytes)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return TextPatchResult{}, err
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return TextPatchResult{}, errors.New("file is not UTF-8 text")
	}
	if !strings.EqualFold(hashBytes(data), options.ExpectedSHA256) {
		return TextPatchResult{}, errors.New("file content does not match expected_sha256")
	}

	type locatedHunk struct {
		start, end int
		new        string
		index      int
	}
	original := string(data)
	located := make([]locatedHunk, 0, len(options.Hunks))
	for index, hunk := range options.Hunks {
		if hunk.Old == "" && hunk.Before == "" && hunk.After == "" {
			return TextPatchResult{}, fmt.Errorf("hunks[%d] insertion requires before or after context", index)
		}
		anchor := hunk.Before + hunk.Old + hunk.After
		matches := allExactMatches(original, anchor)
		if len(matches) == 0 {
			return TextPatchResult{}, fmt.Errorf("hunks[%d] context was not found in the original file", index)
		}
		if len(matches) != 1 {
			return TextPatchResult{}, fmt.Errorf("hunks[%d] context matched %d times in the original file", index, len(matches))
		}
		start := matches[0] + len(hunk.Before)
		located = append(located, locatedHunk{start: start, end: start + len(hunk.Old), new: hunk.New, index: index})
	}
	sort.Slice(located, func(i, j int) bool { return located[i].start < located[j].start })
	for index := 1; index < len(located); index++ {
		previous, current := located[index-1], located[index]
		if current.start < previous.end || current.start == previous.start {
			return TextPatchResult{}, fmt.Errorf("hunks[%d] overlaps hunks[%d] in the original file", current.index, previous.index)
		}
	}
	updated := append([]byte(nil), data...)
	for index := len(located) - 1; index >= 0; index-- {
		hunk := located[index]
		updated = append(updated[:hunk.start], append([]byte(hunk.new), updated[hunk.end:]...)...)
	}
	updatedBytes := updated
	if len(updatedBytes) > maxTextBytes {
		return TextPatchResult{}, fmt.Errorf("patched file exceeds limit of %d bytes", maxTextBytes)
	}
	if err := atomicWrite(resolved, updatedBytes, info.Mode().Perm()); err != nil {
		return TextPatchResult{}, err
	}
	return TextPatchResult{Path: resolved, Bytes: len(updatedBytes), Hunks: len(located), SHA256: hashBytes(updatedBytes)}, nil
}

func allExactMatches(value, pattern string) []int {
	if pattern == "" {
		return nil
	}
	var matches []int
	for offset := 0; offset <= len(value)-len(pattern); {
		index := strings.Index(value[offset:], pattern)
		if index < 0 {
			break
		}
		absolute := offset + index
		matches = append(matches, absolute)
		offset = absolute + 1
	}
	return matches
}

func SearchText(path, query string, regex, caseSensitive, includeHidden bool, maxResults int) (SearchResult, error) {
	start, err := resolve(path)
	if err != nil {
		return SearchResult{}, err
	}
	if query == "" {
		return SearchResult{}, errors.New("query cannot be empty")
	}
	if maxResults == 0 {
		maxResults = defaultResults
	}
	if maxResults < 1 || maxResults > 10_000 {
		return SearchResult{}, errors.New("max_results must be between 1 and 10000")
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
		return SearchResult{}, fmt.Errorf("compile query: %w", err)
	}
	result := SearchResult{Matches: make([]SearchMatch, 0)}
	err = filepath.WalkDir(start, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			result.Skipped++
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if current != start && !includeHidden && isHiddenName(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(result.Matches) >= maxResults {
			result.Truncated = true
			return filepath.SkipAll
		}
		if entry.Type()&os.ModeSymlink != 0 || (!includeHidden && isHiddenName(entry.Name())) {
			return nil
		}
		skipped, err := searchFile(current, re, maxResults, &result.Matches)
		if skipped {
			result.Skipped++
		}
		if len(result.Matches) >= maxResults {
			result.Truncated = true
			return filepath.SkipAll
		}
		return err
	})
	return result, err
}

func searchFile(path string, re *regexp.Regexp, limit int, matches *[]SearchMatch) (bool, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxTextBytes {
		return true, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return true, nil
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		data := scanner.Bytes()
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return true, nil
		}
		for _, location := range re.FindAllIndex(data, -1) {
			*matches = append(*matches, SearchMatch{Path: path, Line: line, Column: utf8.RuneCount(data[:location[0]]) + 1, Text: scanner.Text()})
			if len(*matches) >= limit {
				return false, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return true, nil
	}
	return false, nil
}

func isHiddenName(name string) bool { return strings.HasPrefix(name, ".") }

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return hashReader(file)
}

func hashReader(reader io.Reader) (string, error) {
	digest := sha256.New()
	if _, err := io.Copy(digest, reader); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".local-runtime-mcp-write-*")
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
	if err := replaceFile(temporaryName, path); err != nil {
		return err
	}
	committed = true
	return nil
}

func ensureParent(path string, create bool) error {
	parent := filepath.Dir(path)
	info, err := os.Stat(parent)
	if err == nil {
		if !info.IsDir() {
			return errors.New("write parent must be a directory")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !create {
		return errors.New("write parent does not exist; set create_parents to create it")
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create write parent: %w", err)
	}
	return nil
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
