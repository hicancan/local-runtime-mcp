package modelapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
)

func appendAnswer(r Request, answer Assistant, next Message) Request {
	r.Messages = append(append([]Message(nil), r.Messages...), normalizedAssistant(answer), next)
	return r
}
func user(text string) Message {
	data, _ := json.Marshal(text)
	return Message{Role: "user", Content: data}
}
func sourceStarts(f *fakeSource) []sourceCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []sourceCall
	for _, call := range f.calls {
		if call.name == "source.start" {
			result = append(result, call)
		}
	}
	return result
}

func TestConversationToolRoundUsesOnePageAndOnlyNewMessages(t *testing.T) {
	s, f, b := newFixture(t)
	f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.read" && f.count("source.start") > 0 {
			text := `{"content":null,"tool_calls":[{"name":"lookup","arguments":{"room":"A"}}]}`
			if f.count("source.start") > 1 {
				text = `{"content":"final answer","tool_calls":[]}`
			}
			return state("complete", text), nil
		}
		return f.defaultCall(ctx, call)
	}
	r := request()
	r.Messages = []Message{{Role: "system", Content: json.RawMessage(`"time-one"`)}, user("old-body-never-repeat")}
	r.Tools = []Tool{functionTool("lookup")}
	first, err := s.Generate(context.Background(), r, "", "api", nil)
	if err != nil {
		t.Fatal(err)
	}
	result := Message{Role: "tool", ToolCallID: first.ToolCalls[0].ID, Content: json.RawMessage(`"new tool result"`)}
	next := appendAnswer(r, first, result)
	next.Messages[0].Content = json.RawMessage(`"time-two"`)
	last, err := s.Generate(context.Background(), next, "", "api", nil)
	if err != nil || deref(last.Content) != "final answer" || last.ConversationID != r.ConversationID {
		t.Fatal("continued tool round failed:", err)
	}
	starts := sourceStarts(f)
	if len(starts) != 2 || starts[0].tab != starts[1].tab || starts[0].args["mode"] != "new" || starts[1].args["mode"] != "continue" || starts[0].args["turn_id"] == starts[1].args["turn_id"] {
		t.Fatal("tool rounds did not retain one owned page with isolated turns")
	}
	prompt := starts[1].args["prompt"].(string)
	if strings.Contains(prompt, "old-body-never-repeat") || strings.Contains(prompt, "time-one") || !strings.Contains(prompt, "new tool result") || !strings.Contains(prompt, "time-two") {
		t.Fatal("continuation resent old history or omitted new context/input")
	}
	opened, closed, _ := b.counts()
	if opened != 1 || closed != 0 {
		t.Fatal("one conversation must own one retained page")
	}
}

func TestStudentFormattingRemainsInsideTheToolTransportOnEveryTurn(t *testing.T) {
	s, f, _ := newFixture(t)
	const formatting = "只用一句话回答学生，不要向学生展示 JSON 或工具语法。"
	f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.start" {
			prompt := call.args["prompt"].(string)
			dataAt := strings.Index(prompt, "CONVERSATION DATA:")
			endAt := strings.LastIndex(prompt, "END CONVERSATION DATA.")
			if dataAt < 0 || endAt <= dataAt || !strings.Contains(prompt[endAt:], `"tool_calls":[]`) || !strings.Contains(prompt[:dataAt], "apply ONLY to the text inside content") {
				t.Fatal("student formatting could replace the outer tool transport envelope")
			}
			if f.count("source.start") == 1 && !strings.Contains(prompt[dataAt:endAt], formatting) {
				t.Fatal("transport adaptation discarded the caller's answer-format instruction")
			}
		}
		if call.name == "source.read" && f.count("source.start") > 0 {
			if f.count("source.start") == 1 {
				return state("complete", `{"content":null,"tool_calls":[{"name":"lookup","arguments":{"room":"A"}}]}`), nil
			}
			return state("complete", `{"content":"已查到教室安排。","tool_calls":[]}`), nil
		}
		return f.defaultCall(ctx, call)
	}
	r := request()
	r.Messages = []Message{{Role: "system", Content: json.RawMessage(`"` + formatting + `"`)}, user("查询后只给我一句话。")}
	r.Tools = []Tool{functionTool("lookup")}
	first, err := s.Generate(context.Background(), r, "", "api", nil)
	if err != nil {
		t.Fatal(err)
	}
	r = appendAnswer(r, first, Message{Role: "tool", ToolCallID: first.ToolCalls[0].ID, Content: json.RawMessage(`"教室安排已确认"`)})
	last, err := s.Generate(context.Background(), r, "", "api", nil)
	if err != nil || deref(last.Content) != "已查到教室安排。" || len(last.ToolCalls) != 0 {
		t.Fatal("the ordinary final answer was not decoded from its transport envelope:", err)
	}
}

