package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AdminRequest performs an authenticated JSON call against the local-only daemon
// control plane (/admin/*) without inheriting any system HTTP proxy.
func AdminRequest(ctx context.Context, address, token, method, path string, payload any, out any) error {
	if address == "" || token == "" {
		return errors.New("local.token is required; run lrmcp setup local first")
	}
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+address+path, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := loopbackClient(token, 15*time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connect local daemon at %s: %w", address, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var apiErr struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &apiErr) == nil && apiErr.Error.Message != "" {
			return errors.New(apiErr.Error.Message)
		}
		return fmt.Errorf("local daemon returned HTTP %d", resp.StatusCode)
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode daemon response: %w", err)
		}
	}
	return nil
}

// AdminStream opens an authenticated streaming GET request against the local
// daemon (for SSE tail or NDJSON export). The caller must close resp.Body.
func AdminStream(ctx context.Context, address, token, path string) (*http.Response, error) {
	if address == "" || token == "" {
		return nil, errors.New("local.token is required; run lrmcp setup local first")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+path, nil)
	if err != nil {
		return nil, err
	}
	client := loopbackClient(token, 0)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect local daemon at %s: %w", address, err)
	}
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		var apiErr struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &apiErr) == nil && apiErr.Error.Message != "" {
			return nil, errors.New(apiErr.Error.Message)
		}
		return nil, fmt.Errorf("local daemon returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}
