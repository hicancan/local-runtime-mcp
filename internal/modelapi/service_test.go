package modelapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
)

type sourceCall struct {
	name, profile, tab string
	args               map[string]any
}
type fakeSource struct {
	mu      sync.Mutex
	calls   []sourceCall
	started map[string]bool
	reads   map[string]int
	hook    func(context.Context, sourceCall) (map[string]any, error)
}

func (f *fakeSource) Call(ctx context.Context, name, profile, tab string, args map[string]any) (map[string]any, error) {
	call := sourceCall{name, profile, tab, args}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, call)
	}
	return f.defaultCall(ctx, call)
}
func (f *fakeSource) defaultCall(ctx context.Context, call sourceCall) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch call.name {
	case "source.start":
		f.started[call.tab] = true
		return state("generating", ""), nil
	case "source.read":
		if !f.started[call.tab] {
			return state("idle", ""), nil
		}
		f.reads[call.tab]++
		return state("complete", "answer"), nil
	case "source.stop":
		return state("stopped", ""), nil
	default:
		return nil, errors.New("unknown fake capability")
	}
}
func (f *fakeSource) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, call := range f.calls {
		if call.name == name {
			n++
		}
	}
	return n
}

type fakeBrowser struct {
	mu             sync.Mutex
	instances      []browser.InstanceStatus
	opened, closed []browser.Tab
	active, peak   int
	openErr        error
	closeHook      func(context.Context, string) error
}

func (b *fakeBrowser) Status() browser.Status {
	return browser.Status{Instances: append([]browser.InstanceStatus(nil), b.instances...)}
}
func (b *fakeBrowser) Open(ctx context.Context, profile, url string, active bool, control string) (browser.Tab, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openErr != nil {
		return browser.Tab{}, b.openErr
	}
	if ctx.Err() != nil {
		return browser.Tab{}, ctx.Err()
	}
	tab := browser.Tab{ID: "owned-" + randomID(), BrowserID: profile, URL: url, Active: active}
	b.opened = append(b.opened, tab)
	b.active++
	if b.active > b.peak {
		b.peak = b.active
	}
	return tab, nil
}
func (b *fakeBrowser) CloseTab(ctx context.Context, id, control string) error {
	if b.closeHook != nil {
		if err := b.closeHook(ctx, id); err != nil {
			return err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, tab := range b.opened {
		if tab.ID == id {
			b.closed = append(b.closed, tab)
			b.active--
			return ctx.Err()
		}
	}
	return errors.New("not an owned test tab")
}
func (b *fakeBrowser) counts() (int, int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.opened), len(b.closed), b.peak
}
func state(phase, text string) map[string]any { return map[string]any{"state": phase, "text": text} }
func newFixture(t *testing.T) (*Service, *fakeSource, *fakeBrowser) {
	t.Helper()
	f := &fakeSource{started: map[string]bool{}, reads: map[string]int{}}
	b := &fakeBrowser{instances: []browser.InstanceStatus{{BrowserID: "profile-a", Connected: true}}}
	s, err := New([]Model{{ID: "web-test", URL: "https://model.example.test/", Start: "source.start", Read: "source.read", Stop: "source.stop"}}, f, b)
	if err != nil {
		t.Fatal(err)
	}
	s.poll = time.Millisecond
	return s, f, b
}
func request() Request {
	return Request{Model: "web-test", Messages: []Message{{Role: "user", Content: json.RawMessage(`"question"`)}}}
}
func errorCode(t *testing.T, err error, code string) {
	t.Helper()
	var target *Error
	if !errors.As(err, &target) || target.Code != code {
		t.Fatalf("wanted safe error %s, got %v", code, err)
	}
}

func TestValidationRejectsUnsupportedRequestsBeforeBrowserActivity(t *testing.T) {
	s, f, b := newFixture(t)
	for _, tc := range []struct {
		name string
		edit func(*Request)
		code string
	}{
		{"unknown-model", func(r *Request) { r.Model = "missing" }, "model_not_found"},
		{"no-messages", func(r *Request) { r.Messages = nil }, "invalid_request"},
		{"unknown-role", func(r *Request) { r.Messages[0].Role = "other" }, "invalid_request"},
		{"missing-content", func(r *Request) { r.Messages[0].Content = nil }, "invalid_request"},
		{"image", func(r *Request) {
			r.Messages[0].Content = json.RawMessage(`[{"type":"image_url","image_url":{"url":"private"}}]`)
		}, "unsupported_content"},
		{"format", func(r *Request) { r.ResponseFormat = json.RawMessage(`{"type":"json_object"}`) }, "unsupported_parameter"},
		{"usage", func(r *Request) { r.StreamOptions = json.RawMessage(`{"include_usage":true}`) }, "unsupported_parameter"},
		{"sampling", func(r *Request) { value := 0.0; r.Temperature = &value }, "unsupported_parameter"},
		{"tokens", func(r *Request) { value := 100; r.MaxTokens = &value }, "unsupported_parameter"},
		{"orphan-tool-result", func(r *Request) { r.Messages[0].Role = "tool" }, "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request()
			tc.edit(&r)
			errorCode(t, s.ValidateRequest(r), tc.code)
			_, err := s.Generate(context.Background(), r, "", nil)
			errorCode(t, err, tc.code)
		})
	}
	opened, _, _ := b.counts()
	if opened != 0 || f.count("source.read") != 0 {
		t.Fatal("request validation reached browser activity")
	}
	if _, err := New([]Model{{ID: "m"}}, nil, b); err == nil {
		t.Fatal("a configured source accepted nil dependencies")
	}
}