func TestConversationNormalizesTextPartsAndToolArgumentJSON(t *testing.T) {
	s, f, _ := newFixture(t)
	f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.read" && f.count("source.start") > 0 {
			if f.count("source.start") == 1 {
				return state("complete", `{"content":null,"tool_calls":[{"name":"lookup","arguments":{"b":2,"a":1}}]}`), nil
			}
			return state("complete", `{"content":"done","tool_calls":[]}`), nil
		}
		return f.defaultCall(ctx, call)
	}
	r := request()
	r.Messages[0].Content = json.RawMessage(`[{"type":"text","text":"hel"},{"type":"text","text":"lo"}]`)
	r.Tools = []Tool{{Type: "function", Function: Function{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	answer, err := s.Generate(context.Background(), r, "", "api", nil)
	if err != nil {
		t.Fatal(err)
	}
	next := appendAnswer(r, answer, Message{Role: "tool", ToolCallID: answer.ToolCalls[0].ID, Content: json.RawMessage(`"result"`)})
	next.Messages[0].Content = json.RawMessage(`"hello"`)
	next.Messages[1].Content = json.RawMessage(`""`)
	next.Messages[1].ToolCalls = append([]ToolCall(nil), next.Messages[1].ToolCalls...)
	next.Messages[1].ToolCalls[0].Function.Arguments = ` { "a":1.0,"b":2 } `
	if _, err := s.Generate(context.Background(), next, "", "api", nil); err != nil {
		t.Fatal("equivalent typed history falsely forked:", err)
	}
}

func TestConversationRejectsBranchesAndRequiresExplicitRetry(t *testing.T) {
	s, f, b := newFixture(t)
	r := request()
	answer, err := s.Generate(context.Background(), r, "", "api", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Generate(context.Background(), r, "", "api", nil)
	errorCode(t, err, "conversation_retry_required")
	branched := appendAnswer(r, answer, user("next"))
	branched.Messages[0] = user("changed past")
	_, err = s.Generate(context.Background(), branched, "", "api", nil)
	errorCode(t, err, "conversation_fork")
	if f.count("source.start") != 1 {
		t.Fatal("branch/repeat silently resent a request")
	}
	r.Retry = true
	if recovered, err := s.Generate(context.Background(), r, "", "api", nil); err != nil || deref(recovered.Content) != deref(answer.Content) {
		t.Fatal("explicit retry failed:", err)
	}
	starts := sourceStarts(f)
	if len(starts) != 1 {
		t.Fatal("recovering an authoritative answer resent a model request")
	}
	opened, closed, _ := b.counts()
	if opened != 1 || closed != 0 {
		t.Fatal("retry created/replaced a page")
	}
}

func TestFailedRequestExplicitRetryAppendsButUnknownConversationRetryCreatesOnce(t *testing.T) {
	s, f, b := newFixture(t)
	f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.read" && f.count("source.start") == 1 {
			return state("error", ""), nil
		}
		return f.defaultCall(ctx, call)
	}
	r := request()
	_, err := s.Generate(context.Background(), r, "", "api", nil)
	errorCode(t, err, "provider_unavailable")
	r.Retry = true
	if _, err := s.Generate(context.Background(), r, "", "api", nil); err != nil {
		t.Fatal(err)
	}
	starts := sourceStarts(f)
	if len(starts) != 2 || starts[0].tab != starts[1].tab || starts[1].args["mode"] != "continue" || !strings.Contains(starts[1].args["prompt"].(string), "Explicitly retry") {
		t.Fatal("failed explicit retry did not append to its owned page")
	}
	unknown := request()
	unknown.ConversationID = "not-previously-accepted"
	unknown.Retry = true
	if _, err := s.Generate(context.Background(), unknown, "", "api", nil); err != nil {
		t.Fatal("explicit retry before any acceptance could not create a first session:", err)
	}
	starts = sourceStarts(f)
	if starts[2].args["mode"] != "new" {
		t.Fatal("an unknown conversation guessed a prior page")
	}
	opened, _, _ := b.counts()
	if opened != 2 {
		t.Fatal("explicit retry created an extra page")
	}
}

func TestLostValidatedResponseRecoversSameToolCallsWithoutAnotherStart(t *testing.T) {
	s, f, _ := newFixture(t)
	f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.read" && f.count("source.start") > 0 {
			return state("complete", `{"content":null,"tool_calls":[{"name":"lookup","arguments":{"room":"A"}}]}`), nil
		}
		return f.defaultCall(ctx, call)
	}
	r := request()
	r.Tools = []Tool{functionTool("lookup")}
	var original []ToolCall
	_, err := s.Generate(context.Background(), r, "", "api", func(delta Delta) error {
		if len(delta.ToolCalls) > 0 {
			original = append([]ToolCall(nil), delta.ToolCalls...)
			return errors.New("consumer lost the response")
		}
		return nil
	})
	errorCode(t, err, "stream_write_failed")
	r.Retry = true
	recovered, err := s.Generate(context.Background(), r, "", "api", nil)
	if err != nil || len(original) != 1 || len(recovered.ToolCalls) != 1 || original[0] != recovered.ToolCalls[0] || f.count("source.start") != 1 {
		t.Fatal("authoritative tool recovery regenerated IDs or repeated a send:", err)
	}
	changed := r
	changed.Messages = append([]Message(nil), r.Messages...)
	changed.Messages = append(changed.Messages, Message{Role: "system", Content: json.RawMessage(`"different context"`)})
	_, err = s.Generate(context.Background(), changed, "", "api", nil)
	errorCode(t, err, "conversation_retry_mismatch")
	if f.count("source.start") != 1 {
		t.Fatal("changed cache identity reached the model")
	}
}

