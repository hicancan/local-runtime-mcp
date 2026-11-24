package browser

import (
	"encoding/json"
	"io"
	"net/http"
)

// GrantHandler answers the owner's grant panel in the extension, open on a page origin: "get" returns every web
// grant with the catalog they choose from and that page's site; "set" replaces one origin's grant and returns the same view. Only the extension, holding the
// bridge credential, reaches it; no web page does.
type GrantHandler func(method, origin string, grant json.RawMessage) (any, error)

// The panel reads and sets site/remote grants, approves or rejects proposed page
// tools, toggles live remote tunnels, and can stop desktop control.
var validGrantMethod = map[string]bool{
	"get":              true,
	"set":              true,
	"approve":          true,
	"reject":           true,
	"tunnel_up":        true,
	"tunnel_down":      true,
	"set_remote_grant": true,
	"stop_desktop":     true,
	"ui_bootstrap":     true,
}

func (b *Bridge) SetGrantHandler(handler GrantHandler) {
	b.mu.Lock()
	b.grants = handler
	b.mu.Unlock()
}

func (b *Bridge) receiveGrants(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !b.authorized(request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	b.mu.Lock()
	handler := b.grants
	b.mu.Unlock()
	if handler == nil {
		http.Error(writer, "grants are not available", http.StatusServiceUnavailable)
		return
	}
	var envelope struct {
		Method     string          `json:"method"`
		Origin     string          `json:"origin"`
		Grant      json.RawMessage `json:"grant"`
		Name       string          `json:"name"`
		TTLSeconds int             `json:"ttl_seconds"`
		Target     string          `json:"target"`
		Remote     json.RawMessage `json:"remote"`
	}
	data, err := io.ReadAll(io.LimitReader(request.Body, 256*1024))
	if err != nil || json.Unmarshal(data, &envelope) != nil || !validGrantMethod[envelope.Method] {
		http.Error(writer, "invalid grant request", http.StatusBadRequest)
		return
	}
	body := envelope.Grant
	switch envelope.Method {
	case "approve", "reject":
		body, _ = json.Marshal(envelope.Name)
	case "tunnel_up":
		body, _ = json.Marshal(map[string]any{"name": envelope.Name, "ttl_seconds": envelope.TTLSeconds})
	case "tunnel_down":
		body, _ = json.Marshal(map[string]any{"name": envelope.Name})
	case "set_remote_grant":
		body, _ = json.Marshal(map[string]any{"target": envelope.Target, "grant": envelope.Remote})
	case "stop_desktop", "ui_bootstrap":
		body = nil
	}
	result, err := handler(envelope.Method, envelope.Origin, body)
	writer.Header().Set("Content-Type", "application/json")
	if err != nil {
		writer.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(writer).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(writer).Encode(result)
}
