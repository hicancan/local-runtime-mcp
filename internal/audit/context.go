package audit

import (
	"context"
	"net/http"
	"strings"
)

// ToolPolicy reports whether a tool with the given name and annotations may be
// seen and invoked under the current caller credential.
type ToolPolicy func(name string, readOnly, destructive bool) bool

// CallMeta carries transport-level caller attribution and optional per-token
// authorization down to MCP middleware and tool handlers.
type CallMeta struct {
	Wire       string
	AgentID    string
	SessionID  string
	ClientName string
	Policy     ToolPolicy
}

type metaKey struct{}

// WithCallMeta attaches caller attribution to ctx.
func WithCallMeta(ctx context.Context, meta CallMeta) context.Context {
	return context.WithValue(ctx, metaKey{}, meta)
}

// CallMetaFromContext retrieves caller attribution from ctx, if any.
func CallMetaFromContext(ctx context.Context) CallMeta {
	if ctx == nil {
		return CallMeta{}
	}
	meta, _ := ctx.Value(metaKey{}).(CallMeta)
	return meta
}

// SanitizeID cleans an external agent/session header into a safe bounded identifier.
func SanitizeID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ':', r == '/':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
		if b.Len() >= 64 {
			break
		}
	}
	return strings.Trim(b.String(), "-_.:/")
}

// HeaderAgentID extracts an optional agent identifier from X-LRMCP-Agent or
// X-Agent-Id, falling back to fallback when absent or empty.
func HeaderAgentID(h http.Header, fallback string) string {
	if h != nil {
		if v := SanitizeID(h.Get("X-LRMCP-Agent")); v != "" {
			return v
		}
		if v := SanitizeID(h.Get("X-Agent-Id")); v != "" {
			return v
		}
	}
	return fallback
}

// HeaderSessionID extracts an optional session grouping key from X-LRMCP-Session
// or Mcp-Session-Id.
func HeaderSessionID(h http.Header) string {
	if h == nil {
		return ""
	}
	if v := SanitizeID(h.Get("X-LRMCP-Session")); v != "" {
		return v
	}
	return SanitizeID(h.Get("Mcp-Session-Id"))
}
