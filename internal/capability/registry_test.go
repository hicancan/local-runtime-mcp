package capability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hicancan/local-runtime-mcp/internal/browser"
)

const definitionJSON = `{
  "title":"Echo a page value",
  "description":"Read a value from the selected page",
  "origins":["https://example.test"],
  "inputSchema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false},
  "outputSchema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false},
  "annotations":{"readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":true}
}`

type fakeBrowser struct {
	tabs     []browser.Tab
	tabCalls int
	actions  []browser.Action
	act      func(context.Context, browser.Action) (browser.ActionResult, error)
}

func (b *fakeBrowser) Tabs(ctx context.Context, instance string) ([]browser.Tab, error) {
	b.tabCalls++
	return b.tabs, ctx.Err()
}

func (b *fakeBrowser) Act(ctx context.Context, action browser.Action) (browser.ActionResult, error) {
	b.actions = append(b.actions, action)
	if b.act != nil {
		return b.act(ctx, action)
	}
	return browser.ActionResult{Success: true, Value: map[string]any{"text": "answer"}}, nil
}

func writeCapability(t *testing.T, root, name, definition, script string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".json", []byte(definition), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".js", []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
}

func registryFixture(t *testing.T) (*Registry, *fakeBrowser) {
	t.Helper()
	root := t.TempDir()
	writeCapability(t, root, "example/echo", definitionJSON, "async function(input) { return {text:input.text}; }")
	b := &fakeBrowser{tabs: []browser.Tab{{ID: "tab", BrowserID: "profile", URL: "https://example.test/course?private=not-returned"}}}
	r, err := Load(root, b)
	if err != nil {
		t.Fatal(err)
	}
	return r, b
}

func TestLoadTreeHasOneIdentityAndDetachedDefinitions(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "z/read", definitionJSON, "input => ({text:input.text})")
	writeCapability(t, root, "a/read", definitionJSON, "input => ({text:input.text})")
	r, err := Load(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	listed := r.List()
	if len(listed) != 2 || listed[0].Name != "a.read" || listed[1].Name != "z.read" {
		t.Fatal(listed)
	}
	listed[0].Origins[0] = "https://other.test"
	listed[0].InputSchema[0] = '!'
	listed[0].Annotations.ReadOnlyHint = false
	actual := r.List()[0]
	if actual.Origins[0] != "https://example.test" || actual.InputSchema[0] != '{' || !actual.Annotations.ReadOnlyHint {
		t.Fatal("List exposed mutable registry state")
	}
	empty, err := Load("", nil)
	if err != nil || len(empty.List()) != 0 {
		t.Fatal("empty configuration must disable capabilities")
	}
	if _, err := Load(filepath.Join(root, "missing"), nil); err == nil {
		t.Fatal("configured missing directory was silently ignored")
	}
}

