package browser

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// WebSource is supplied by our extension's real content-script sender, never by
// the page SDK. Only top-level HTTP(S) documents are admitted.
type WebSource struct {
	BrowserID  string `json:"browser_id,omitempty"`
	TabID      string `json:"tab_id"`
	Origin     string `json:"origin"`
	URL        string `json:"url"`
	FrameID    int    `json:"frame_id"`
	DocumentID string `json:"document_id"`
}

type WebRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type WebEvent struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

// WebHandler is installed by the owner-configured Runtime Host. It must apply
// exact-origin grants before disclosing definitions or invoking any capability.
// Its context ends when the requesting page disconnects/cancels the request.
type WebHandler func(context.Context, WebSource, WebRequest, func(WebEvent) error) (any, error)

func (b *Bridge) SetWebHandler(handler WebHandler) {
	b.mu.Lock()
	b.webHandler = handler
	b.mu.Unlock()
}

type webEnvelope struct {
	InstanceID string     `json:"instance_id"`
	BootID     string     `json:"boot_id"`
	Source     WebSource  `json:"source"`
	Request    WebRequest `json:"request"`
}

func (b *Bridge) receiveWeb(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !b.authorized(request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	var envelope webEnvelope
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 2*1024*1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil || decoder.Decode(new(any)) != io.EOF || !validWebEnvelope(envelope) {
		http.Error(writer, "invalid web request or sender identity", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	instance := b.instances[envelope.InstanceID]
	if instance == nil || instance.socket == nil || instance.peer.BootID != envelope.BootID || time.Since(instance.lastSeen) >= 35*time.Second {
		b.mu.Unlock()
		http.Error(writer, "browser generation is offline or stale", http.StatusConflict)
		return
	}
	handler := b.webHandler
	if handler == nil {
		b.mu.Unlock()
		http.Error(writer, "web capability access is not enabled by the owner", http.StatusForbidden)
		return
	}
	if old, exists := b.tabs[envelope.Source.TabID]; exists && (old.browserID != envelope.InstanceID || old.bootID != envelope.BootID) {
		b.mu.Unlock()
		http.Error(writer, "sender tab belongs to a different browser generation", http.StatusConflict)
		return
	}
	if b.webActive >= 32 {
		b.mu.Unlock()
		http.Error(writer, "web request capacity reached", http.StatusTooManyRequests)
		return
	}
	b.webActive++
	b.tabs[envelope.Source.TabID] = tabRoute{envelope.InstanceID, envelope.BootID}
	b.mu.Unlock()
	defer func() { b.mu.Lock(); b.webActive--; b.mu.Unlock() }()
	envelope.Source.BrowserID = envelope.InstanceID
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Minute)
	defer cancel()
	writer.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	var mutex sync.Mutex
	total := 0
	ended := false
	write := func(value any, final bool) error {
		mutex.Lock()
		defer mutex.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if ended {
			return errors.New("web request already completed")
		}
		body, err := json.Marshal(value)
		if err != nil {
			return errors.New("web result cannot be encoded")
		}
		if len(body) > 2*1024*1024 || total+len(body) > 64*1024*1024 {
			cancel()
			return errors.New("web result capacity exceeded")
		}
		total += len(body)
		_, err = writer.Write(append(body, '\n'))
		if err != nil {
			cancel()
			return err
		}
		ended = final
		return http.NewResponseController(writer).Flush()
	}
	result, err := handler(ctx, envelope.Source, envelope.Request, func(event WebEvent) error {
		if event.Type == "" {
			return errors.New("web event type is required")
		}
		return write(map[string]any{"type": "event", "data": event}, false)
	})
	if err != nil {
		failure := map[string]any{"type": "error", "error": err.Error()}
		var coded interface{ ErrorCode() string }
		if errors.As(err, &coded) && coded.ErrorCode() != "" {
			failure["code"] = coded.ErrorCode()
		}
		_ = write(failure, true)
		return
	}
	_ = write(map[string]any{"type": "result", "result": result}, true)
}

func validWebEnvelope(v webEnvelope) bool {
	if v.InstanceID == "" || len(v.InstanceID) > 128 || v.BootID == "" || len(v.BootID) > 128 || len(v.Source.TabID) > 256 ||
		!strings.HasPrefix(v.Source.TabID, v.BootID+".") || v.Source.TabID == v.BootID+"." || v.Source.BrowserID != "" ||
		v.Source.FrameID != 0 || v.Source.DocumentID == "" || len(v.Source.DocumentID) > 256 || len(v.Source.URL) > 8192 ||
		len(v.Source.Origin) > 1024 || v.Request.ID == "" || len(v.Request.ID) > 100 {
		return false
	}
	for _, ch := range v.Request.ID {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return false
		}
	}
	switch v.Request.Method {
	case "capabilities.list", "capabilities.invoke", "models.list", "models.chat", "models.close":
	default:
		return false
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(v.Request.Params, &params) != nil || params == nil {
		return false
	}
	page, err := url.Parse(v.Source.URL)
	if err != nil || page.User != nil || page.Host == "" || page.Scheme != "http" && page.Scheme != "https" {
		return false
	}
	return v.Source.Origin == page.Scheme+"://"+page.Host
}
