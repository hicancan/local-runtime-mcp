package filesystem

import (
	"bufio"
	"bytes"
	"context"
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
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Service coordinates commits made through one runtime. External writers do not
// participate in these locks.
type Service struct {
	mu       sync.Mutex
	paths    map[string]*pathLock
	capacity chan struct{}
}

type pathLock struct {
	gate       chan struct{}
	references int
}

func New() *Service {
	return &Service{paths: make(map[string]*pathLock), capacity: make(chan struct{}, 256)}
}

func (s *Service) acquire(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.capacity <- struct{}{}:
	default:
		return nil, errors.New("filesystem commit capacity is full")
	}
	key, err := commitIdentity(path)
	if err != nil {
		<-s.capacity
		return nil, err
	}
	s.mu.Lock()
	lock := s.paths[key]
	if lock == nil {
		lock = &pathLock{gate: make(chan struct{}, 1)}
		s.paths[key] = lock
	}
	lock.references++
	s.mu.Unlock()
	cleanup := func() {
		s.mu.Lock()
		lock.references--
		if lock.references == 0 {
			delete(s.paths, key)
		}
		s.mu.Unlock()
		<-s.capacity
	}
	select {
	case lock.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-lock.gate
			cleanup()
			return nil, err
		}
		return func() { <-lock.gate; cleanup() }, nil
	case <-ctx.Done():
		cleanup()
		return nil, ctx.Err()
	}
}