func TestReadinessWaitsOnlyBeforeOneSendAndTextPartsRemainText(t *testing.T) {
	s, f, b := newFixture(t)
	readiness := 0
	f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.read" && f.count("source.start") == 0 {
			readiness++
			if readiness < 3 {
				return state("not_ready", ""), nil
			}
		}
		if call.name == "source.start" {
			prompt := call.args["prompt"].(string)
			if !strings.Contains(prompt, `"content":"hello world"`) || strings.Contains(prompt, "client-private-metadata") {
				t.Fatal("text parts or request-only metadata were projected incorrectly")
			}
		}
		return f.defaultCall(ctx, call)
	}
	r := request()
	r.Messages[0].Content = json.RawMessage(`[{"type":"text","text":"hello "},{"type":"text","text":"world"}]`)
	r.Metadata = json.RawMessage(`{"private":"client-private-metadata"}`)
	answer, err := s.Generate(context.Background(), r, "", nil)
	if err != nil || deref(answer.Content) != "answer" || readiness != 3 || f.count("source.start") != 1 || f.count("source.stop") != 0 {
		t.Fatal("readiness/send lifecycle failed:", err)
	}
	opened, closed, _ := b.counts()
	if opened != 1 || closed != 1 {
		t.Fatal("owned model tab was not closed")
	}
}

func TestEmptyCompleteLostStateAndStoppedDoNotResend(t *testing.T) {
	for _, tc := range []struct{ phase, code string }{{"complete", "empty_model_response"}, {"not_ready", "model_generation_lost"}, {"idle", "model_generation_lost"}, {"stopped", "model_interrupted"}} {
		t.Run(tc.phase, func(t *testing.T) {
			s, f, b := newFixture(t)
			f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
				if call.name == "source.read" && f.count("source.start") > 0 {
					return state(tc.phase, ""), nil
				}
				return f.defaultCall(ctx, call)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := s.Generate(ctx, request(), "", nil)
			errorCode(t, err, tc.code)
			_, closed, _ := b.counts()
			if f.count("source.start") != 1 || closed != 1 {
				t.Fatal("an interrupted/empty response replayed start or leaked its tab")
			}
		})
	}
}

func TestReadinessCannotSendIntoOccupiedOrUnreadySessions(t *testing.T) {
	for _, phase := range []string{"complete", "generating", "pending", "not_ready"} {
		t.Run(phase, func(t *testing.T) {
			s, f, b := newFixture(t)
			f.hook = func(context.Context, sourceCall) (map[string]any, error) { return state(phase, ""), nil }
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			_, err := s.Generate(ctx, request(), "", nil)
			if phase == "not_ready" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("unready admission ignored cancellation")
				}
			} else {
				errorCode(t, err, "model_session_not_empty")
			}
			_, closed, _ := b.counts()
			if f.count("source.start") != 0 || f.count("source.stop") != 0 || closed != 1 {
				t.Fatal("readiness changed a generation it did not start or leaked a tab")
			}
		})
	}
}

