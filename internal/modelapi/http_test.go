package modelapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testKey = "model-api-fixture-key-0123456789abcdef"

func postRequest(t *testing.T, server *httptest.Server, ctx context.Context, request Request) *http.Response {
	t.Helper()
	data, _ := json.Marshal(request)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	client := *server.Client()
	client.Timeout = 5 * time.Second
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func nextEvent(reader *bufio.Reader) (map[string]any, bool, error) {
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, false, err
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if data == "[DONE]" {
			return nil, true, nil
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return nil, false, err
		}
		return event, false, nil
	}
}
func firstChoice(t *testing.T, event map[string]any) map[string]any {
	t.Helper()
	if event["conversation_id"] != "test-conversation" {
		t.Fatal("completion chunk lost conversation identity")
	}
	choices, ok := event["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatal("missing completion choice")
	}
	return choices[0].(map[string]any)
}

func TestHTTPAuthenticatesAndValidatesBeforeAnySSEOrBrowserWork(t *testing.T) {
	s, _, b := newFixture(t)
	h := &HTTP{Service: s, Key: testKey}
	for _, tc := range []struct {
		name, body, authorization, origin string
		status                            int
	}{
		{"missing-key", `{}`, "", "", 401},
		{"wrong-key", `{}`, "Bearer fixture-wrong-key", "", 401},
		{"browser-origin", `{}`, "Bearer " + testKey, "https://consumer.example.test", 403},
		{"unknown-model", `{"model":"absent","messages":[{"role":"user","content":"q"}],"stream":true}`, "Bearer " + testKey, "", 404},
		{"format", `{"model":"web-test","messages":[{"role":"user","content":"q"}],"stream":true,"response_format":{"type":"json_object"}}`, "Bearer " + testKey, "", 400},
		{"usage", `{"model":"web-test","messages":[{"role":"user","content":"q"}],"stream":true,"stream_options":{"include_usage":true}}`, "Bearer " + testKey, "", 400},
		{"unknown-field", `{"model":"web-test","messages":[{"role":"user","content":"q"}],"stream":true,"unexpected":"sensitive-fixture-data"}`, "Bearer " + testKey, "", 400},
		{"bad-json", `{"model":"sensitive-fixture-data",`, "Bearer " + testKey, "", 400},
		{"dangling-tool-result", `{"model":"web-test","conversation_id":"test-conversation","messages":[{"role":"tool","tool_call_id":"unrequested","content":"result"}],"stream":true}`, "Bearer " + testKey, "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			r.Header.Set("Authorization", tc.authorization)
			r.Header.Set("Origin", tc.origin)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || strings.Contains(w.Body.String(), "data:") || strings.Contains(w.Body.String(), "sensitive-fixture-data") {
				t.Fatal("invalid request did not fail safely before streaming:", w.Code, w.Body.String())
			}
		})
	}
	if opened, _, _ := b.counts(); opened != 0 {
		t.Fatal("rejected HTTP request opened a browser tab")
	}
}