func TestCancelledPartialAssistantCanContinueWithoutForkOrResendingPartial(t *testing.T) {
	s, f, b := newFixture(t)
	f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.read" && f.count("source.start") > 0 {
			if f.count("source.start") == 1 {
				return state("generating", "partial response"), nil
			}
			return state("complete", "new answer"), nil
		}
		return f.defaultCall(ctx, call)
	}
	r := request()
	ctx, cancel := context.WithCancel(context.Background())
	_, err := s.Generate(ctx, r, "", "api", func(delta Delta) error {
		if delta.Content != "" {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("partial generation did not cancel:", err)
	}
	next := r
	next.Messages = append(append([]Message(nil), r.Messages...), Message{Role: "assistant", Content: json.RawMessage(`"partial"`)}, user("next user message"))
	answer, err := s.Generate(context.Background(), next, "", "api", nil)
	if err != nil || deref(answer.Content) != "new answer" {
		t.Fatal("partial continuation falsely forked:", err)
	}
	starts := sourceStarts(f)
	prompt := starts[1].args["prompt"].(string)
	if len(starts) != 2 || starts[0].tab != starts[1].tab || strings.Contains(prompt, "partial") || strings.Contains(prompt, `"question"`) || !strings.Contains(prompt, "next user message") {
		t.Fatal("partial/old request was sent again instead of only the new user input")
	}
	opened, closed, _ := b.counts()
	if opened != 1 || closed != 0 {
		t.Fatal("cancel/new user replaced or discarded the owned conversation")
	}
}

func TestFailedConversationAcceptsANewUserWithoutInventedAssistant(t *testing.T) {
	s, f, b := newFixture(t)
	f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.read" && f.count("source.start") == 1 {
			return state("error", ""), nil
		}
		return f.defaultCall(ctx, call)
	}
	r := request()
	_, err := s.Generate(context.Background(), r, "", "api", nil)
	errorCode(t, err, "provider_unavailable")
	next := r
	next.Messages = append(append([]Message(nil), r.Messages...), user("new instruction"))
	if _, err := s.Generate(context.Background(), next, "", "api", nil); err != nil {
		t.Fatal("failed history forced a fake assistant or new page:", err)
	}
	starts := sourceStarts(f)
	if len(starts) != 2 || starts[0].tab != starts[1].tab || strings.Contains(starts[1].args["prompt"].(string), `"question"`) || !strings.Contains(starts[1].args["prompt"].(string), "new instruction") {
		t.Fatal("failed conversation continuation duplicated accepted input")
	}
	opened, _, _ := b.counts()
	if opened != 1 {
		t.Fatal("failure silently reopened a page")
	}
}

func TestTurnMismatchNeverReadsOrStopsAnotherTurn(t *testing.T) {
	s, f, _ := newFixture(t)
	f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.read" && f.count("source.start") > 0 {
			return map[string]any{"state": "complete", "text": "other turn", "turn_id": "foreign-turn"}, nil
		}
		return f.defaultCall(ctx, call)
	}
	_, err := s.Generate(context.Background(), request(), "", "api", nil)
	errorCode(t, err, "model_turn_mismatch")
	starts := sourceStarts(f)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call.name == "source.stop" && (call.args["turn_id"] != starts[0].args["turn_id"] || call.args["turn_id"] == "foreign-turn") {
			t.Fatal("cleanup targeted a different turn")
		}
	}
}

