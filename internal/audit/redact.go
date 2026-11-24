package audit

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	maxFieldChars = 12 * 1024
	maxJSONChars  = 48 * 1024
)

var sensitiveKeyFragments = []string{
	"token", "secret", "password", "passwd", "api_key", "apikey",
	"authorization", "cookie", "private_key", "credential", "owner_token",
}

func isSensitiveKey(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(lower, "local_runtime_mcp_") {
		return true
	}
	for _, frag := range sensitiveKeyFragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}

func redactValue(key string, v any, depth int) any {
	if depth > 10 {
		return "[TRUNCATED]"
	}
	if key != "" && isSensitiveKey(key) {
		return "[REDACTED]"
	}
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, child := range val {
			if isSensitiveKey(k) {
				out[k] = "[REDACTED]"
				continue
			}
			out[k] = redactValue(k, child, depth+1)
		}
		return out
	case []any:
		limit := len(val)
		if limit > 120 {
			limit = 120
		}
		out := make([]any, limit)
		for i := 0; i < limit; i++ {
			out[i] = redactValue("", val[i], depth+1)
		}
		return out
	case string:
		if len(val) > maxFieldChars {
			return val[:maxFieldChars] + "…[truncated]"
		}
		return val
	default:
		return val
	}
}

// RedactRaw parses arbitrary JSON, masks secret fields and sensitive env vars,
// bounds payload size, and returns a compact JSON string safe for local persistence.
func RedactRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		s := string(raw)
		if len(s) > 1024 {
			s = s[:1024] + "…"
		}
		b, _ := json.Marshal(map[string]string{"raw": s})
		return string(b)
	}
	return RedactAny(parsed)
}

// RedactAny redacts and serializes any Go value into bounded JSON.
func RedactAny(v any) string {
	if v == nil {
		return "{}"
	}
	// Normalize typed structs through JSON so redaction covers every field uniformly.
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return "{}"
	}
	cleaned := redactValue("", generic, 0)
	out, err := json.Marshal(cleaned)
	if err != nil {
		return "{}"
	}
	if len(out) > maxJSONChars {
		preview := string(out[:maxJSONChars])
		wrapped, _ := json.Marshal(map[string]any{
			"truncated": true,
			"bytes":     len(out),
			"preview":   preview,
		})
		return string(wrapped)
	}
	return string(out)
}

// SummarizeTool generates a concise single-line human summary for timeline rows.
func SummarizeTool(name string, rawArgs json.RawMessage) string {
	var args map[string]any
	_ = json.Unmarshal(rawArgs, &args)
	str := func(k string) string {
		v, _ := args[k].(string)
		return strings.TrimSpace(v)
	}
	clip := func(s string, max int) string {
		s = strings.ReplaceAll(s, "\r\n", " ")
		s = strings.ReplaceAll(s, "\n", " ")
		r := []rune(s)
		if len(r) > max {
			return string(r[:max]) + "…"
		}
		return s
	}

	switch name {
	case "agent_report":
		if msg := str("message"); msg != "" {
			return clip(msg, 140)
		}
		return "status update"
	case "process_run":
		prog := str("program")
		if rawList, ok := args["args"].([]any); ok && len(rawList) > 0 {
			parts := make([]string, 0, len(rawList)+1)
			parts = append(parts, prog)
			for _, item := range rawList {
				if s, ok := item.(string); ok {
					parts = append(parts, s)
				}
			}
			return clip(strings.Join(parts, " "), 120)
		}
		return clip("run "+prog, 120)
	case "process_continue":
		if term, _ := args["terminate"].(bool); term {
			return "terminate session " + clip(str("session_id"), 24)
		}
		return "continue session " + clip(str("session_id"), 24)
	case "filesystem_read_text":
		return "read " + clip(str("path"), 110)
	case "filesystem_write_text":
		return fmt.Sprintf("write(%s) %s", str("mode"), clip(str("path"), 100))
	case "filesystem_patch_text":
		return "patch " + clip(str("path"), 110)
	case "filesystem_list":
		return "list " + clip(str("path"), 110)
	case "filesystem_stat":
		return "stat " + clip(str("path"), 110)
	case "filesystem_search_text":
		return fmt.Sprintf("search %q in %s", clip(str("query"), 40), clip(str("path"), 70))
	case "image_read":
		return "read image " + clip(str("path"), 100)
	case "browser_status":
		return "check browser profiles"
	case "browser_tabs":
		return "list browser tabs"
	case "browser_open":
		return "open " + clip(str("url"), 110)
	case "browser_close":
		return "close tab " + clip(str("tab_id"), 32)
	case "browser_navigate":
		if u := str("url"); u != "" {
			return "navigate " + clip(u, 110)
		}
		return "navigate " + str("kind")
	case "browser_snapshot":
		return "snapshot page " + clip(str("tab_id"), 32)
	case "browser_screenshot":
		return "screenshot page " + clip(str("tab_id"), 32)
	case "browser_action":
		return "browser." + str("kind")
	case "computer_targets":
		if p := str("process"); p != "" {
			return "list windows (" + clip(p, 40) + ")"
		}
		return "list displays & windows"
	case "computer_state":
		if tid := str("target_id"); tid != "" {
			return "observe window " + clip(tid, 32)
		}
		return "observe desktop"
	case "computer_wait":
		return "wait for desktop element"
	case "computer_action":
		return "desktop." + str("kind")
	case "computer_control":
		return "desktop control " + str("kind")
	case "capability_propose":
		return "propose page tool " + clip(str("name"), 64)
	default:
		return clip(name, 100)
	}
}
