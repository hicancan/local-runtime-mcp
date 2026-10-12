package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This checks the raw HTTP JSON-RPC result, not the SDK client's projection of
// structuredContent. The pixels belong to a generated fixture, not a desktop.
func TestHTTPNativeImageAndStructuredContent(t *testing.T) {
	fixture := image.NewRGBA(image.Rect(0, 0, 3, 2))
	fixture.Set(1, 1, color.RGBA{R: 31, G: 127, B: 223, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, fixture); err != nil {
		t.Fatal(err)
	}
	pixels := encoded.Bytes()
	type desktopState struct {
		Width    int    `json:"width"`
		Height   int    `json:"height"`
		MIMEType string `json:"mime_type"`
		StateID  string `json:"state_id"`
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "image-delivery-test", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "computer_state", Description: "Return the test fixture image and dimensions."}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, desktopState, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png", Data: pixels}}},
			desktopState{Width: 3, Height: 2, MIMEType: "image/png", StateID: "fixture-observation"}, nil
	})
	instance := startHTTPFixture(t, server, HTTPConfig{Listen: "127.0.0.1:0", BearerToken: testBearerToken})
	endpoint := "http://" + instance.Address() + "/mcp"
	initialized := request(t, endpoint, testBearerToken, "", "")
	initialized.Body.Close()
	if initialized.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d", initialized.StatusCode)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"computer_state","arguments":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testBearerToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("raw HTTP result status/type = %d %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	var envelope struct {
		Result struct {
			Content []struct {
				Type     string `json:"type"`
				MIMEType string `json:"mimeType"`
				Data     string `json:"data"`
			} `json:"content"`
			Structured desktopState `json:"structuredContent"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Error) != 0 || len(envelope.Result.Content) != 1 || envelope.Result.Content[0].Type != "image" {
		t.Fatalf("raw response did not contain one image block: %+v", envelope)
	}
	block := envelope.Result.Content[0]
	decoded, err := base64.StdEncoding.DecodeString(block.Data)
	if err != nil || !bytes.Equal(decoded, pixels) || block.MIMEType != "image/png" {
		t.Fatal("native PNG image block changed in HTTP transport")
	}
	actual, err := png.Decode(bytes.NewReader(decoded))
	if err != nil || actual.Bounds().Dx() != 3 || actual.Bounds().Dy() != 2 {
		t.Fatalf("decoded fixture pixels/dimensions mismatch: %v", err)
	}
	red, green, blue, alpha := actual.At(1, 1).RGBA()
	if red != 31*257 || green != 127*257 || blue != 223*257 || alpha != 65535 {
		t.Fatal("fixture pixel was not preserved")
	}
	if state := envelope.Result.Structured; state.Width != 3 || state.Height != 2 || state.MIMEType != "image/png" || state.StateID != "fixture-observation" {
		t.Fatalf("structured metadata changed: %+v", state)
	}
}

func startHTTPFixture(t *testing.T, server *mcp.Server, cfg HTTPConfig) *HTTPServer {
	t.Helper()
	instance, err := NewHTTP(server, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- instance.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("HTTP fixture shutdown: %v", err)
		}
	})
	return instance
}