func TestConversationScopeIsolationAndCloseNeverTouchesUnownedTabs(t *testing.T) {
	s, _, b := newFixture(t)
	b.mu.Lock()
	b.opened = append(b.opened, browser.Tab{ID: "external-user-tab", BrowserID: "profile-a"})
	b.active++
	b.mu.Unlock()
	r := request()
	if _, err := s.Generate(context.Background(), r, "", "scope-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Generate(context.Background(), r, "", "scope-b", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseConversation(context.Background(), "not-owner", r.Model, r.ConversationID); err != nil {
		t.Fatal(err)
	}
	if _, closed, _ := b.counts(); closed != 0 {
		t.Fatal("an unknown scope closed another caller's page")
	}
	if err := s.CloseConversation(context.Background(), "scope-a", r.Model, r.ConversationID); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseConversation(context.Background(), "scope-a", r.Model, r.ConversationID); err != nil {
		t.Fatal("repeated close was not idempotent:", err)
	}
	if _, closed, _ := b.counts(); closed != 1 {
		t.Fatal("repeated close touched an unrelated resource")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.closed) != 2 {
		t.Fatal("CloseAll did not close both owned conversations")
	}
	for _, tab := range b.closed {
		if tab.ID == "external-user-tab" {
			t.Fatal("runtime closed a tab it did not create")
		}
	}
}

func TestClosingAnActiveConversationCancelsOnlyItsOwnedTurn(t *testing.T) {
	s, f, b := newFixture(t)
	started := make(chan struct{})
	var once sync.Once
	f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.start" {
			value, _ := f.defaultCall(ctx, call)
			once.Do(func() { close(started) })
			return value, nil
		}
		if call.name == "source.read" && f.count("source.start") > 0 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return f.defaultCall(ctx, call)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Generate(context.Background(), request(), "", "api", nil); done <- err }()
	<-started
	if err := s.CloseConversation(context.Background(), "api", request().Model, request().ConversationID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("close did not cancel active generation:", err)
	}
	_, closed, _ := b.counts()
	if closed != 1 || f.count("source.stop") != 1 {
		t.Fatal("active close did not stop one turn and close one owned page")
	}
}

func TestConcurrentCloseCallsReleaseOneBindingOnlyOnce(t *testing.T) {
	s, _, b := newFixture(t)
	r := request()
	if _, err := s.Generate(context.Background(), r, "", "api", nil); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	b.closeHook = func(context.Context, string) error {
		first.Do(func() { close(entered); <-release })
		return nil
	}
	results := make(chan error, 16)
	go func() { results <- s.CloseConversation(context.Background(), "api", r.Model, r.ConversationID) }()
	<-entered
	for i := 1; i < 16; i++ {
		go func() { results <- s.CloseConversation(context.Background(), "api", r.Model, r.ConversationID) }()
	}
	close(release)
	for i := 0; i < 16; i++ {
		if err := <-results; err != nil {
			t.Fatal("concurrent close was not idempotent:", err)
		}
	}
	if _, closed, _ := b.counts(); closed != 1 {
		t.Fatal("concurrent closes invoked browser closure more than once")
	}
}

func TestConversationCapacityRejectsWithoutEvictionAndCloseReleasesSlot(t *testing.T) {
	s, _, b := newFixture(t)
	for i := 0; i < 32; i++ {
		r := request()
		r.ConversationID = fmt.Sprintf("conversation-%d", i)
		if _, err := s.Generate(context.Background(), r, "", "api", nil); err != nil {
			t.Fatal(err)
		}
	}
	r := request()
	r.ConversationID = "overflow"
	_, err := s.Generate(context.Background(), r, "", "api", nil)
	errorCode(t, err, "conversation_capacity")
	opened, closed, _ := b.counts()
	if opened != 32 || closed != 0 {
		t.Fatal("capacity silently evicted or opened a page")
	}
	if err := s.CloseConversation(context.Background(), "api", r.Model, "conversation-0"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Generate(context.Background(), r, "", "api", nil); err != nil {
		t.Fatal("explicit close did not release capacity:", err)
	}
}

func TestCloseAllCancelsActiveRequestsBeforeClosingOwnedPages(t *testing.T) {
	s, f, b := newFixture(t)
	started := make(chan struct{})
	var once sync.Once
	f.hook = func(ctx context.Context, call sourceCall) (map[string]any, error) {
		if call.name == "source.start" {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return f.defaultCall(ctx, call)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Generate(context.Background(), request(), "", "api", nil); done <- err }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.CloseAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, closed, _ := b.counts()
	if closed != 1 || f.count("source.stop") != 1 {
		t.Fatal("shutdown did not stop before closing")
	}
	_, err := s.Generate(context.Background(), request(), "", "api", nil)
	errorCode(t, err, "model_unavailable")
}
