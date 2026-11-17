package modelapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTP authenticates only model requests, never MCP or machine control.
type HTTP struct {
	Service *Service
	Key     string
}

func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Header.Get("Origin") != "" {
		writeError(w, failure("browser_origin_forbidden", "Use the authorized extension bridge for web applications.", 403))
		return
	}
	provided, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(h.Key) < 32 || !ok || len(provided) != len(h.Key) || subtle.ConstantTimeCompare([]byte(provided), []byte(h.Key)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="local-runtime-models"`)
		writeError(w, failure("invalid_api_key", "A valid local model API key is required.", 401))
		return
	}
	if h.Service == nil {
		writeError(w, failure("model_unavailable", "Local model sources are unavailable.", 503))
		return
	}
	if r.URL.Path == "/v1/models" && r.Method == http.MethodGet {
		writeJSON(w, map[string]any{"object": "list", "data": h.Service.List()})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/conversations/") {
		if r.Method != http.MethodDelete {
			writeError(w, failure("method_not_allowed", "Use DELETE to close a conversation.", 405))
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/conversations/")
		if err := h.Service.CloseConversation(r.Context(), "api", r.URL.Query().Get("model"), id); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path != "/v1/chat/completions" {
		writeError(w, failure("not_found", "Unknown model endpoint.", 404))
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, failure("method_not_allowed", "Use POST for chat completions.", 405))
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		writeError(w, failure("invalid_request", "Request body exceeds the allowed size.", 400))
		return
	}
	request, err := DecodeRequest(data)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := h.Service.ValidateRequest(request); err != nil {
		writeError(w, err)
		return
	}
	if !request.Stream {
		answer, err := h.Service.Generate(r.Context(), request, "", "api", nil)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, Completion(request.Model, answer))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, failure("stream_unavailable", "Streaming is unavailable on this connection.", 500))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	id, created := "chatcmpl_"+randomID(), time.Now().Unix()
	send := func(delta any, finish any) error {
		body, _ := json.Marshal(map[string]any{"id": id, "conversation_id": request.ConversationID, "object": "chat.completion.chunk", "created": created, "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		if _, err := fmt.Fprintf(w, "data: %s\n\n", body); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	if send(map[string]any{"role": "assistant"}, nil) != nil {
		return
	}
	lastWrite := time.Now()
	answer, err := h.Service.Generate(r.Context(), request, "", "api", func(delta Delta) error {
		if delta.Content == "" && len(delta.ToolCalls) == 0 {
			if time.Since(lastWrite) > 10*time.Second {
				if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
					return err
				}
				flusher.Flush()
				lastWrite = time.Now()
			}
			return nil
		}
		wire := map[string]any{}
		if delta.Content != "" {
			wire["content"] = delta.Content
		}
		if len(delta.ToolCalls) > 0 {
			calls := make([]any, 0, len(delta.ToolCalls))
			for i, call := range delta.ToolCalls {
				calls = append(calls, map[string]any{"index": i, "id": call.ID, "type": "function", "function": call.Function})
			}
			wire["tool_calls"] = calls
		}
		lastWrite = time.Now()
		return send(wire, nil)
	})
	if err != nil {
		if r.Context().Err() == nil {
			value := errorBody(err)
			value["conversation_id"] = request.ConversationID
			body, _ := json.Marshal(value)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
			flusher.Flush()
		}
		return
	}
	finish := "stop"
	if len(answer.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	if send(map[string]any{}, finish) == nil {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
}

func Completion(model string, answer Assistant) map[string]any {
	finish := "stop"
	if len(answer.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	return map[string]any{"id": "chatcmpl_" + randomID(), "conversation_id": answer.ConversationID, "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "message": answer, "finish_reason": finish}}}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}
func errorBody(err error) map[string]any {
	code, message := "model_source_error", "The local model source failed; no request was replayed."
	var modelError *Error
	if errors.As(err, &modelError) {
		code, message = modelError.Code, modelError.Message
	} else if errors.Is(err, context.DeadlineExceeded) {
		code, message = "model_timeout", "The model request timed out; source stop was requested."
	} else if errors.Is(err, context.Canceled) {
		code, message = "request_cancelled", "The model request was cancelled."
	}
	return map[string]any{"error": map[string]any{"message": message, "type": "local_runtime_error", "code": code}}
}
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	var modelError *Error
	if errors.As(err, &modelError) {
		status = modelError.Status
	} else if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody(err))
}
