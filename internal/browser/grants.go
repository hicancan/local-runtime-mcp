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

// The panel reads and sets grants, and approves or rejects page tools an AI proposed.
var validGrantMethod = map[string]bool{"get": true, "set": true, "approve": true, "reject": true}

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
		Method string          `json:"method"`
		Origin string          `json:"origin"`
		Grant  json.RawMessage `json:"grant"`
		Name   string          `json:"name"`
	}
	data, err := io.ReadAll(io.LimitReader(request.Body, 256*1024))
	if err != nil || json.Unmarshal(data, &envelope) != nil || !validGrantMethod[envelope.Method] {
		http.Error(writer, "invalid grant request", http.StatusBadRequest)
		return
	}
	body := envelope.Grant
	if envelope.Method == "approve" || envelope.Method == "reject" {
		body, _ = json.Marshal(envelope.Name)
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