func TestLoadRejectsInvalidOrAmbiguousDefinitions(t *testing.T) {
	for _, tc := range []struct{ name, definition string }{
		{"unknown-field", strings.Replace(definitionJSON, `"title":`, `"unexpected":true,"title":`, 1)},
		{"duplicate-name-source", strings.Replace(definitionJSON, `"title":`, `"name":"other","title":`, 1)},
		{"missing-hint", strings.Replace(definitionJSON, `"readOnlyHint":true,`, ``, 1)},
		{"null-hint", strings.Replace(definitionJSON, `"readOnlyHint":true`, `"readOnlyHint":null`, 1)},
		{"inconsistent-hint", strings.Replace(definitionJSON, `"destructiveHint":false`, `"destructiveHint":true`, 1)},
		{"wildcard-origin", strings.ReplaceAll(definitionJSON, "https://example.test", "https://*.example.test")},
		{"origin-path", strings.ReplaceAll(definitionJSON, "https://example.test", "https://example.test/course")},
		{"origin-credentials", strings.ReplaceAll(definitionJSON, "https://example.test", "https://user:password@example.test")},
		{"origin-not-http", strings.ReplaceAll(definitionJSON, "https://example.test", "file:///private")},
		{"origin-invalid-port", strings.ReplaceAll(definitionJSON, "https://example.test", "https://example.test:65536")},
		{"invalid-schema", strings.Replace(definitionJSON, `"inputSchema":{"type":"object"`, `"inputSchema":{"type":"string"`, 1)},
		{"remote-ref", strings.Replace(definitionJSON, `"inputSchema":{"type":"object"`, `"inputSchema":{"$ref":"https://schema.invalid/not-fetched","type":"object"`, 1)},
		{"trailing-json", definitionJSON + ` {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeCapability(t, root, "example/read", tc.definition, "input => ({text:input.text})")
			if _, err := Load(root, nil); err == nil {
				t.Fatal("invalid capability was published")
			}
		})
	}
	t.Run("missing-script", func(t *testing.T) {
		root := t.TempDir()
		writeCapability(t, root, "example/read", definitionJSON, "input => ({text:input.text})")
		if err := os.Remove(filepath.Join(root, "example", "read.js")); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(root, nil); err == nil {
			t.Fatal("missing implementation was ignored")
		}
	})
	t.Run("path-collision", func(t *testing.T) {
		root := t.TempDir()
		writeCapability(t, root, "a.b/read", definitionJSON, "input => ({text:input.text})")
		writeCapability(t, root, "a/b.read", definitionJSON, "input => ({text:input.text})")
		if _, err := Load(root, nil); err == nil {
			t.Fatal("two file paths registered the same name")
		}
	})
	t.Run("root-tool-has-no-namespace", func(t *testing.T) {
		root := t.TempDir()
		writeCapability(t, root, "browser_action", definitionJSON, "input => ({text:input.text})")
		if _, err := Load(root, nil); err == nil {
			t.Fatal("a capability can overwrite an unnamespaced built-in tool")
		}
	})
}

func TestCallRejectsWrongParametersOrTargetBeforeExecution(t *testing.T) {
	for _, tc := range []struct {
		name     string
		instance string
		tab      browser.Tab
		args     map[string]any
	}{
		{"input-type", "profile", browser.Tab{ID: "tab", URL: "https://example.test"}, map[string]any{"text": 7}},
		{"input-extra", "profile", browser.Tab{ID: "tab", URL: "https://example.test"}, map[string]any{"text": "yes", "script": "ignored"}},
		{"other-instance", "profile", browser.Tab{ID: "tab", BrowserID: "other", URL: "https://example.test"}, map[string]any{"text": "yes"}},
		{"other-tab", "profile", browser.Tab{ID: "other", URL: "https://example.test"}, map[string]any{"text": "yes"}},
		{"other-origin", "profile", browser.Tab{ID: "tab", URL: "https://evil.test"}, map[string]any{"text": "yes"}},
		{"origin-suffix", "profile", browser.Tab{ID: "tab", URL: "https://example.test.evil.test"}, map[string]any{"text": "yes"}},
		{"other-port", "profile", browser.Tab{ID: "tab", URL: "https://example.test:8443"}, map[string]any{"text": "yes"}},
		{"no-instance", "", browser.Tab{ID: "tab", URL: "https://example.test"}, map[string]any{"text": "yes"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, b := registryFixture(t)
			b.tabs = []browser.Tab{tc.tab}
			if _, err := r.Call(context.Background(), "example.echo", tc.instance, "tab", tc.args); err == nil {
				t.Fatal("call unexpectedly succeeded")
			}
			if len(b.actions) != 0 {
				t.Fatal("rejected call reached page execution")
			}
		})
	}
}

func TestCallValidatesOutputAndDoesNotReplay(t *testing.T) {
	r, b := registryFixture(t)
	b.act = func(context.Context, browser.Action) (browser.ActionResult, error) {
		return browser.ActionResult{Success: true, Value: map[string]any{"text": 9}}, nil
	}
	if _, err := r.Call(context.Background(), "example.echo", "profile", "tab", map[string]any{"text": "hi"}); err == nil {
		t.Fatal("invalid output was returned")
	}
	if len(b.actions) != 1 {
		t.Fatal("output failure replayed execution")
	}
	b.actions = nil
	b.act = func(context.Context, browser.Action) (browser.ActionResult, error) {
		return browser.ActionResult{}, errors.New("lost response")
	}
	if _, err := r.Call(context.Background(), "example.echo", "profile", "tab", map[string]any{"text": "hi"}); err == nil || len(b.actions) != 1 {
		t.Fatal("unknown side-effect outcome was retried")
	}
}

func TestCallCancellationReachesExistingBrowserExecution(t *testing.T) {
	r, b := registryFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Call(ctx, "example.echo", "profile", "tab", map[string]any{"text": "hi"}); !errors.Is(err, context.Canceled) || b.tabCalls != 0 {
		t.Fatal("canceled admission touched browser")
	}
	b.act = func(ctx context.Context, _ browser.Action) (browser.ActionResult, error) {
		<-ctx.Done()
		return browser.ActionResult{}, ctx.Err()
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := r.Call(ctx, "example.echo", "profile", "tab", map[string]any{"text": "hi"}); !errors.Is(err, context.DeadlineExceeded) || len(b.actions) != 1 {
		t.Fatal("deadline did not propagate without replay")
	}
}

func TestActualJavaScriptWrapperKeepsArgumentsAsDataAndChecksNavigation(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is used only to verify the actual emitted page expression")
	}
	r, b := registryFixture(t)
	origin := "https://example.test"
	b.act = func(ctx context.Context, action browser.Action) (browser.ActionResult, error) {
		payload, _ := json.Marshal(map[string]any{"script": action.Script, "origin": origin})
		// This is an isolated JS VM, not a real browser, page, account, or service.
		cmd := exec.CommandContext(ctx, node, "--input-type=module", "-e", `import vm from 'node:vm';
let data='';for await(const chunk of process.stdin)data+=chunk;
const p=JSON.parse(data), sandbox={location:{origin:p.origin},pwned:false};
try { const value=await vm.runInNewContext(p.script,sandbox); process.stdout.write(JSON.stringify({value,pwned:sandbox.pwned})); }
catch { process.stdout.write(JSON.stringify({error:true,pwned:sandbox.pwned})); }`)
		cmd.Stdin = bytes.NewReader(payload)
		data, err := cmd.Output()
		if err != nil {
			return browser.ActionResult{}, err
		}
		var response struct {
			Value map[string]any `json:"value"`
			Error bool           `json:"error"`
			Pwned bool           `json:"pwned"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return browser.ActionResult{}, err
		}
		if response.Pwned {
			t.Fatal("input escaped the argument value or script ran after navigation")
		}
		if response.Error {
			return browser.ActionResult{}, errors.New("page origin guard rejected execution")
		}
		return browser.ActionResult{Success: true, Value: response.Value}, nil
	}
	text := `"); globalThis.pwned=true; //   </script>`
	output, err := r.Call(context.Background(), "example.echo", "profile", "tab", map[string]any{"text": text})
	if err != nil || output["text"] != text {
		t.Fatal("literal JSON input did not survive actual page-expression execution:", err)
	}
	// The tab list still points at the allowed URL, but navigation completes
	// before evaluation. The page guard must stop the trusted script itself.
	e := r.entries["example.echo"]
	e.script = "async function(input) { globalThis.pwned=true; return {text:input.text}; }"
	r.entries["example.echo"] = e
	origin = "https://evil.test"
	if _, err := r.Call(context.Background(), "example.echo", "profile", "tab", map[string]any{"text": "hi"}); err == nil {
		t.Fatal("navigation race executed the capability on another origin")
	}
}

