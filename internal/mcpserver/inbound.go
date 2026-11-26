package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/hicancan/local-runtime-mcp/internal/jsrt"
)

const (
	defaultMaxInboundFiles = 16
	defaultMaxInboundBytes = 100 << 20 // 100 MiB per inbound file
	inboundFetchTimeout    = 60 * time.Second
	inboundRetention       = 48 * time.Hour
	inboundMaxRedirects    = 5
)

// InboundFileRef matches OpenAI Apps SDK's _meta["openai/fileParams"] file
// object contract: all four properties declared; download_url and file_id
// required; mime_type and file_name optional.
type InboundFileRef struct {
	DownloadURL string `json:"download_url" jsonschema:"HTTPS download URL provided by the chat host for an uploaded or generated file"`
	FileID      string `json:"file_id" jsonschema:"Opaque host file identifier"`
	MIMEType    string `json:"mime_type,omitempty" jsonschema:"Optional MIME type (e.g. image/png, application/pdf)"`
	FileName    string `json:"file_name,omitempty" jsonschema:"Optional original file name"`
}

type inboundStore struct {
	dir           string
	maxFiles      int
	maxBytes      int64
	allowInsecure bool // test-only override for httptest loopback servers
	client        *http.Client

	mu       sync.Mutex
	cached   map[string]jsrt.InboundFile
	lastPrune time.Time
}

func newInboundStore(dir string) *inboundStore {
	if strings.TrimSpace(dir) == "" {
		dir = filepath.Join(os.TempDir(), "lrmcp-inbound")
	}
	s := &inboundStore{
		dir:      dir,
		maxFiles: defaultMaxInboundFiles,
		maxBytes: defaultMaxInboundBytes,
		cached:   map[string]jsrt.InboundFile{},
	}
	s.client = s.buildHTTPClient()
	return s
}

func (s *inboundStore) buildHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	if !s.allowInsecure {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			// When connecting directly (not via an explicit HTTP(S)_PROXY), resolve DNS
			// up front and refuse loopback/private/link-local targets to stop DNS rebinding SSRF.
			if !isConfiguredProxyAddr(addr) {
				if ip := net.ParseIP(host); ip != nil {
					if err := checkInboundIP(ip); err != nil {
						return nil, err
					}
					return dialer.DialContext(ctx, network, addr)
				}
				ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
				if err != nil {
					return nil, err
				}
				if len(ips) == 0 {
					return nil, fmt.Errorf("no IP addresses resolved for %s", host)
				}
				for _, ipAddr := range ips {
					if err := checkInboundIP(ipAddr.IP); err != nil {
						return nil, fmt.Errorf("blocked inbound download target %s (%s): %w", host, ipAddr.IP, err)
					}
				}
				var lastErr error
				for _, ipAddr := range ips {
					conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
					if dialErr == nil {
						return conn, nil
					}
					lastErr = dialErr
				}
				return nil, lastErr
			}
			return dialer.DialContext(ctx, network, addr)
		}
	}
	return &http.Client{
		Transport: transport,
		Timeout:   inboundFetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= inboundMaxRedirects {
				return errors.New("too many redirects while downloading inbound file")
			}
			if _, err := validateInboundURL(req.URL.String(), s.allowInsecure); err != nil {
				return err
			}
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			return nil
		},
	}
}

func isConfiguredProxyAddr(addr string) bool {
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			continue
		}
		port := u.Port()
		if port == "" {
			if strings.EqualFold(u.Scheme, "https") {
				port = "443"
			} else {
				port = "80"
			}
		}
		if strings.EqualFold(net.JoinHostPort(u.Hostname(), port), addr) {
			return true
		}
	}
	return false
}

func validateInboundURL(rawURL string, allowInsecure bool) (*url.URL, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return nil, errors.New("inbound file download_url is required")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid inbound file download_url: %w", err)
	}
	if u.User != nil {
		return nil, errors.New("inbound file download_url must not contain userinfo")
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return nil, errors.New("inbound file download_url is missing host")
	}
	if allowInsecure {
		if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
			return nil, fmt.Errorf("unsupported inbound file scheme %q", u.Scheme)
		}
		return u, nil
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, fmt.Errorf("inbound file download_url must use https (got %q)", u.Scheme)
	}
	lowerHost := strings.ToLower(strings.TrimSuffix(host, "."))
	if lowerHost == "localhost" ||
		strings.HasSuffix(lowerHost, ".localhost") ||
		strings.HasSuffix(lowerHost, ".local") ||
		strings.HasSuffix(lowerHost, ".internal") {
		return nil, fmt.Errorf("blocked local inbound host %q", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if err := checkInboundIP(ip); err != nil {
			return nil, fmt.Errorf("blocked inbound IP %s: %w", host, err)
		}
	}
	return u, nil
}