func commitIdentity(path string) (string, error) {
	// Resolve existing parent aliases (including Windows junctions), while leaving
	// the final component untouched so mutations can reject symbolic links.
	parent := filepath.Dir(path)
	suffix := filepath.Base(path)
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			key := filepath.Clean(filepath.Join(resolved, suffix))
			if runtime.GOOS == "windows" {
				key = strings.ToLower(strings.TrimPrefix(key, `\\?\`))
			}
			return key, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", err
		}
		suffix = filepath.Join(filepath.Base(parent), suffix)
		parent = next
	}
}

const (
	defaultTextBytes  = 1 << 20
	maxTextBytes      = 8 << 20
	defaultResults    = 200
	defaultEntries    = 1_000
	maxMatchLineBytes = 4096
)

func truncateUTF8Bytes(data []byte, maxBytes int) string {
	if len(data) <= maxBytes {
		return string(data)
	}
	i := maxBytes
	for i > 0 && !utf8.RuneStart(data[i]) {
		i--
	}
	return string(data[:i]) + "…"
}

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
	return ListContext(context.Background(), path, maxDepth, includeHidden, maxEntries)
}

func ListContext(ctx context.Context, path string, maxDepth int, includeHidden bool, maxEntries int) (ListResult, error) {
	if err := ctx.Err(); err != nil {
		return ListResult{}, err
	}
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
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
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
		if entry.IsDir() && currentDepth >= maxDepth {
			return filepath.SkipDir
		}
		return nil
	})
	return result, err
}

func Stat(path string, withSHA256 bool) (FileInfo, error) {
	return StatContext(context.Background(), path, withSHA256)
}

func StatContext(ctx context.Context, path string, withSHA256 bool) (FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return FileInfo{}, err
	}
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
	if !withSHA256 {
		if value := mime.TypeByExtension(strings.ToLower(filepath.Ext(resolved))); value != "" {
			result.MIMEType = value
			return result, nil
		}
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
	return ReadTextContext(context.Background(), path, options)
}

func ReadTextContext(ctx context.Context, path string, options ReadTextOptions) (TextReadResult, error) {
	if err := ctx.Err(); err != nil {
		return TextReadResult{}, err
	}
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
		if line&63 == 0 {
			if err := ctx.Err(); err != nil {
				return TextReadResult{}, err
			}
		}
		part, present, readErr := readTextLine(reader, options.MaxBytes-len(data), line+1 >= options.StartLine)
		if present {
			line++
			if line >= options.StartLine {
				if options.LineCount > 0 && line >= options.StartLine+options.LineCount {
					truncated, nextLine = true, line
					break
				}
				if errors.Is(readErr, errTextLineLimit) {
					if len(data) == 0 {
						return TextReadResult{}, fmt.Errorf("line %d exceeds max_bytes %d", line, options.MaxBytes)
					}
					truncated, nextLine = true, line
					break
				}
				if !utf8.Valid(part) || bytes.IndexByte(part, 0) >= 0 {
					return TextReadResult{}, fmt.Errorf("file is not UTF-8 text at line %d", line)
				}
				data = appendTextBytes(data, part, options.MaxBytes)
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

var errTextLineLimit = errors.New("text line exceeds retained byte budget")

// readTextLine retains complete lines within a fixed budget. Skipped lines are
// scanned in reader-sized fragments without allocating a full line.
func readTextLine(reader *bufio.Reader, limit int, retain bool) ([]byte, bool, error) {
	var data []byte
	present := false
	for {
		fragment, err := reader.ReadSlice('\n')
		present = present || len(fragment) != 0
		if retain {
			if len(fragment) > limit-len(data) {
				return data, present, errTextLineLimit
			}
			data = appendTextBytes(data, fragment, limit)
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return data, present, err
		}
	}
}

func appendTextBytes(data, fragment []byte, limit int) []byte {
	if len(data)+len(fragment) > cap(data) {
		capacity := min(limit, max(len(data)+len(fragment), cap(data)*2))
		grown := make([]byte, len(data), capacity)
		copy(grown, data)
		data = grown
	}
	return append(data, fragment...)
}

func (s *Service) WriteText(ctx context.Context, path, content string, options WriteTextOptions) (TextWriteResult, error) {
	resolved, err := resolve(path)
	if err != nil {
		return TextWriteResult{}, err
	}
	if options.Mode != "create" && options.Mode != "replace" {
		return TextWriteResult{}, errors.New("mode must be create or replace")
	}
	if options.Mode == "replace" && !validHash(options.ExpectedSHA256) {
		return TextWriteResult{}, errors.New("replace requires expected_sha256 containing 64 hexadecimal characters")
	}
	if options.Mode == "create" && options.ExpectedSHA256 != "" {
		return TextWriteResult{}, errors.New("create cannot include expected_sha256")
	}
	unlock, err := s.acquire(ctx, resolved)
	if err != nil {
		return TextWriteResult{}, err
	}
	defer unlock()
	data := []byte(content)
	if len(data) > maxTextBytes {
		return TextWriteResult{}, fmt.Errorf("content exceeds limit of %d bytes", maxTextBytes)
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
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
		if options.Mode == "create" {
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
		if options.Mode == "replace" {
			return TextWriteResult{}, errors.New("replace requires an existing file")
		}
		created = true
	default:
		return TextWriteResult{}, statErr
	}
	if err := ensureParent(resolved, options.CreateParents); err != nil {
		return TextWriteResult{}, err
	}
	if err := atomicWrite(ctx, resolved, data, mode, options.Mode == "create"); err != nil {
		return TextWriteResult{}, err
	}
	return TextWriteResult{Path: resolved, Bytes: len(data), Created: created, SHA256: hashBytes(data)}, nil
}

func (s *Service) PatchText(ctx context.Context, path string, options PatchTextOptions) (TextPatchResult, error) {
	resolved, err := resolve(path)
	if err != nil {
		return TextPatchResult{}, err
	}
	unlock, err := s.acquire(ctx, resolved)
	if err != nil {
		return TextPatchResult{}, err
	}
	defer unlock()
	if len(options.Hunks) == 0 {
		return TextPatchResult{}, errors.New("hunks cannot be empty")
	}
	if len(options.Hunks) > 1_000 {
		return TextPatchResult{}, errors.New("hunks cannot contain more than 1000 items")
	}
	if !validHash(options.ExpectedSHA256) {
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
	file, err := os.Open(resolved)
	if err != nil {
		return TextPatchResult{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxTextBytes+1))
	closeErr := file.Close()
	if err != nil {
		return TextPatchResult{}, err
	}
	if closeErr != nil {
		return TextPatchResult{}, closeErr
	}
	if len(data) > maxTextBytes {
		return TextPatchResult{}, fmt.Errorf("file exceeds patch limit of %d bytes", maxTextBytes)
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
	located := make([]locatedHunk, 0, len(options.Hunks))
	for index, hunk := range options.Hunks {
		if len(hunk.Before)+len(hunk.Old)+len(hunk.After) > maxTextBytes || len(hunk.New) > maxTextBytes {
			return TextPatchResult{}, fmt.Errorf("hunks[%d] exceeds text limit", index)
		}
		if !utf8.ValidString(hunk.New) || strings.IndexByte(hunk.New, 0) >= 0 {
			return TextPatchResult{}, fmt.Errorf("hunks[%d] new must be UTF-8 text", index)
		}
		if hunk.Old == "" && hunk.Before == "" && hunk.After == "" {
			return TextPatchResult{}, fmt.Errorf("hunks[%d] insertion requires before or after context", index)
		}
		anchor := []byte(hunk.Before + hunk.Old + hunk.After)
		matches := exactByteMatches(data, anchor, 10)
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
	updatedSize := int64(len(data))
	for _, hunk := range located {
		updatedSize += int64(len(hunk.new)) - int64(hunk.end-hunk.start)
	}
	if updatedSize > maxTextBytes {
		return TextPatchResult{}, fmt.Errorf("patched file exceeds limit of %d bytes", maxTextBytes)
	}
	var updated bytes.Buffer
	updated.Grow(int(updatedSize))
	offset := 0
	for _, hunk := range located {
		updated.Write(data[offset:hunk.start])
		updated.WriteString(hunk.new)
		offset = hunk.end
	}
	updated.Write(data[offset:])
	updatedBytes := updated.Bytes()
	if err := atomicWrite(ctx, resolved, updatedBytes, info.Mode().Perm(), false); err != nil {
		return TextPatchResult{}, err
	}
	return TextPatchResult{Path: resolved, Bytes: len(updatedBytes), Hunks: len(located), SHA256: hashBytes(updatedBytes)}, nil
}

func exactByteMatches(value, pattern []byte, maxReport int) []int {
	if len(pattern) == 0 {
		return nil
	}
	var matches []int
	for offset := 0; offset <= len(value)-len(pattern); {
		index := bytes.Index(value[offset:], pattern)
		if index < 0 {
			break
		}
		absolute := offset + index
		matches = append(matches, absolute)
		if maxReport > 0 && len(matches) >= maxReport {
			break
		}
		offset = absolute + 1
	}
	return matches
}

type lineMatcher struct {
	re            *regexp.Regexp
	literal       []byte
	caseSensitive bool
	asciiFold     bool
}

func newLineMatcher(query string, regex, caseSensitive bool) (*lineMatcher, error) {
	if !regex {
		if caseSensitive {
			return &lineMatcher{literal: []byte(query), caseSensitive: true}, nil
		}
		ascii := true
		for i := 0; i < len(query); i++ {
			if query[i] >= 0x80 {
				ascii = false
				break
			}
		}
		if ascii {
			return &lineMatcher{literal: []byte(strings.ToLower(query)), asciiFold: true}, nil
		}
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
	return &lineMatcher{re: re}, nil
}

// forEachMatch invokes fn(startByteOffset) for each non-overlapping match in line.
// Returning false from fn stops scanning immediately.
func (m *lineMatcher) forEachMatch(line []byte, fn func(start int) bool) {
	if m.re != nil {
		for _, loc := range m.re.FindAllIndex(line, -1) {
			if !fn(loc[0]) {
				return
			}
		}
		return
	}
	needle := m.literal
	nLen := len(needle)
	if nLen == 0 || len(line) < nLen {
		return
	}
	if m.caseSensitive {
		for off := 0; off <= len(line)-nLen; {
			idx := bytes.Index(line[off:], needle)
			if idx < 0 {
				return
			}
			pos := off + idx
			if !fn(pos) {
				return
			}
			off = pos + nLen
		}
		return
	}
	// Zero-allocation ASCII case-insensitive scan (safe on UTF-8 since ASCII bytes < 0x80
	// never collide with multi-byte continuation bytes >= 0x80).
	firstLo := needle[0]
	firstHi := firstLo
	if firstLo >= 'a' && firstLo <= 'z' {
		firstHi = firstLo - ('a' - 'A')
	}
	for off := 0; off <= len(line)-nLen; {
		b := line[off]
		if b != firstLo && b != firstHi {
			off++
			continue
		}
		matched := true
		for j := 1; j < nLen; j++ {
			c := line[off+j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needle[j] {
				matched = false
				break
			}
		}
		if matched {
			if !fn(off) {
				return
			}
			off += nLen
		} else {
			off++
		}
	}
}

func SearchText(path, query string, regex, caseSensitive, includeHidden bool, maxResults int) (SearchResult, error) {
	return SearchTextContext(context.Background(), path, query, regex, caseSensitive, includeHidden, maxResults)
}

func SearchTextContext(ctx context.Context, path, query string, regex, caseSensitive, includeHidden bool, maxResults int) (SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return SearchResult{}, err
	}
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
	matcher, err := newLineMatcher(query, regex, caseSensitive)
	if err != nil {
		return SearchResult{}, err
	}
	scanBuf := make([]byte, 64*1024)
	result := SearchResult{Matches: make([]SearchMatch, 0)}
	err = filepath.WalkDir(start, func(current string, entry os.DirEntry, walkErr error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
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
		if entry.Type()&os.ModeSymlink != 0 || (!includeHidden && isHiddenName(entry.Name())) {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() || info.Size() > maxTextBytes {
			result.Skipped++
			return nil
		}
		skipped, truncated, scanErr := searchFile(ctx, current, matcher, scanBuf, maxResults, &result.Matches)
		if scanErr != nil {
			return scanErr
		}
		if skipped {
			result.Skipped++
		}
		if truncated {
			result.Truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	return result, err
}

func searchFile(ctx context.Context, path string, matcher *lineMatcher, scanBuf []byte, limit int, matches *[]SearchMatch) (skipped, truncated bool, err error) {
	file, openErr := os.Open(path)
	if openErr != nil {
		return true, false, nil
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(scanBuf, 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		if line&255 == 0 {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return false, false, ctxErr
			}
		}
		data := scanner.Bytes()
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return true, false, nil
		}
		var lineText string
		lineTextReady := false
		hitOverflow := false
		matcher.forEachMatch(data, func(start int) bool {
			if len(*matches) >= limit {
				hitOverflow = true
				return false
			}
			if !lineTextReady {
				lineText = truncateUTF8Bytes(data, maxMatchLineBytes)
				lineTextReady = true
			}
			*matches = append(*matches, SearchMatch{
				Path:   path,
				Line:   line,
				Column: utf8.RuneCount(data[:start]) + 1,
				Text:   lineText,
			})
			return true
		})
		if hitOverflow {
			return false, true, nil
		}
	}
	if scanner.Err() != nil {
		return true, false, nil
	}
	return false, false, nil
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

func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func atomicWrite(ctx context.Context, path string, data []byte, mode os.FileMode, createOnly bool) error {
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
	if err := ctx.Err(); err != nil {
		return err
	}
	commit := replaceFile
	if createOnly {
		commit = createFile
	}
	if err := commit(temporaryName, path); err != nil {
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