func TestRawSourceErrorsAreSanitizedAndUnknownSendIsNotReplayed(t *testing.T) {
	for _, operation := range []string{"source.read", "source.start"} {
		t.Run(operation, func(t *testing.T) {
			s, f, _ := newFixture(t)
			f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
				if call.name == operation {
					return nil, errors.New("sensitive-source-detail")
				}
				return f.defaultCall(ctx, call)
			}
			_, err := s.Generate(context.Background(), request(), "", nil)
			if err == nil || strings.Contains(err.Error(), "sensitive-source-detail") || f.count("source.start") > 1 {
				t.Fatal("raw error leaked or a send was replayed")
			}
		})
	}
}

func TestProviderErrorStateIsExplicitAndNeverLeaksThePageError(t *testing.T) {
	s, f, b := newFixture(t)
	f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.read" && f.count("source.start") > 0 {
			return map[string]any{"state": "error", "text": "", "error": "sensitive-page-error-detail"}, nil
		}
		return f.defaultCall(ctx, call)
	}
	_, err := s.Generate(context.Background(), request(), "", nil)
	errorCode(t, err, "provider_unavailable")
	if strings.Contains(err.Error(), "sensitive-page-error-detail") || f.count("source.start") != 1 || f.count("source.stop") != 1 {
		t.Fatal("provider rejection leaked or repeated its send")
	}
	_, closed, _ := b.counts()
	if closed != 1 {
		t.Fatal("provider rejection leaked the owned tab")
	}
}

func TestCancellationStopsAndClosesWithIndependentLiveContexts(t *testing.T) {
	s, f, b := newFixture(t)
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	f.hook = func(callContext context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.start" {
			close(started)
			<-callContext.Done()
			return nil, callContext.Err()
		}
		if call.name == "source.stop" {
			if callContext.Err() != nil || ctx.Err() == nil {
				t.Error("stop reused the canceled request context")
			}
			return state("stopped", ""), nil
		}
		return f.defaultCall(callContext, call)
	}
	b.closeHook = func(closeContext context.Context, _ string) error {
		if closeContext.Err() != nil {
			t.Error("close reused the canceled request context")
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { _, err := s.Generate(ctx, request(), "", nil); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || f.count("source.stop") != 1 {
		t.Fatal("cancel did not propagate and stop once:", err)
	}
	_, closed, _ := b.counts()
	if closed != 1 {
		t.Fatal("cancel left an owned page open")
	}
}

func TestStopDeadlineCannotConsumeCloseDeadline(t *testing.T) {
	s, f, b := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.hook = func(callContext context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.start" {
			cancel()
			return nil, context.Canceled
		}
		if call.name == "source.stop" {
			<-callContext.Done()
			return nil, callContext.Err()
		}
		return f.defaultCall(callContext, call)
	}
	b.closeHook = func(closeContext context.Context, _ string) error {
		if closeContext.Err() != nil {
			t.Error("exhausted stop deadline prevented closing")
		}
		return nil
	}
	_, err := s.Generate(ctx, request(), "", nil)
	_, closed, _ := b.counts()
	if !errors.Is(err, context.Canceled) || closed != 1 {
		t.Fatal("stop timeout prevented close")
	}
}

func TestOneModelSerializesSessionsAndWaitingCancellationDoesNotOpen(t *testing.T) {
	s, f, b := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	once := sync.Once{}
	f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.start" {
			once.Do(func() { close(entered); <-release })
		}
		return f.defaultCall(ctx, call)
	}
	first := make(chan error, 1)
	go func() { _, err := s.Generate(context.Background(), request(), "", nil); first <- err }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.Generate(ctx, request(), "", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("waiting request ignored its deadline")
	}
	opened, _, peak := b.counts()
	if opened != 1 || peak != 1 || f.count("source.start") != 1 {
		t.Fatal("a queued request opened or sent concurrently")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Generate(context.Background(), request(), "", nil); err != nil {
		t.Fatal("lane was not released:", err)
	}
	_, closed, peak := b.counts()
	if closed != 2 || peak != 1 {
		t.Fatal("model sessions overlapped or failed to clean up")
	}
}

