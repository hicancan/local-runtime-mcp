package audit

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	maxFieldChars = 12 * 1024
	maxJSONChars  = 48 * 1024
)

func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	i := maxBytes
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

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
			return truncateUTF8(val, maxFieldChars) + "…[truncated]"
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
			s = truncateUTF8(s, 1024) + "…"
		}
		b, _ := json.Marshal(map[string]string{"raw": s})
		return string(b)
	}
	return marshalRedacted(parsed)
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
	return marshalRedacted(generic)
}

func marshalRedacted(generic any) string {
	cleaned := redactValue("", generic, 0)
	out, err := json.Marshal(cleaned)
	if err != nil {
		return "{}"
	}
	if len(out) > maxJSONChars {
		preview := truncateUTF8(string(out), maxJSONChars)
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
	case "run":
		if title := str("title"); title != "" {
			return clip(title, 140)
		}
		return "run script"
	case "wait":
		cid := clip(str("cell_id"), 24)
		if term, _ := args["terminate"].(bool); term {
			return "terminate cell " + cid
		}
		return "wait cell " + cid
	case "session":
		if msg := str("message"); msg != "" {
			return clip(msg, 140)
		}
		if title := str("title"); title != "" {
			return clip(title, 140)
		}
		if act := str("action"); act != "" {
			return "session " + act
		}
		return "session update"
	case "capability_propose":
		return "propose page tool " + clip(str("name"), 64)
	default:
		return clip(name, 100)
	}
}
