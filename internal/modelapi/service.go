// Package modelapi projects registered browser capabilities as a model source.
// It owns no agent loop: callers execute any returned tool calls themselves.
package modelapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/hicancan/local-runtime-mcp/internal/browser"
)

type Model struct {
	ID             string `yaml:"id" json:"id"`
	Title          string `yaml:"title,omitempty" json:"title,omitempty"`
	BrowserID      string `yaml:"browser_id,omitempty" json:"-"`
	URL            string `yaml:"url" json:"-"`
	Start          string `yaml:"start" json:"-"`
	Read           string `yaml:"read" json:"-"`
	Stop           string `yaml:"stop" json:"-"`
	TimeoutSeconds int    `yaml:"timeout_seconds,omitempty" json:"-"`
}

type Config struct {
	Key    string  `yaml:"key,omitempty"`
	Models []Model `yaml:"items,omitempty"`
}

type Capabilities interface {
	Call(context.Context, string, string, string, map[string]any) (map[string]any, error)
}

type Browser interface {
	Status() browser.Status
	Open(context.Context, string, string, bool, string) (browser.Tab, error)
	CloseTab(context.Context, string, string) error
}

type Service struct {
	models   map[string]Model
	order    []string
	caps     Capabilities
	browser  Browser
	lanes    map[string]chan struct{}
	poll     time.Duration
	mu       sync.Mutex
	sessions map[sessionKey]*conversation
	closed   bool
}

type sessionKey struct{ scope, model, id string }
type conversation struct {
	gate                             chan struct{}
	browserID, tabID, turnID         string
	started, accepted, closing       bool
	lastAccepted, expected, contexts []Message
	identity                         requestIdentity
	lastResult                       *Assistant
	partial                          string
	delivered                        bool
	cancel                           context.CancelFunc // guarded by Service.mu, including while waiting for a model lane
}
type requestIdentity struct {
	dialogue, contexts []Message
	tools              []Tool
	choice             string
	parallel           bool
}

func New(models []Model, caps Capabilities, b Browser) (*Service, error) {
	if len(models) > 0 && (caps == nil || b == nil) {
		return nil, errors.New("model sources require capabilities and a browser")
	}
	s := &Service{models: map[string]Model{}, caps: caps, browser: b, lanes: map[string]chan struct{}{}, poll: 400 * time.Millisecond, sessions: map[sessionKey]*conversation{}}
	for _, m := range models {
		u, err := url.Parse(m.URL)
		if m.ID == "" || strings.ContainsAny(m.ID, " /\\") || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || m.Start == "" || m.Read == "" || m.Stop == "" {
			return nil, errors.New("model requires an id, HTTPS target and start/read/stop capabilities")
		}
		if _, exists := s.models[m.ID]; exists {
			return nil, fmt.Errorf("duplicate model %q", m.ID)
		}
		if m.TimeoutSeconds == 0 {
			m.TimeoutSeconds = 300
		}
		if m.TimeoutSeconds < 10 || m.TimeoutSeconds > 1800 {
			return nil, errors.New("model timeout_seconds must be between 10 and 1800")
		}
		s.models[m.ID] = m
		s.order = append(s.order, m.ID)
		s.lanes[m.ID] = make(chan struct{}, 1)
	}
	return s, nil
}

func (s *Service) List() []map[string]any {
	if s == nil {
		return []map[string]any{}
	}
	result := make([]map[string]any, 0, len(s.order))
	for _, id := range s.order {
		m := s.models[id]
		result = append(result, map[string]any{"id": id, "object": "model", "owned_by": "local-runtime", "name": m.Title})
	}
	return result
}

type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
}