func TestBrowserSelectionRequiresExplicitChoiceForMultipleProfiles(t *testing.T) {
	s, _, b := newFixture(t)
	b.instances = append(b.instances, browser.InstanceStatus{BrowserID: "profile-b", Connected: true})
	_, err := s.Generate(context.Background(), request(), "", nil)
	errorCode(t, err, "browser_selection_required")
	if opened, _, _ := b.counts(); opened != 0 {
		t.Fatal("ambiguous selection opened a tab")
	}
	_, err = s.Generate(context.Background(), request(), "unknown-profile", nil)
	errorCode(t, err, "browser_unavailable")
	if _, err := s.Generate(context.Background(), request(), "profile-b", nil); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	selected := b.opened[0].BrowserID
	b.mu.Unlock()
	if selected != "profile-b" {
		t.Fatal("preferred browser was ignored")
	}
	m := s.models["web-test"]
	m.BrowserID = "profile-a"
	s.models[m.ID] = m
	if _, err := s.Generate(context.Background(), request(), "profile-b", nil); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	selected = b.opened[1].BrowserID
	b.mu.Unlock()
	if selected != "profile-a" {
		t.Fatal("configured browser did not bind the model")
	}
}

func functionTool(name string) Tool {
	return Tool{Type: "function", Function: Function{Name: name, Parameters: json.RawMessage(`{"type":"object","properties":{"room":{"type":"string"}},"required":["room"],"additionalProperties":false}`)}}
}
func TestToolSchemaChoiceAndArgumentsAreEnforcedWithoutExecutingTools(t *testing.T) {
	r := request()
	r.Tools = []Tool{functionTool("lookup")}
	validators, choice, err := toolValidators(r)
	if err != nil || choice != "auto" {
		t.Fatal(err)
	}
	valid := `{"content":null,"tool_calls":[{"name":"lookup","arguments":{"room":"A"}}]}`
	answer, err := parseToolAnswer(valid, validators, "required", nil)
	if err != nil || len(answer.ToolCalls) != 1 || answer.ToolCalls[0].ID == "" || answer.ToolCalls[0].Function.Arguments != `{"room":"A"}` {
		t.Fatal("structured tool response was not projected:", err)
	}
	r.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":"lookup"}}`)
	forced, selected, err := toolValidators(r)
	if err != nil || selected != "lookup" {
		t.Fatal("named tool selection was not preserved")
	}
	if _, err := parseToolAnswer(valid, forced, selected, nil); err != nil {
		t.Fatal("valid forced function was rejected:", err)
	}
	for _, tc := range []struct{ text, choice, code string }{
		{valid, "none", "invalid_tool_response"},
		{`{"content":"done","tool_calls":[]}`, "required", "invalid_tool_response"},
		{`{"content":null,"tool_calls":[{"name":"unavailable","arguments":{}}]}`, "auto", "invalid_tool_response"},
		{`{"content":null,"tool_calls":[{"name":"lookup","arguments":{"room":1}}]}`, "auto", "invalid_tool_arguments"},
		{`{"content":null,"tool_calls":[{"name":"lookup","arguments":"not-object"}]}`, "auto", "invalid_tool_arguments"},
		{valid + ` trailing`, "auto", "invalid_tool_response"},
		{"<DSML>invoke lookup</DSML>", "auto", "invalid_tool_response"},
		{`{"content":"done"}`, "auto", "invalid_tool_response"},
		{`{"content":"done","tool_calls":null}`, "auto", "invalid_tool_response"},
	} {
		_, err := parseToolAnswer(tc.text, validators, tc.choice, nil)
		errorCode(t, err, tc.code)
	}
	parallel := false
	_, err = parseToolAnswer(`{"content":null,"tool_calls":[{"name":"lookup","arguments":{"room":"A"}},{"name":"lookup","arguments":{"room":"B"}}]}`, validators, "auto", &parallel)
	errorCode(t, err, "invalid_tool_response")
	for _, mutate := range []func(*Request){
		func(r *Request) { r.Tools = append(r.Tools, r.Tools[0]) },
		func(r *Request) { r.Tools[0].Function.Parameters = json.RawMessage(`{"type":"string"}`) },
		func(r *Request) {
			r.Tools[0].Function.Parameters = json.RawMessage(`{"type":"object","$ref":"https://not-fetched.invalid/schema"}`)
		},
		func(r *Request) { r.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":"missing"}}`) },
	} {
		bad := request()
		bad.Tools = []Tool{functionTool("lookup")}
		mutate(&bad)
		if _, _, err := toolValidators(bad); err == nil {
			t.Fatal("invalid tool configuration was accepted")
		}
	}
}
