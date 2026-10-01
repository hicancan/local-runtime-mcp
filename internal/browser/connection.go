package browser

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

const keepaliveInterval = 20 * time.Second

type connectionMessage struct {
	Type     string `json:"type"`
	Token    string `json:"token,omitempty"`
	Peer     Peer   `json:"peer,omitempty"`
	BridgeID string `json:"bridge_id,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (b *Bridge) socketHandshake(_ *websocket.Config, request *http.Request) error {
	host, port, err := net.SplitHostPort(request.Host)
	_, expectedPort, _ := net.SplitHostPort(b.address)
	if err != nil || port != expectedPort || host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
		return errors.New("browser bridge requires its loopback host")
	}
	origin, err := url.Parse(request.Header.Get("Origin"))
	if err != nil || origin.Scheme != "chrome-extension" || len(origin.Host) != 32 || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || origin.User != nil {
		return errors.New("browser bridge requires an extension origin")
	}
	for _, c := range origin.Host {
		if c < 'a' || c > 'p' {
			return errors.New("invalid extension origin")
		}
	}
	if request.URL.RawQuery != "" {
		return errors.New("connection URL must not contain credentials")
	}
	return nil
}

func (b *Bridge) connect(socket *websocket.Conn) {
	b.mu.Lock()
	if b.ctx.Err() != nil || len(b.sockets) >= 64 {
		b.mu.Unlock()
		return
	}
	b.sockets[socket] = struct{}{}
	b.socketWG.Add(1)
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.sockets, socket); b.mu.Unlock(); b.socketWG.Done() }()
	socket.MaxPayloadBytes = 4096
	_ = socket.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hello connectionMessage
	if err := websocket.JSON.Receive(socket, &hello); err != nil {
		return
	}
	if hello.Type != "authenticate" || len(hello.Token) != len(b.token) || subtle.ConstantTimeCompare([]byte(hello.Token), []byte(b.token)) != 1 {
		_ = sendConnection(socket, connectionMessage{Type: "error", Error: "unauthorized"})
		return
	}
	peer := hello.Peer
	if peer.ExtensionVersion != ExtensionVersion {
		_ = sendConnection(socket, connectionMessage{Type: "error", Error: "browser extension version does not match lrmcp; run lrmcp setup browser and reload the extension"})
		return
	}
	if strings.TrimSpace(peer.InstanceID) == "" || len(peer.InstanceID) > 128 || strings.TrimSpace(peer.BootID) == "" || len(peer.BootID) > 128 || len(peer.Label) > 256 || len(peer.Browser) > 1024 {
		_ = sendConnection(socket, connectionMessage{Type: "error", Error: "invalid peer metadata"})
		return
	}
	b.mu.Lock()
	old := b.instances[peer.InstanceID]
	if b.ctx.Err() != nil || old == nil && len(b.instances) >= 32 {
		b.mu.Unlock()
		return
	}
	var oldSocket *websocket.Conn
	if old != nil {
		oldSocket = old.socket
	}
	// Each authenticated stream has a fresh queue. Commands from a disconnected
	// stream are canceled, never replayed after a reconnection.
	if old != nil {
		for tabID, route := range b.tabs {
			if route.browserID == peer.InstanceID {
				delete(b.tabs, tabID)
			}
		}
		for id, pending := range b.pending {
			if pending.browserID != peer.InstanceID {
				continue
			}
			if !pending.abandoned {
				pending.result <- response{Error: "browser extension restarted or reconnected; operation outcome may be unknown"}
			}
			pending.abandoned = true
			pending.cancel()
			if old.peer.BootID != peer.BootID {
				if pending.stopGateWatch != nil {
					pending.stopGateWatch()
				}
				if pending.release != nil {
					pending.release()
				}
				delete(b.pending, id)
			} else if !pending.dispatched || pending.release == nil && !pending.authorizing {
				delete(b.pending, id)
			}
		}
	}
	current := &instance{peer: peer, lastSeen: time.Now(), commands: make(chan command, 64), controls: make(chan command, 256), socket: socket}
	b.instances[peer.InstanceID] = current
	b.mu.Unlock()
	if oldSocket != nil {
		_ = oldSocket.SetDeadline(time.Now())
		_ = oldSocket.Close()
	}
	slog.Info("browser extension connected", "browser_id", peer.InstanceID, "extension_version", peer.ExtensionVersion)
	disconnectReason := "stream_closed"
	defer func() {
		_ = socket.Close()
		b.mu.Lock()
		isCurrent := b.instances[peer.InstanceID] == current
		if isCurrent {
			current.socket = nil
			for id, pending := range b.pending {
				if pending.browserID != peer.InstanceID || pending.bootID != peer.BootID {
					continue
				}
				pending.cancel()
				pending.abandoned = true
				if !pending.dispatched || pending.release == nil && !pending.authorizing {
					delete(b.pending, id)
				}
			}
		}
		b.mu.Unlock()
		if !isCurrent {
			disconnectReason = "stream_replaced"
		} else if b.ctx.Err() != nil {
			disconnectReason = "host_stopped"
		}
		slog.Info("browser extension disconnected", "browser_id", peer.InstanceID, "reason", disconnectReason)
	}()
	if err := sendConnection(socket, connectionMessage{Type: "ready", BridgeID: b.prefix}); err != nil {
		return
	}
	readerDone := make(chan struct{})
	readerReason := "stream_closed"
	go func() {
		defer close(readerDone)
		for {
			_ = socket.SetReadDeadline(time.Now().Add(35 * time.Second))
			var message connectionMessage
			if err := websocket.JSON.Receive(socket, &message); err != nil {
				if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
					readerReason = "keepalive_timeout"
				}
				_ = socket.Close()
				return
			}
			if message.Type != "keepalive" {
				readerReason = "invalid_keepalive"
				_ = socket.Close()
				return
			}
			b.mu.Lock()
			if b.instances[peer.InstanceID] != current || current.socket != socket {
				b.mu.Unlock()
				_ = socket.Close()
				return
			}
			current.lastSeen = time.Now()
			b.mu.Unlock()
		}
	}()
	defer func() {
		_ = socket.Close()
		<-readerDone
		if disconnectReason == "stream_closed" {
			disconnectReason = readerReason
		}
	}()
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()
	for {
		var item command
		select {
		case item = <-current.controls:
		default:
			select {
			case item = <-current.controls:
			case item = <-current.commands:
			case <-ticker.C:
				if sendConnection(socket, connectionMessage{Type: "keepalive"}) != nil {
					disconnectReason = "write_failed"
					return
				}
				continue
			case <-readerDone:
				return
			case <-b.ctx.Done():
				return
			}
		}
		b.mu.Lock()
		pending := b.pending[item.ID]
		valid := b.instances[peer.InstanceID] == current && pending != nil && pending.bootID == peer.BootID && (item.Method == "cancel" || !pending.abandoned && pending.ctx.Err() == nil && time.Now().UnixMilli() < item.Deadline)
		if valid && item.Method != "cancel" {
			pending.dispatched = true
		}
		if !valid && pending != nil && !pending.dispatched {
			pending.cancel()
			delete(b.pending, item.ID)
		}
		b.mu.Unlock()
		if valid {
			data, err := json.Marshal(item)
			if err != nil || len(data) > 32<<20 {
				disconnectReason = "command_too_large"
				return
			}
			if sendConnection(socket, item) != nil {
				disconnectReason = "write_failed"
				return
			}
		}
	}
}

func sendConnection(socket *websocket.Conn, message any) error {
	_ = socket.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return websocket.JSON.Send(socket, message)
}