func checkInboundIP(ip net.IP) error {
	if ip == nil {
		return errors.New("empty IP")
	}
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return errors.New("non-public IP address")
	}
	// Block CGNAT 100.64.0.0/10 (RFC 6598) and 0.0.0.0/8.
	// Note: 198.18.0.0/15 is intentionally permitted for transparent TUN proxy fake-IP resolvers.
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 0 {
			return errors.New("reserved IPv4 network")
		}
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return errors.New("CGNAT IPv4 address")
		}
	}
	return nil
}

// FetchAll downloads and stages every inbound file ref concurrently (bounded),
// deduplicating repeated file_ids already cached on disk.
func (s *inboundStore) FetchAll(ctx context.Context, refs []InboundFileRef) ([]jsrt.InboundFile, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if len(refs) > s.maxFiles {
		return nil, fmt.Errorf("too many inbound files (%d); maximum per run is %d", len(refs), s.maxFiles)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, fmt.Errorf("prepare inbound directory: %w", err)
	}
	s.maybePrune()

	out := make([]jsrt.InboundFile, len(refs))
	errs := make([]error, len(refs))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, ref := range refs {
		wg.Add(1)
		go func(idx int, r InboundFileRef) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errs[idx] = ctx.Err()
				return
			}
			f, err := s.fetchOne(ctx, r)
			if err != nil {
				errs[idx] = fmt.Errorf("files[%d] (%s): %w", idx, firstNonEmptyString(r.FileName, r.FileID, "unnamed"), err)
				return
			}
			out[idx] = f
		}(i, ref)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *inboundStore) fetchOne(ctx context.Context, ref InboundFileRef) (jsrt.InboundFile, error) {
	start := time.Now()
	fileID := strings.TrimSpace(ref.FileID)
	if fileID == "" {
		return jsrt.InboundFile{}, errors.New("file_id is required")
	}
	if _, err := validateInboundURL(ref.DownloadURL, s.allowInsecure); err != nil {
		return jsrt.InboundFile{}, err
	}

	// Reuse a verified local copy if this exact file_id was already staged
	// (avoids redundant downloads across approval retries or follow-up turns
	// and keeps files usable even after short-lived signed URLs expire).
	s.mu.Lock()
	if cached, ok := s.cached[fileID]; ok {
		if info, err := os.Stat(cached.Path); err == nil && !info.IsDir() && info.Size() == cached.Size {
			s.mu.Unlock()
			cached.DurationMS = time.Since(start).Milliseconds()
			return cached, nil
		}
		delete(s.cached, fileID)
	}
	s.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(ref.DownloadURL), nil)
	if err != nil {
		return jsrt.InboundFile{}, fmt.Errorf("build download request: %w", err)
	}
	req.Header.Set("User-Agent", "local-runtime-mcp/"+Version)

	resp, err := s.client.Do(req)
	if err != nil {
		return jsrt.InboundFile{}, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return jsrt.InboundFile{}, fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > s.maxBytes {
		return jsrt.InboundFile{}, fmt.Errorf("file size %d exceeds maximum allowed %d bytes", resp.ContentLength, s.maxBytes)
	}

	tmp, err := os.CreateTemp(s.dir, ".inbound-*.tmp")
	if err != nil {
		return jsrt.InboundFile{}, fmt.Errorf("create staging file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanupTmp := true
	defer func() {
		_ = tmp.Close()
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()
	_ = tmp.Chmod(0o600)

	hasher := sha256.New()
	sniff := make([]byte, 0, 512)
	limited := io.LimitReader(resp.Body, s.maxBytes+1)
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, readErr := limited.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > s.maxBytes {
				return jsrt.InboundFile{}, fmt.Errorf("file exceeds maximum allowed %d bytes", s.maxBytes)
			}
			chunk := buf[:n]
			if len(sniff) < 512 {
				need := 512 - len(sniff)
				if need > len(chunk) {
					need = len(chunk)
				}
				sniff = append(sniff, chunk[:need]...)
			}
			if _, wErr := tmp.Write(chunk); wErr != nil {
				return jsrt.InboundFile{}, fmt.Errorf("write staging file: %w", wErr)
			}
			_, _ = hasher.Write(chunk)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return jsrt.InboundFile{}, fmt.Errorf("read response stream: %w", readErr)
		}
	}
	if err := tmp.Close(); err != nil {
		return jsrt.InboundFile{}, fmt.Errorf("close staging file: %w", err)
	}

	mimeType := resolveInboundMIME(ref.MIMEType, resp.Header.Get("Content-Type"), sniff)
	rawName := strings.TrimSpace(ref.FileName)
	if rawName == "" {
		rawName = filenameFromContentDisposition(resp.Header.Get("Content-Disposition"))
	}
	safeName := sanitizeInboundFilename(rawName, mimeType)
	safeID := sanitizeInboundID(fileID)
	finalPath := filepath.Join(s.dir, safeID+"_"+safeName)
	if err := replaceFile(tmpPath, finalPath); err != nil {
		return jsrt.InboundFile{}, fmt.Errorf("finalize inbound file: %w", err)
	}
	cleanupTmp = false
	_ = os.Chmod(finalPath, 0o600)

	staged := jsrt.InboundFile{
		FileID:     fileID,
		Name:       safeName,
		Path:       finalPath,
		MIMEType:   mimeType,
		Size:       total,
		SHA256:     hex.EncodeToString(hasher.Sum(nil)),
		DurationMS: time.Since(start).Milliseconds(),
	}
	s.mu.Lock()
	s.cached[fileID] = staged
	s.mu.Unlock()
	return staged, nil
}

func (s *inboundStore) maybePrune() {
	s.mu.Lock()
	if !s.lastPrune.IsZero() && time.Since(s.lastPrune) < time.Hour {
		s.mu.Unlock()
		return
	}
	s.lastPrune = time.Now()
	s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-inboundRetention)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(s.dir, entry.Name()))
		}
	}
}

func replaceFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	_ = os.Remove(dst)
	return os.Rename(src, dst)
}

func resolveInboundMIME(declared, headerCT string, sniff []byte) string {
	for _, candidate := range []string{declared, headerCT} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if mediaType, _, err := mime.ParseMediaType(candidate); err == nil && mediaType != "" && mediaType != "application/octet-stream" {
			return strings.ToLower(mediaType)
		}
	}
	if len(sniff) > 0 {
		detected := http.DetectContentType(sniff)
		if mediaType, _, err := mime.ParseMediaType(detected); err == nil && mediaType != "" {
			return strings.ToLower(mediaType)
		}
	}
	if declared != "" {
		return strings.ToLower(strings.TrimSpace(declared))
	}
	return "application/octet-stream"
}

func filenameFromContentDisposition(cd string) string {
	if strings.TrimSpace(cd) == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(cd)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(params["filename"])
}

func sanitizeInboundID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
			if b.Len() >= 36 {
				break
			}
		}
	}
	if b.Len() >= 6 {
		return b.String()
	}
	sum := sha256.Sum256([]byte(id))
	return "f_" + hex.EncodeToString(sum[:8])
}

func sanitizeInboundFilename(name, mimeType string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	if name == "." || name == "/" || name == ".." {
		name = ""
	}
	var b strings.Builder
	runeCount := 0
	for _, r := range name {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			b.WriteByte('_')
		} else {
			b.WriteRune(r)
		}
		runeCount++
		if runeCount >= 96 {
			break
		}
	}
	clean := strings.Trim(b.String(), " .")
	if clean == "" {
		clean = "file" + extensionForMIME(mimeType)
	} else if filepath.Ext(clean) == "" {
		if ext := extensionForMIME(mimeType); ext != "" {
			clean += ext
		}
	}
	return clean
}

func extensionForMIME(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/svg+xml":
		return ".svg"
	case "application/pdf":
		return ".pdf"
	case "application/json":
		return ".json"
	case "application/zip":
		return ".zip"
	case "text/plain":
		return ".txt"
	case "text/markdown":
		return ".md"
	case "text/csv":
		return ".csv"
	case "text/html":
		return ".html"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "video/mp4":
		return ".mp4"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return ".xlsx"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return ".pptx"
	}
	if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ""
}

// redactInboundArgs strips sensitive signed query parameters from download_url
// entries inside `run` tool arguments before persisting raw args to audit logs.
func redactInboundArgs(raw []byte) []byte {
	if len(raw) == 0 || !strings.Contains(string(raw), "download_url") {
		return raw
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	filesVal, ok := obj["files"].([]any)
	if !ok || len(filesVal) == 0 {
		return raw
	}
	changed := false
	for _, item := range filesVal {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if rawURL, ok := m["download_url"].(string); ok && rawURL != "" {
			if u, err := url.Parse(rawURL); err == nil && u.RawQuery != "" {
				u.RawQuery = "redacted"
				m["download_url"] = u.String()
				changed = true
			}
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}