func TestHTTPModelDiscoveryAndNonStreamingCompletion(t *testing.T) {
	s, _, _ := newFixture(t)
	server := httptest.NewServer(&HTTP{Service: s, Key: testKey})
	defer server.Close()
	r, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+testKey)
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(data), `"id":"web-test"`) || strings.Contains(string(data), "model.example.test") || strings.Contains(string(data), testKey) {
		t.Fatal("model discovery exposed source configuration or failed")
	}
	response = postRequest(t, server, context.Background(), request())
	defer response.Body.Close()
	var body struct {
		ConversationID string `json:"conversation_id"`
		Object         string `json:"object"`
		Choices        []struct {
			Message Assistant `json:"message"`
			Finish  string    `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.NewDecoder(response.Body).Decode(&body) != nil || response.StatusCode != 200 || body.ConversationID != "test-conversation" || body.Object != "chat.completion" || len(body.Choices) != 1 || deref(body.Choices[0].Message.Content) != "answer" || body.Choices[0].Finish != "stop" {
		t.Fatal("nonstreaming completion did not follow the wire contract")
	}
}

func TestSSEFlushesActualIncrementalTextBeforeTheSourceCompletes(t *testing.T) {
	s, f, _ := newFixture(t)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	reads := 0
	f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.read" && f.count("source.start") > 0 {
			reads++
			switch reads {
			case 1:
				return state("generating", "你"), nil
			case 2:
				return state("generating", "你好"), nil
			default:
				select {
				case <-release:
					return state("complete", "你好世界"), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}
		return f.defaultCall(ctx, call)
	}
	server := httptest.NewServer(&HTTP{Service: s, Key: testKey})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r := request()
	r.Stream = true
	response := postRequest(t, server, ctx, r)
	defer response.Body.Close()
	if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatal("missing SSE content type")
	}
	reader := bufio.NewReader(response.Body)
	var text strings.Builder
	finish := ""
	for text.Len() == 0 {
		event, done, err := nextEvent(reader)
		if err != nil || done {
			t.Fatal("source was not streamed before completion:", err)
		}
		choice := firstChoice(t, event)
		if delta, ok := choice["delta"].(map[string]any)["content"].(string); ok {
			text.WriteString(delta)
		}
	}
	if text.String() != "你" || f.count("source.start") != 1 {
		t.Fatal("expected a stable Unicode prefix before the blocked source completion")
	}
	close(release)
	for {
		event, done, err := nextEvent(reader)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
		choice := firstChoice(t, event)
		if delta, ok := choice["delta"].(map[string]any)["content"].(string); ok {
			text.WriteString(delta)
		}
		if value, ok := choice["finish_reason"].(string); ok {
			finish = value
		}
	}
	if text.String() != "你好世界" || finish != "stop" {
		t.Fatal("SSE deltas did not reassemble to the source answer")
	}
}

func TestSSEToolCallsUseIndexedArgumentsAndToolFinishWithoutProtocolText(t *testing.T) {
	s, f, _ := newFixture(t)
	f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.read" && f.count("source.start") > 0 {
			return state("complete", `{"content":null,"tool_calls":[{"name":"lookup","arguments":{"room":"A"}}]}`), nil
		}
		return f.defaultCall(ctx, call)
	}
	server := httptest.NewServer(&HTTP{Service: s, Key: testKey})
	defer server.Close()
	r := request()
	r.Stream, r.Tools = true, []Tool{functionTool("lookup")}
	response := postRequest(t, server, context.Background(), r)
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	calls, finish := 0, ""
	for {
		event, done, err := nextEvent(reader)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
		choice := firstChoice(t, event)
		delta := choice["delta"].(map[string]any)
		if text, _ := delta["content"].(string); text != "" {
			t.Fatal("internal tool JSON was exposed as assistant text")
		}
		if values, ok := delta["tool_calls"].([]any); ok {
			call := values[0].(map[string]any)
			function := call["function"].(map[string]any)
			if call["index"] != float64(0) || call["id"] == "" || call["type"] != "function" || function["name"] != "lookup" || function["arguments"] != `{"room":"A"}` {
				t.Fatal("tool-call chunk is not OpenAI-compatible")
			}
			calls += len(values)
		}
		if value, ok := choice["finish_reason"].(string); ok {
			finish = value
		}
	}
	if calls != 1 || finish != "tool_calls" {
		t.Fatal("tool stream lost its result or terminal reason")
	}
}

func TestSSEMidstreamFailureIsSanitizedAndNeverReportsSuccessfulDone(t *testing.T) {
	s, f, b := newFixture(t)
	f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.read" && f.count("source.start") > 0 {
			return nil, errors.New("sensitive-source-detail")
		}
		return f.defaultCall(ctx, call)
	}
	server := httptest.NewServer(&HTTP{Service: s, Key: testKey})
	defer server.Close()
	r := request()
	r.Stream = true
	response := postRequest(t, server, context.Background(), r)
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	text := string(data)
	if response.StatusCode != 200 || !strings.Contains(text, `"error"`) || strings.Contains(text, "sensitive-source-detail") || strings.Contains(text, "[DONE]") || strings.Contains(text, `"finish_reason":"stop"`) {
		t.Fatal("failed stream leaked detail or claimed success:", text)
	}
	_, closed, _ := b.counts()
	if f.count("source.start") != 1 || f.count("source.stop") != 1 || closed != 0 {
		t.Fatal("stream failure replayed or did not clean up")
	}
}

func TestHTTPClientDisconnectCancelsSourceAndCleansUp(t *testing.T) {
	s, f, b := newFixture(t)
	started, stopped, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.start" {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if call.name == "source.stop" {
			if ctx.Err() != nil {
				return nil, errors.New("cleanup reused dead context")
			}
			close(stopped)
		}
		return f.defaultCall(ctx, call)
	}
	b.closeHook = func(ctx context.Context, _ string) error {
		if ctx.Err() != nil {
			t.Error("close reused canceled HTTP context")
		}
		close(closed)
		return nil
	}
	server := httptest.NewServer(&HTTP{Service: s, Key: testKey})
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	r := request()
	r.Stream = true
	response := postRequest(t, server, ctx, r)
	<-started
	cancel()
	response.Body.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("HTTP disconnect did not reach independent cleanup")
	}
	if _, count, _ := b.counts(); count != 0 {
		t.Fatal("HTTP disconnect closed the reusable conversation")
	}
	deleteRequest, _ := http.NewRequest(http.MethodDelete, server.URL+"/v1/conversations/test-conversation?model=web-test", nil)
	deleteRequest.Header.Set("Authorization", "Bearer "+testKey)
	deleteResponse, err := server.Client().Do(deleteRequest)
	if err != nil {
		t.Fatal(err)
	}
	deleteResponse.Body.Close()
	if deleteResponse.StatusCode != http.StatusNoContent {
		t.Fatal("explicit HTTP conversation close failed")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("explicit close did not close the owned tab")
	}
	if f.count("source.start") != 1 || f.count("source.stop") != 1 {
		t.Fatal("HTTP disconnect retried the source or skipped stop")
	}
}

func TestHTTPCachedRetryAndScopedDeletePreserveConversationIdentity(t *testing.T) {
	s, f, b := newFixture(t)
	server := httptest.NewServer(&HTTP{Service: s, Key: testKey})
	defer server.Close()
	r := request()
	first := postRequest(t, server, context.Background(), r)
	io.Copy(io.Discard, first.Body)
	first.Body.Close()
	r.Retry = true
	retry := postRequest(t, server, context.Background(), r)
	var result map[string]any
	if json.NewDecoder(retry.Body).Decode(&result) != nil {
		t.Fatal("invalid cached HTTP response")
	}
	retry.Body.Close()
	if retry.StatusCode != 200 || result["conversation_id"] != r.ConversationID || f.count("source.start") != 1 {
		t.Fatal("HTTP retry did not return cached authority without another model send")
	}
	if _, err := s.Generate(context.Background(), r, "", "web-scope", nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		model  string
		status int
	}{{"not-owned", 204}, {r.Model, 204}, {r.Model, 204}} {
		req, _ := http.NewRequest(http.MethodDelete, server.URL+"/v1/conversations/"+r.ConversationID+"?model="+tc.model, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != tc.status {
			t.Fatal("close did not enforce the scoped model binding")
		}
	}
	opened, closed, _ := b.counts()
	if opened != 2 || closed != 1 {
		t.Fatal("API close crossed into another scope")
	}
	if _, err := s.Generate(context.Background(), r, "", "web-scope", nil); err != nil {
		t.Fatal("other scope was damaged:", err)
	}
}
