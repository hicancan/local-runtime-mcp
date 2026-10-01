package transport

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const DefaultHTTPListen = "127.0.0.1:9316"

// InternalTokenHeader authenticates the provider-to-host loopback hop separately
// from the MCP client's forwarded Authorization header.
const InternalTokenHeader = "X-Local-Runtime-MCP-Internal-Token"

type HTTPConfig struct {
	Listen        string
	BearerToken   string
	PublicHost    string
	InternalToken bool
}

type HTTPServer struct {
	server   *http.Server
	listener net.Listener
}

func NewHTTP(server *mcp.Server, cfg HTTPConfig) (*HTTPServer, error) {
	if cfg.Listen == "" {
		cfg.Listen = DefaultHTTPListen
	}
	if err := validateLoopbackAddress(cfg.Listen); err != nil {
		return nil, err
	}
	if len(cfg.BearerToken) < 32 {
		return nil, errors.New("HTTP bearer token must contain at least 32 characters")
	}
	publicHost, err := normalizeHost(cfg.PublicHost)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen for Streamable HTTP: %w", err)
	}

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		DisableLocalhostProtection:   true,
		PropagateRequestCancellation: true,
		MaxRequestBodyBytes:          32 << 20,
	})
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/mcp" {
			http.NotFound(writer, request)
			return
		}
		if !allowedHost(request.Host, publicHost) {
			http.Error(writer, "forbidden host", http.StatusForbidden)
			return
		}
		if !cfg.InternalToken && request.Header.Get("Origin") != "" {
			http.Error(writer, "browser-origin requests are not allowed", http.StatusForbidden)
			return
		}
		provided, bearer := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
		if cfg.InternalToken {
			provided, bearer = request.Header.Get(InternalTokenHeader), true
		}
		if !bearer || len(provided) != len(cfg.BearerToken) || subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.BearerToken)) != 1 {
			if !cfg.InternalToken {
				writer.Header().Set("WWW-Authenticate", `Bearer realm="local-runtime-mcp"`)
			}
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(writer, request)
	})
	return &HTTPServer{
		server: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
		},
		listener: listener,
	}, nil
}

func (s *HTTPServer) Address() string { return s.listener.Addr().String() }

func (s *HTTPServer) Run(ctx context.Context) error {
	errorsChannel := make(chan error, 1)
	go func() { errorsChannel <- s.server.Serve(s.listener) }()
	select {
	case err := <-errorsChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// This server is stateless: terminal shutdown should take the machine
		// offline immediately instead of waiting on a client's open HTTP body.
		err := s.server.Close()
		serveErr := <-errorsChannel
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(err, serveErr)
	}
}

func validateLoopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("HTTP listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return errors.New("HTTP listen address must use a loopback host")
	}
	return nil
}

func normalizeHost(value string) (string, error) {
	value = strings.TrimSpace(strings.TrimSuffix(value, "."))
	if value == "" {
		return "", nil
	}
	if strings.ContainsAny(value, "/:@") {
		return "", errors.New("public host must be a hostname without scheme, port, path, or credentials")
	}
	return strings.ToLower(value), nil
}

func allowedHost(requestHost, publicHost string) bool {
	host := requestHost
	if parsed, _, err := net.SplitHostPort(requestHost); err == nil {
		host = parsed
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return publicHost != "" && host == publicHost
}
