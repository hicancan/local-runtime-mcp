package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// loopbackClient returns an HTTP client that connects directly to loopback
// without inheriting any system HTTP proxy.
func loopbackClient(token string, timeout time.Duration) *http.Client {
	base := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: localBearerRoundTripper{base: base, token: token},
	}
}

type localBearerRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (t localBearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

// ProbeLocal checks whether a local-runtime daemon is already listening on the
// configured loopback address and accepts the local owner token.
func ProbeLocal(ctx context.Context, address, token string) bool {
	if address == "" || token == "" {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://"+address+"/health", nil)
	if err != nil {
		return false
	}
	client := loopbackClient(token, 500*time.Millisecond)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var payload struct {
		OK bool `json:"ok"`
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&payload) == nil && payload.OK
}

// ProxyStdioToLocal attaches a stdio MCP client to an already-running local
// runtime daemon via an authenticated loopback HTTP 101 full-duplex stream.
// This preserves exact MCP stdio framing, every protocol version, cancellations,
// and live server-initiated notifications (such as tools/list_changed).
func ProxyStdioToLocal(ctx context.Context, address, token string, stdin io.Reader, stdout io.Writer) error {
	if address == "" || token == "" {
		return errors.New("local address and token are required to attach")
	}
	dialer := &net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("connect local runtime daemon: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	}
	handshake := fmt.Sprintf(
		"POST /mcp/attach HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n",
		address, token, localUpgradeProto,
	)
	if _, err := io.WriteString(conn, handshake); err != nil {
		return fmt.Errorf("send local attach handshake: %w", err)
	}
	reader := bufio.NewReaderSize(conn, 64*1024)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil {
		return fmt.Errorf("read local attach response: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("local runtime daemon rejected attach: HTTP %d", resp.StatusCode)
	}
	_ = conn.SetDeadline(time.Time{})

	stopWatch := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopWatch()

	type copyResult struct {
		side string
		err  error
	}
	done := make(chan copyResult, 2)

	go func() {
		_, err := io.Copy(conn, stdin)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- copyResult{"stdin", err}
	}()
	go func() {
		_, err := io.Copy(stdout, reader)
		done <- copyResult{"daemon", err}
	}()

	first := <-done
	_ = conn.Close()
	if first.side == "stdin" {
		<-done
		if first.err == nil || errors.Is(first.err, io.EOF) || errors.Is(first.err, net.ErrClosed) || ctx.Err() != nil {
			return nil
		}
		return first.err
	}
	// Daemon closed the connection; stdin may remain blocked in a caller-owned
	// reader (e.g. os.Stdin), so do not block waiting on stdin to return.
	if ctx.Err() != nil || first.err == nil || errors.Is(first.err, io.EOF) || errors.Is(first.err, net.ErrClosed) {
		return nil
	}
	return first.err
}