type Function struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function CallFunction `json:"function"`
}
type CallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type Request struct {
	Model               string          `json:"model"`
	ConversationID      string          `json:"conversation_id"`
	Retry               bool            `json:"retry,omitempty"`
	Messages            []Message       `json:"messages"`
	Tools               []Tool          `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	ResponseFormat      json.RawMessage `json:"response_format,omitempty"`
	StreamOptions       json.RawMessage `json:"stream_options,omitempty"`
	// Client metadata is not sent to the target page or used as a session credential.
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

type Assistant struct {
	ConversationID string     `json:"-"`
	Role           string     `json:"role"`
	Content        *string    `json:"content"`
	ToolCalls      []ToolCall `json:"tool_calls,omitempty"`
}
type Delta struct {
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}
type Error struct {
	Code, Message string
	Status        int
}

func (e *Error) Error() string                       { return e.Message }
func (e *Error) ErrorCode() string                   { return e.Code }
func failure(code, message string, status int) error { return &Error{code, message, status} }

// ValidateRequest performs protocol validation without opening a page or
// emitting a streaming response. Generate repeats it for non-HTTP consumers.
func (s *Service) ValidateRequest(request Request) error {
	_, _, _, err := s.validateRequest(request)
	return err
}

func (s *Service) validateRequest(request Request) (Model, map[string]*jsonschema.Resolved, string, error) {
	if s == nil {
		return Model{}, nil, "", failure("model_unavailable", "Local model sources are unavailable.", 503)
	}
	m, exists := s.models[request.Model]
	if !exists {
		return Model{}, nil, "", failure("model_not_found", "The requested local model is not registered.", 404)
	}
	if !conversationID.MatchString(request.ConversationID) {
		return Model{}, nil, "", failure("invalid_conversation_id", "conversation_id must be a stable identifier of 1 to 128 characters.", 400)
	}
	if len(request.Messages) == 0 || len(request.Messages) > 200 {
		return Model{}, nil, "", failure("invalid_request", "messages must contain between 1 and 200 entries.", 400)
	}
	if request.Temperature != nil || request.TopP != nil || request.MaxTokens != nil || request.MaxCompletionTokens != nil {
		return Model{}, nil, "", failure("unsupported_parameter", "This browser model does not expose sampling or token-limit controls.", 400)
	}
	if nonNull(request.ResponseFormat) || nonNull(request.StreamOptions) {
		return Model{}, nil, "", failure("unsupported_parameter", "This browser model does not implement response_format or stream_options.", 400)
	}
	for _, message := range request.Messages {
		if message.Role != "system" && message.Role != "developer" && message.Role != "user" && message.Role != "assistant" && message.Role != "tool" {
			return Model{}, nil, "", failure("invalid_request", "Unknown message role.", 400)
		}
		if !nonNull(message.Content) && !(message.Role == "assistant" && len(message.ToolCalls) > 0) {
			return Model{}, nil, "", failure("invalid_request", "Messages require text content unless an assistant is requesting tools.", 400)
		}
		if _, err := messageText(message.Content); err != nil {
			return Model{}, nil, "", err
		}
		if message.Role == "tool" && message.ToolCallID == "" {
			return Model{}, nil, "", failure("invalid_request", "A tool result requires tool_call_id.", 400)
		}
		if len(message.ToolCalls) > 0 && message.Role != "assistant" {
			return Model{}, nil, "", failure("invalid_request", "Only assistant messages may contain tool_calls.", 400)
		}
		for _, call := range message.ToolCalls {
			var args map[string]any
			if call.ID == "" || call.Type != "function" || !functionName.MatchString(call.Function.Name) || json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args == nil {
				return Model{}, nil, "", failure("invalid_request", "Assistant tool calls require an id, a function name and object JSON arguments.", 400)
			}
		}
	}
	validators, choice, err := toolValidators(request)
	return m, validators, choice, err
}

func nonNull(value json.RawMessage) bool {
	return len(bytes.TrimSpace(value)) > 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}

// OpenAI-compatible clients may represent text as a string or text-only parts.
// Both project to text; image/audio/file parts are explicitly unsupported.
func messageText(content json.RawMessage) (string, error) {
	if !nonNull(content) {
		return "", nil
	}
	if !json.Valid(content) {
		return "", failure("invalid_request", "Message content must be valid JSON.", 400)
	}
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&parts) == nil && parts != nil {
		var text strings.Builder
		for _, part := range parts {
			if part.Type != "text" {
				return "", failure("unsupported_content", "This browser model currently accepts text messages only.", 400)
			}
			text.WriteString(part.Text)
		}
		return text.String(), nil
	}
	return "", failure("unsupported_content", "This browser model currently accepts text messages only.", 400)
}

var conversationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func (s *Service) Generate(ctx context.Context, request Request, preferredBrowser, scope string, emit func(Delta) error) (Assistant, error) {
	m, validators, choice, err := s.validateRequest(request)
	if err != nil {
		return Assistant{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(m.TimeoutSeconds)*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Assistant{}, err
	}
	if scope == "" {
		return Assistant{}, failure("invalid_scope", "A caller scope is required.", 400)
	}
	key := sessionKey{scope, m.ID, request.ConversationID}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Assistant{}, failure("model_unavailable", "The model runtime is closing.", 503)
	}
	c := s.sessions[key]
	if c == nil {
		if len(s.sessions) >= 32 {
			s.mu.Unlock()
			return Assistant{}, failure("conversation_capacity", "Close an existing conversation before creating another.", 429)
		}
		c = &conversation{gate: make(chan struct{}, 1)}
		s.sessions[key] = c
	}
	closing := c.closing
	s.mu.Unlock()
	if closing {
		return Assistant{}, failure("conversation_closed", "The conversation is closing.", 409)
	}
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return Assistant{}, ctx.Err()
	}
	s.mu.Lock()
	if c.closing || s.closed {
		s.mu.Unlock()
		return Assistant{}, failure("conversation_closed", "The conversation is closing.", 409)
	}
	c.cancel = cancel
	s.mu.Unlock()
	defer func() { s.mu.Lock(); c.cancel = nil; s.mu.Unlock() }()
	if err := ctx.Err(); err != nil {
		return Assistant{}, err
	}
	dialog, contexts := normalizedHistory(request.Messages)
	identity := normalizedIdentity(request, dialog, contexts, choice)
	var delta []Message
	retrying := false
	if c.accepted {
		if reflect.DeepEqual(dialog, c.lastAccepted) {
			if !request.Retry {
				return Assistant{}, failure("conversation_retry_required", "This request was already accepted; use retry:true to explicitly retry in the same conversation.", 409)
			}
			if c.lastResult != nil {
				if !reflect.DeepEqual(identity, c.identity) {
					return Assistant{}, failure("conversation_retry_mismatch", "Retry must preserve the accepted request, including context and tools.", 409)
				}
				answer := cloneAssistant(*c.lastResult)
				if emit != nil {
					if err := emit(Delta{Content: deref(answer.Content), ToolCalls: answer.ToolCalls}); err != nil {
						return Assistant{}, sourceFailure(err, "stream_write_failed", "The response consumer disconnected.")
					}
				}
				return answer, nil
			}
			retrying = true
		} else {
			if historyPrefix(c.expected, dialog) {
				delta = dialog[len(c.expected):]
			} else if !c.delivered && historyPrefix(c.lastAccepted, dialog) {
				delta = dialog[len(c.lastAccepted):]
			} else {
				return Assistant{}, failure("conversation_fork", "The supplied history branches from this conversation; choose a new conversation_id.", 409)
			}
			if !c.delivered && len(delta) > 0 && delta[0].Role == "assistant" {
				text, _ := messageText(delta[0].Content)
				if len(delta[0].ToolCalls) > 0 || !strings.HasPrefix(c.partial, text) {
					return Assistant{}, failure("conversation_fork", "The partial assistant does not match this conversation's interrupted output.", 409)
				}
				delta = delta[1:] // The website already owns this partial assistant.
			}
			if len(delta) == 0 {
				if c.lastResult != nil {
					return Assistant{}, failure("conversation_retry_mismatch", "Recover a completed response using its original accepted request; choose a new conversation_id to regenerate.", 409)
				}
				if !request.Retry {
					return Assistant{}, failure("conversation_retry_required", "No new conversation input was supplied; use retry:true for an explicit retry.", 409)
				}
				retrying = true
			}
		}
	}
	select {
	case s.lanes[m.ID] <- struct{}{}:
		defer func() { <-s.lanes[m.ID] }()
	case <-ctx.Done():
		return Assistant{}, ctx.Err()
	}
	browserID := m.BrowserID
	if browserID == "" {
		browserID = preferredBrowser
	}
	if c.browserID != "" {
		if browserID != "" && browserID != c.browserID {
			return Assistant{}, failure("conversation_browser_conflict", "This conversation belongs to another browser profile.", 409)
		}
		browserID = c.browserID
	}
	instances := s.browser.Status().Instances
	if browserID == "" {
		for _, instance := range instances {
			if instance.Connected {
				if browserID != "" {
					return Assistant{}, failure("browser_selection_required", "Select a browser_id for this model; several profiles are connected.", 409)
				}
				browserID = instance.BrowserID
			}
		}
	}
	if browserID == "" {
		return Assistant{}, failure("browser_unavailable", "Connect the configured browser profile before calling this model.", 503)
	}
	online := false
	for _, instance := range instances {
		if instance.BrowserID == browserID && instance.Connected {
			online = true
			break
		}
	}
	if !online {
		return Assistant{}, failure("browser_unavailable", "The selected model browser profile is unavailable.", 503)
	}
	if c.tabID == "" {
		tab, err := s.browser.Open(ctx, browserID, m.URL, false, "")
		if err != nil {
			return Assistant{}, sourceFailure(err, "browser_open_failed", "The model session could not be opened.")
		}
		c.tabID, c.browserID = tab.ID, browserID
	}
	complete, startAttempted := false, false
	turnID := "turn_" + randomID()
	defer func() {
		if startAttempted && !complete {
			cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = s.caps.Call(cleanup, m.Stop, browserID, c.tabID, map[string]any{"turn_id": turnID})
			done()
		}
	}()
	for !c.started {
		value, err := s.caps.Call(ctx, m.Read, browserID, c.tabID, map[string]any{})
		if err != nil {
			return Assistant{}, sourceFailure(err, "browser_read_failed", "The model page could not be observed.")
		}
		state, _, err := sourceState(value)
		if err != nil {
			return Assistant{}, err
		}
		if state == "idle" {
			break
		}
		if state != "not_ready" {
			return Assistant{}, failure("model_session_not_empty", "The model session was not confirmed empty before sending.", 409)
		}
		if err := s.wait(ctx, emit); err != nil {
			return Assistant{}, err
		}
	}
	promptRequest := request
	mode := "new"
	if c.started {
		mode = "continue"
		promptRequest.Messages = append([]Message(nil), delta...)
		if !reflect.DeepEqual(contexts, c.contexts) {
			updates := contexts
			if len(updates) == 0 {
				updates = []Message{{Role: "system", Content: json.RawMessage(`"Previous system/developer context is cleared."`)}}
			}
			promptRequest.Messages = append(append([]Message(nil), updates...), promptRequest.Messages...)
		}
		if retrying {
			promptRequest.Messages = append(promptRequest.Messages, Message{Role: "user", Content: json.RawMessage(`"Explicitly retry the preceding request in this same conversation. Do not invent tool results."`)})
		}
	}
	prompt := browserPrompt(promptRequest, choice)
	c.lastAccepted, c.expected, c.contexts = dialog, dialog, contexts
	c.identity = identity
	c.lastResult = nil
	c.partial = ""
	c.delivered = false
	c.accepted = true
	c.started = true
	c.turnID = turnID
	startAttempted = true // Even a lost acknowledgement may follow a real send.
	start, err := s.caps.Call(ctx, m.Start, browserID, c.tabID, map[string]any{"prompt": prompt, "mode": mode, "turn_id": turnID})
	if err != nil {
		return Assistant{}, sourceFailure(err, "browser_start_failed", "The page did not confirm the send; the request was not replayed.")
	}
	if err := validateTurn(start, turnID); err != nil {
		return Assistant{}, err
	}
	state, text, err := sourceState(start)
	if err != nil {
		return Assistant{}, err
	}
	emitted, previous, previousState := "", "", ""
	for {
		if state == "not_ready" || state == "idle" {
			return Assistant{}, failure("model_generation_lost", "The model page lost the active generation state; the request was not replayed.", 502)
		}
		if state == "complete" && previousState == "complete" && previous == text {
			complete = true
			if len(validators) > 0 {
				answer, parseErr := parseToolAnswer(text, validators, choice, request.ParallelToolCalls)
				if parseErr != nil {
					return Assistant{}, parseErr
				}
				answer.ConversationID = request.ConversationID
				cacheAnswer(c, dialog, answer)
				if emit != nil {
					if err := emit(Delta{Content: deref(answer.Content), ToolCalls: answer.ToolCalls}); err != nil {
						return Assistant{}, sourceFailure(err, "stream_write_failed", "The model response consumer disconnected.")
					}
				}
				c.delivered = true
				return answer, nil
			}
			if text == "" {
				return Assistant{}, failure("empty_model_response", "The page completed without an answer.", 502)
			}
			if !strings.HasPrefix(text, emitted) {
				return Assistant{}, failure("source_revised_output", "The page revised output that was already streamed.", 502)
			}
			answer := Assistant{Role: "assistant", Content: &text, ConversationID: request.ConversationID}
			cacheAnswer(c, dialog, answer)
			if emit != nil {
				c.partial = text
				if err := emit(Delta{Content: strings.TrimPrefix(text, emitted)}); err != nil {
					return Assistant{}, sourceFailure(err, "stream_write_failed", "The model response consumer disconnected.")
				}
			}
			c.delivered = true
			return answer, nil
		}
		if len(validators) == 0 && emit != nil {
			stable := commonPrefix(previous, text)
			if !strings.HasPrefix(stable, emitted) {
				return Assistant{}, failure("source_revised_output", "The page revised output that was already streamed.", 502)
			}
			c.partial = stable
			if err := emit(Delta{Content: strings.TrimPrefix(stable, emitted)}); err != nil {
				return Assistant{}, sourceFailure(err, "stream_write_failed", "The model response consumer disconnected.")
			}
			emitted = stable
		} else if emit != nil {
			if err := emit(Delta{}); err != nil {
				return Assistant{}, sourceFailure(err, "stream_write_failed", "The model response consumer disconnected.")
			}
		}
		previous, previousState = text, state
		if err := s.wait(ctx, nil); err != nil {
			return Assistant{}, err
		}
		value, err := s.caps.Call(ctx, m.Read, browserID, c.tabID, map[string]any{"turn_id": turnID})
		if err != nil {
			return Assistant{}, sourceFailure(err, "browser_read_failed", "The model generation could not be observed; the request was not replayed.")
		}
		if err := validateTurn(value, turnID); err != nil {
			return Assistant{}, err
		}
		state, text, err = sourceState(value)
		if err != nil {
			return Assistant{}, err
		}
	}
}

func (s *Service) wait(ctx context.Context, emit func(Delta) error) error {
	if emit != nil {
		if err := emit(Delta{}); err != nil {
			return sourceFailure(err, "stream_write_failed", "The model response consumer disconnected.")
		}
	}
	timer := time.NewTimer(s.poll)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func validateTurn(value map[string]any, turnID string) error {
	if value["turn_id"] != turnID {
		return failure("model_turn_mismatch", "The model page did not confirm this generation turn.", 502)
	}
	return nil
}

func normalizedHistory(messages []Message) (dialogue, contexts []Message) {
	for _, message := range messages {
		text, _ := messageText(message.Content)
		m := Message{Role: message.Role, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			var args any
			_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
			encoded, _ := json.Marshal(args)
			call.Function.Arguments = string(encoded)
			m.ToolCalls = append(m.ToolCalls, call)
		}
		if text == "" && len(m.ToolCalls) > 0 {
			m.Content = json.RawMessage("null")
		} else {
			m.Content, _ = json.Marshal(text)
		}
		if m.Role == "system" || m.Role == "developer" {
			contexts = append(contexts, m)
		} else {
			dialogue = append(dialogue, m)
		}
	}
	return
}

func normalizedIdentity(request Request, dialogue, contexts []Message, choice string) requestIdentity {
	identity := requestIdentity{dialogue: dialogue, contexts: contexts, choice: choice, parallel: request.ParallelToolCalls == nil || *request.ParallelToolCalls}
	for _, tool := range request.Tools {
		var schema any
		_ = json.Unmarshal(tool.Function.Parameters, &schema)
		tool.Function.Parameters, _ = json.Marshal(schema)
		if tool.Function.Strict != nil {
			value := *tool.Function.Strict
			tool.Function.Strict = &value
		}
		identity.tools = append(identity.tools, tool)
	}
	return identity
}

func cloneAssistant(answer Assistant) Assistant {
	if answer.Content != nil {
		text := *answer.Content
		answer.Content = &text
	}
	answer.ToolCalls = append([]ToolCall(nil), answer.ToolCalls...)
	return answer
}

func cacheAnswer(c *conversation, dialogue []Message, answer Assistant) {
	value := cloneAssistant(answer)
	c.lastResult = &value
	c.expected = append(append([]Message(nil), dialogue...), normalizedAssistant(answer))
}

func normalizedAssistant(answer Assistant) Message {
	m := Message{Role: "assistant", ToolCalls: answer.ToolCalls}
	if answer.Content == nil {
		m.Content = json.RawMessage("null")
	} else {
		m.Content, _ = json.Marshal(*answer.Content)
	}
	dialogue, _ := normalizedHistory([]Message{m})
	return dialogue[0]
}

func historyPrefix(prefix, whole []Message) bool {
	if len(prefix) > len(whole) {
		return false
	}
	for i := range prefix {
		if !reflect.DeepEqual(prefix[i], whole[i]) {
			return false
		}
	}
	return true
}

// CloseConversation closes only the page owned by this exact caller/model/id.
// Cancellation stops an active turn before the page is closed; website history
// is never deleted. Ordinary request cancellation does not call this method.
func (s *Service) CloseConversation(ctx context.Context, scope, model, id string) error {
	if s == nil || scope == "" || model == "" || !conversationID.MatchString(id) {
		return failure("invalid_conversation_id", "A caller scope, model and valid conversation_id are required.", 400)
	}
	key := sessionKey{scope, model, id}
	s.mu.Lock()
	c := s.sessions[key]
	if c == nil {
		s.mu.Unlock()
		return failure("conversation_not_found", "This caller does not own that conversation.", 404)
	}
	c.closing = true
	cancel := c.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if c.tabID != "" {
		if err := s.browser.CloseTab(ctx, c.tabID, ""); err != nil {
			return sourceFailure(err, "browser_close_failed", "The owned model page could not be closed.")
		}
	}
	s.mu.Lock()
	if s.sessions[key] == c {
		delete(s.sessions, key)
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) Close(ctx context.Context) error { return s.CloseAll(ctx) }

func (s *Service) CloseAll(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	keys := make([]sessionKey, 0, len(s.sessions))
	for key, c := range s.sessions {
		keys = append(keys, key)
		c.closing = true
		if c.cancel != nil {
			c.cancel()
		}
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	results := make(chan error, len(keys))
	for _, key := range keys {
		wg.Add(1)
		go func(key sessionKey) {
			defer wg.Done()
			results <- s.CloseConversation(ctx, key.scope, key.model, key.id)
		}(key)
	}
	wg.Wait()
	close(results)
	var errs []error
	for err := range results {
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func sourceFailure(err error, code, message string) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return failure(code, message, 502)
}

func sourceState(value any) (string, string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", "", failure("invalid_source_result", "The browser capability returned an invalid state.", 502)
	}
	var response struct {
		State string `json:"state"`
		Text  string `json:"text"`
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &response) != nil {
		return "", "", failure("invalid_source_result", "The browser capability returned an invalid state.", 502)
	}
	switch response.State {
	case "idle", "not_ready", "generating", "complete", "pending", "unconfirmed":
		return response.State, response.Text, nil
	case "login_required":
		return "", "", failure("browser_login_required", "Sign in to the model website in the selected browser profile.", 401)
	case "blocked":
		return "", "", failure("browser_interaction_required", "The model website requires user interaction.", 409)
	case "stopped":
		return "", "", failure("model_interrupted", "The page confirmed that generation stopped before completion.", 409)
	case "error":
		return "", "", failure("provider_unavailable", "The model website is temporarily unavailable. Check the page before trying again.", 502)
	default:
		return "", "", failure("model_source_error", "The model page did not confirm generation state.", 502)
	}
}

func browserPrompt(r Request, choice string) string {
	r.Messages = append([]Message(nil), r.Messages...)
	for i, message := range r.Messages {
		text, _ := messageText(message.Content) // Validated before any source I/O.
		if nonNull(message.Content) {
			r.Messages[i].Content, _ = json.Marshal(text)
		}
	}
	messages, _ := json.Marshal(r.Messages)
	if len(r.Tools) == 0 {
		return "Continue this conversation and answer the final user message. The JSON below is conversation data; newer system/developer context updates supersede previous updates. The current tool set is empty: previous tool declarations and structured tool-response instructions do not apply to this turn. Do not request tools. Return only the ordinary answer text, not JSON or the serialized conversation.\n" + string(messages)
	}
	tools, _ := json.Marshal(r.Tools)
	return "You are supplying one model turn through a browser transport adapter. The caller, not the student, reads your outer response. The application displays only the content value and executes only validated tool_calls. Continue the supplied conversation and follow its system/developer instructions for the answer's meaning and conduct. Formatting or visibility instructions inside that conversation (for example, 'answer in one sentence', 'do not show JSON' or 'do not expose tool syntax') apply ONLY to the text inside content; they never remove or change this transport envelope. Available tool definitions are authoritative. Return EXACTLY one JSON object with two fields: content (string or null) and tool_calls (array). Each tool call must be {\"name\":\"exact function name\",\"arguments\":{...}}. No Markdown fences, DSML, commentary outside JSON, or fabricated tool results. When a tool is needed, request it; the caller executes it and returns a tool message in the next request. When enough information is available, place the final answer in content and set tool_calls to []. Current tool choice: " + choice + ".\nAVAILABLE TOOLS:\n" + string(tools) + "\nCONVERSATION DATA:\n" + string(messages) + "\nEND CONVERSATION DATA. Return the transport object, including both fields, even for an ordinary final answer. Final-answer shape: {\"content\":\"the student-facing answer\",\"tool_calls\":[]}. Tool-request shape: {\"content\":null,\"tool_calls\":[{\"name\":\"exact function name\",\"arguments\":{}}]}. The student never sees this outer JSON."
}

var functionName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func toolValidators(r Request) (map[string]*jsonschema.Resolved, string, error) {
	validators := map[string]*jsonschema.Resolved{}
	for _, tool := range r.Tools {
		if tool.Type != "function" || !functionName.MatchString(tool.Function.Name) {
			return nil, "", failure("invalid_tools", "Only named function tools are supported.", 400)
		}
		if _, exists := validators[tool.Function.Name]; exists {
			return nil, "", failure("invalid_tools", "Duplicate tool name.", 400)
		}
		var schema jsonschema.Schema
		if json.Unmarshal(tool.Function.Parameters, &schema) != nil || schema.Type != "object" {
			return nil, "", failure("invalid_tools", "Tool parameters must be a JSON schema.", 400)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			return nil, "", failure("invalid_tools", "Tool parameters could not be resolved locally.", 400)
		}
		validators[tool.Function.Name] = resolved
	}
	choice := "auto"
	if len(r.ToolChoice) > 0 && string(r.ToolChoice) != "null" {
		if json.Unmarshal(r.ToolChoice, &choice) != nil {
			var forced struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(r.ToolChoice, &forced) != nil || forced.Type != "function" || validators[forced.Function.Name] == nil {
				return nil, "", failure("invalid_tool_choice", "Unknown forced tool.", 400)
			}
			choice = forced.Function.Name
		}
	}
	if choice != "auto" && choice != "none" && choice != "required" && validators[choice] == nil {
		return nil, "", failure("invalid_tool_choice", "Unknown tool choice.", 400)
	}
	if len(validators) == 0 && choice != "auto" && choice != "none" {
		return nil, "", failure("invalid_tool_choice", "tool_choice requires tools.", 400)
	}
	return validators, choice, nil
}

func parseToolAnswer(text string, validators map[string]*jsonschema.Resolved, choice string, parallel *bool) (Assistant, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &fields) != nil || len(fields) != 2 || len(fields["content"]) == 0 || !nonNull(fields["tool_calls"]) {
		return Assistant{}, failure("invalid_tool_response", "The page did not return both content and a tool_calls array.", 502)
	}
	var envelope struct {
		Content *string `json:"content"`
		Calls   []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"tool_calls"`
	}
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(text)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return Assistant{}, failure("invalid_tool_response", "The page did not return the required structured response.", 502)
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return Assistant{}, failure("invalid_tool_response", "The page returned extra content outside the structured response.", 502)
	}
	if choice == "none" && len(envelope.Calls) > 0 || choice == "required" && len(envelope.Calls) == 0 || parallel != nil && !*parallel && len(envelope.Calls) > 1 {
		return Assistant{}, failure("invalid_tool_response", "The response violated the requested tool choice.", 502)
	}
	answer := Assistant{Role: "assistant", Content: envelope.Content}
	for _, call := range envelope.Calls {
		validator := validators[call.Name]
		if validator == nil || choice != "auto" && choice != "none" && choice != "required" && choice != call.Name {
			return Assistant{}, failure("invalid_tool_response", "The page requested an unavailable function.", 502)
		}
		var args map[string]any
		if json.Unmarshal(call.Arguments, &args) != nil || args == nil || validator.Validate(args) != nil {
			return Assistant{}, failure("invalid_tool_arguments", "The page returned function arguments that do not match the supplied schema.", 502)
		}
		answer.ToolCalls = append(answer.ToolCalls, ToolCall{ID: "call_" + randomID(), Type: "function", Function: CallFunction{Name: call.Name, Arguments: string(call.Arguments)}})
	}
	if choice != "auto" && choice != "none" && choice != "required" && len(answer.ToolCalls) == 0 {
		return Assistant{}, failure("invalid_tool_response", "The response omitted the requested function.", 502)
	}
	if len(answer.ToolCalls) == 0 && (answer.Content == nil || *answer.Content == "") {
		return Assistant{}, failure("empty_model_response", "The page returned neither content nor tool calls.", 502)
	}
	return answer, nil
}

func commonPrefix(a, b string) string {
	ar, br := []rune(a), []rune(b)
	n := 0
	for n < len(ar) && n < len(br) && ar[n] == br[n] {
		n++
	}
	return string(ar[:n])
}
func randomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// DecodeRequest deliberately rejects unknown provider parameters instead of
// silently claiming that a browser page implements the complete model API.
func DecodeRequest(data []byte) (Request, error) {
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, failure("invalid_request", "Unsupported or malformed chat-completions request.", 400)
	}
	var more any
	if decoder.Decode(&more) != io.EOF {
		return request, failure("invalid_request", "Expected one request object.", 400)
	}
	return request, nil
}
