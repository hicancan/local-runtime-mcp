package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DiscoveredBrowser describes one local Chromium browser exposing browser-level CDP.
type DiscoveredBrowser struct {
	ID          string
	Label       string
	WSURL       string
	HTTPURL     string
	Fingerprint string
	Source      string
}

type activePortCandidate struct {
	id    string
	label string
	path  string
}

func defaultActivePortCandidates() []activePortCandidate {
	switch runtime.GOOS {
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			return nil
		}
		return []activePortCandidate{
			{id: "edge", label: "Microsoft Edge", path: filepath.Join(local, "Microsoft", "Edge", "User Data", "DevToolsActivePort")},
			{id: "chrome", label: "Google Chrome", path: filepath.Join(local, "Google", "Chrome", "User Data", "DevToolsActivePort")},
			{id: "edge-beta", label: "Microsoft Edge Beta", path: filepath.Join(local, "Microsoft", "Edge Beta", "User Data", "DevToolsActivePort")},
			{id: "edge-dev", label: "Microsoft Edge Dev", path: filepath.Join(local, "Microsoft", "Edge Dev", "User Data", "DevToolsActivePort")},
			{id: "chrome-canary", label: "Google Chrome Canary", path: filepath.Join(local, "Google", "Chrome SxS", "User Data", "DevToolsActivePort")},
			{id: "brave", label: "Brave", path: filepath.Join(local, "BraveSoftware", "Brave-Browser", "User Data", "DevToolsActivePort")},
		}
	case "darwin":
		home, _ := os.UserHomeDir()
		if home == "" {
			return nil
		}
		base := filepath.Join(home, "Library", "Application Support")
		return []activePortCandidate{
			{id: "chrome", label: "Google Chrome", path: filepath.Join(base, "Google", "Chrome", "DevToolsActivePort")},
			{id: "edge", label: "Microsoft Edge", path: filepath.Join(base, "Microsoft Edge", "DevToolsActivePort")},
			{id: "chrome-canary", label: "Google Chrome Canary", path: filepath.Join(base, "Google", "Chrome Canary", "DevToolsActivePort")},
			{id: "brave", label: "Brave", path: filepath.Join(base, "BraveSoftware", "Brave-Browser", "DevToolsActivePort")},
		}
	default:
		home, _ := os.UserHomeDir()
		if home == "" {
			return nil
		}
		base := filepath.Join(home, ".config")
		return []activePortCandidate{
			{id: "chrome", label: "Google Chrome", path: filepath.Join(base, "google-chrome", "DevToolsActivePort")},
			{id: "chromium", label: "Chromium", path: filepath.Join(base, "chromium", "DevToolsActivePort")},
			{id: "edge", label: "Microsoft Edge", path: filepath.Join(base, "microsoft-edge", "DevToolsActivePort")},
			{id: "brave", label: "Brave", path: filepath.Join(base, "BraveSoftware", "Brave-Browser", "DevToolsActivePort")},
		}
	}
}

func readDevToolsActivePort(path string) (string, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		return "", "", errors.New("DevToolsActivePort has fewer than two lines")
	}
	portStr := strings.TrimSpace(lines[0])
	wsPath := strings.TrimSpace(lines[1])
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", "", fmt.Errorf("invalid DevToolsActivePort port %q", portStr)
	}
	if !strings.HasPrefix(wsPath, "/") {
		return "", "", fmt.Errorf("invalid DevToolsActivePort path %q", wsPath)
	}
	wsURL := fmt.Sprintf("ws://127.0.0.1:%d%s", port, wsPath)
	return wsURL, portStr + ":" + wsPath, nil
}

// DiscoverBrowsers locates local Chromium instances exposing browser-level CDP
// via DevToolsActivePort files and any explicitly configured endpoints.
func DiscoverBrowsers(explicit []string, disableAuto bool) []DiscoveredBrowser {
	var out []DiscoveredBrowser
	seen := map[string]bool{}

	for i, raw := range explicit {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		id := fmt.Sprintf("cdp-%d", i+1)
		label := "Configured CDP"
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			label = "CDP " + u.Host
		}
		item := DiscoveredBrowser{
			ID:          id,
			Label:       label,
			Fingerprint: raw,
			Source:      "config",
		}
		if strings.HasPrefix(raw, "ws://") || strings.HasPrefix(raw, "wss://") {
			item.WSURL = raw
		} else {
			item.HTTPURL = strings.TrimRight(raw, "/")
		}
		if !seen[item.Fingerprint] {
			seen[item.Fingerprint] = true
			out = append(out, item)
		}
	}

	if disableAuto {
		return out
	}

	for _, cand := range defaultActivePortCandidates() {
		wsURL, fp, err := readDevToolsActivePort(cand.path)
		if err != nil || seen[fp] {
			continue
		}
		seen[fp] = true
		out = append(out, DiscoveredBrowser{
			ID:          cand.id,
			Label:       cand.label,
			WSURL:       wsURL,
			Fingerprint: fp,
			Source:      cand.path,
		})
	}
	return out
}

func resolveHTTPCDPURL(ctx context.Context, httpBase string) (string, string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.TrimRight(httpBase, "/")+"/json/version", nil)
	if err != nil {
		return "", "", err
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("CDP /json/version returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Browser              string `json:"Browser"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", "", err
	}
	if payload.WebSocketDebuggerURL == "" {
		return "", "", errors.New("CDP /json/version omitted webSocketDebuggerUrl")
	}
	return payload.WebSocketDebuggerURL, payload.Browser, nil
}