func TestDefaultPortOriginsUseTheBrowserSerialization(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "example/read", strings.ReplaceAll(definitionJSON, "https://example.test", "https://EXAMPLE.test:443/"), "input => ({text:input.text})")
	r, err := Load(root, nil)
	if err != nil || r.List()[0].Origins[0] != "https://example.test" {
		t.Fatal("origin is incompatible with location.origin serialization:", err)
	}
}

func TestShippedDeepSeekCapabilitiesLoadAndDeclareTheirActualEffects(t *testing.T) {
	r, err := Load(filepath.Join("..", "..", "capabilities"), nil)
	if err != nil {
		t.Fatal("the shipped capability files do not satisfy the production loader:", err)
	}
	wanted := map[string]bool{"deepseek.start": false, "deepseek.read": true, "deepseek.stop": false}
	for _, d := range r.List() {
		readOnly, ok := wanted[d.Name]
		if !ok {
			continue
		}
		if d.Annotations.ReadOnlyHint != readOnly || len(d.Origins) != 1 || d.Origins[0] != "https://chat.deepseek.com" {
			t.Fatal("shipped tool has an unexpected effect or target declaration:", d.Name)
		}
		if d.Name == "deepseek.start" && d.Annotations.IdempotentHint {
			t.Fatal("sending a prompt must not be advertised as safe to replay")
		}
		delete(wanted, d.Name)
	}
	if len(wanted) != 0 {
		t.Fatal("missing shipped DeepSeek capabilities")
	}
}
