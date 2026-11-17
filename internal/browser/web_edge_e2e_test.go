//go:build windows

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEdgeWebBridgeSDKOriginAndStreaming(t *testing.T) {
	if os.Getenv("LOCAL_RUNTIME_MCP_BROWSER_E2E") != "1" {
		t.Skip("set LOCAL_RUNTIME_MCP_BROWSER_E2E=1 to launch isolated Edge")
	}
	edge := findEdge()
	if edge == "" {
		t.Skip("Microsoft Edge was not found")
	}
	bridge, ctx := testBridge(t)
	var ownerURL string
	var mutex sync.Mutex
	var sources []WebSource
	canceled := make(chan struct{})
	bridge.SetWebHandler(func(ctx context.Context, source WebSource, request WebRequest, emit func(WebEvent) error) (any, error) {
		mutex.Lock()
		sources = append(sources, source)
		mutex.Unlock()
		if source.Origin != ownerURL {
			return nil, fmt.Errorf("origin not granted")
		}
		if request.Method != "models.chat" {
			return nil, fmt.Errorf("method not granted")
		}
		if strings.Contains(string(request.Params), "error-code-fixture") {
			return nil, testWebCodedError{}
		}
		if strings.Contains(string(request.Params), "cancel-fixture") {
			if err := emit(WebEvent{Type: "keepalive"}); err != nil {
				return nil, err
			}
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		}
		for _, event := range []WebEvent{{Type: "keepalive"}, {Type: "text_delta", Data: "hello"}, {Type: "tool_delta", Data: map[string]any{"name": "fixture"}}} {
			if err := emit(event); err != nil {
				return nil, err
			}
		}
		return map[string]any{"role": "assistant", "content": "hello"}, nil
	})
	modules := func(w http.ResponseWriter, r *http.Request) bool {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name != "web_sdk.js" && name != "web_protocol.js" {
			return false
		}
		body, err := extensionFiles.ReadFile("extension/" + name)
		if err != nil {
			http.Error(w, "missing module", 500)
			return true
		}
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write(body)
		return true
	}
	child := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if modules(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/iframe" {
			fmt.Fprint(w, `<!doctype html><p>iframe fixture</p><script>parent.postMessage({protocol:'lrmcp.web.v1',kind:'request',id:'iframe-forgery',method:'models.list',params:{}},'*')</script>`)
			return
		}
		fmt.Fprint(w, `<!doctype html><p id="status">waiting</p><script type="module">import{createLRMCPClient}from'/web_sdk.js';try{await createLRMCPClient().listModels({timeoutMs:5000});document.querySelector('#status').textContent='FAIL: denied origin succeeded'}catch(e){document.querySelector('#status').textContent=e.message.includes('origin not granted')?'DENIED_OK':'FAIL: '+e.message}</script>`)
	}))
	defer child.Close()
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if modules(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/coded-error" {
			fmt.Fprint(w, `<!doctype html><p id="status">waiting</p><script type="module">import{createLRMCPClient,LocalRuntimeError}from'/web_sdk.js';try{await createLRMCPClient().chat({messages:[{role:'user',content:'error-code-fixture'}]}).result;document.querySelector('#status').textContent='FAIL: coded error reported completion'}catch(e){document.querySelector('#status').textContent=e instanceof LocalRuntimeError&&e.code==='provider_unavailable'?'ERROR_CODE_OK':'FAIL: public error code lost'}</script>`)
			return
		}
		if r.URL.Path == "/cancel" {
			fmt.Fprint(w, `<!doctype html><p id="status">waiting</p><script type="module">import{createLRMCPClient}from'/web_sdk.js';try{const controller=new AbortController();const call=createLRMCPClient().chat({messages:[{role:'user',content:'cancel-fixture'}]},{signal:controller.signal,timeoutMs:5000});for await(const item of call){controller.abort()}document.querySelector('#status').textContent='FAIL: cancel reported completion'}catch(e){document.querySelector('#status').textContent=e.message.includes('outcome may be unknown')?'CANCEL_OK':'FAIL: '+e.message}</script>`)
			return
		}
		fmt.Fprintf(w, `<!doctype html><p id="status">waiting</p><iframe src=%q></iframe><script type="module">import{createLRMCPClient}from'/web_sdk.js';try{const call=createLRMCPClient().chat({messages:[{role:'user',content:'fixture'}]},{timeoutMs:5000});const events=[];for await(const item of call)events.push(item.type);const result=await call.result;document.querySelector('#status').textContent=result.content==='hello'&&events.join(',')==='keepalive,text_delta,tool_delta'?'STREAM_OK':'FAIL: wrong stream'}catch(e){document.querySelector('#status').textContent='FAIL: '+e.message}</script>`, child.URL+"/iframe")
	}))
	defer owner.Close()
	ownerURL = owner.URL
	extension, err := InstallExtension(filepath.Join(t.TempDir(), "extension"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ConfigureExtension(extension, bridge.Status().Address, testToken); err != nil {
		t.Fatal(err)
	}
	launchIsolatedEdge(t, edge, extension, owner.URL)
	deadline := time.Now().Add(20 * time.Second)
	for !bridge.Status().Connected && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !bridge.Status().Connected {
		t.Fatal("isolated web bridge did not connect")
	}
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	browserID := bridge.Status().Instances[0].BrowserID
	tabs, err := bridge.Tabs(callCtx, browserID)
	if err != nil {
		t.Fatal(err)
	}
	var ownerTab string
	for _, tab := range tabs {
		if strings.HasPrefix(tab.URL, owner.URL) {
			ownerTab = tab.ID
		}
	}
	if ownerTab == "" {
		t.Fatal("owner fixture tab missing")
	}
	awaitStatus := func(tabID, want string) {
		t.Helper()
		until := time.Now().Add(8 * time.Second)
		for time.Now().Before(until) {
			value, err := bridge.Act(callCtx, Action{TabID: tabID, Kind: "evaluate", Script: "document.querySelector('#status')?.textContent"})
			if err != nil {
				t.Fatal(err)
			}
			if value.Value == want {
				return
			}
			if text, ok := value.Value.(string); ok && strings.HasPrefix(text, "FAIL:") {
				t.Fatal(text)
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("fixture did not reach %s", want)
	}
	awaitStatus(ownerTab, "STREAM_OK")
	denied, err := bridge.Open(callCtx, browserID, child.URL+"/denied", false, "")
	if err != nil {
		t.Fatal(err)
	}
	awaitStatus(denied.ID, "DENIED_OK")
	cancelTab, err := bridge.Open(callCtx, browserID, owner.URL+"/cancel", false, "")
	if err != nil {
		t.Fatal(err)
	}
	awaitStatus(cancelTab.ID, "CANCEL_OK")
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("SDK cancellation did not reach the host handler")
	}
	codedTab, err := bridge.Open(callCtx, browserID, owner.URL+"/coded-error", false, "")
	if err != nil {
		t.Fatal(err)
	}
	awaitStatus(codedTab.ID, "ERROR_CODE_OK")
	mutex.Lock()
	defer mutex.Unlock()
	if len(sources) != 4 {
		t.Fatalf("iframe or duplicate request escaped source binding: %+v", sources)
	}
	for _, source := range sources {
		if source.FrameID != 0 || source.BrowserID != browserID || source.DocumentID == "" {
			t.Fatalf("real sender identity missing: %+v", source)
		}
	}
	t.Log("real MV3 content sender -> worker -> authenticated Go callback -> SDK text/tool/keepalive stream passed; foreign origin denied, cross-origin iframe forgery ignored, SDK cancellation reached host context without replay")
}
