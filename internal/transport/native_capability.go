package transport

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

// NewNativeCapabilityHTTP uses a dedicated listener with no machine, model or
// tunnel route. Its client is a local process, not a web page or reverse proxy.
func NewNativeCapabilityHTTP(address, key string, handler http.Handler) (*HTTPServer, error) {
	if err := validateLoopbackAddress(address); err != nil {
		return nil, err
	}
	if len(key) < 32 || handler == nil {
		return nil, errors.New("native capabilities require their own authenticated handler")
	}
	server, err := newLoopbackHTTP(address, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		remoteHost, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(remoteHost)
		_, hasOrigin := r.Header["Origin"]
		if ip == nil || !ip.IsLoopback() || !allowedHost(r.Host, "") || hasOrigin || forwardedNativeRequest(r.Header) {
			writeNativeAuthError(w, http.StatusForbidden, "local_only", "This endpoint is only available to local processes.")
			return
		}
		provided, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !bearer || subtle.ConstantTimeCompare([]byte(provided), []byte(key)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="local-runtime-capabilities"`)
			writeNativeAuthError(w, http.StatusUnauthorized, "unauthorized", "A native capability key is required.")
			return
		}
		handler.ServeHTTP(w, r)
	}))
	if err != nil {
		return nil, err
	}
	server.server.ReadTimeout = 10 * time.Second
	return server, nil
}

func forwardedNativeRequest(headers http.Header) bool {
	return headers.Get("Forwarded") != "" || headers.Get("X-Forwarded-For") != "" || headers.Get("X-Forwarded-Host") != "" || headers.Get("X-Forwarded-Proto") != "" || headers.Get("CF-Connecting-IP") != ""
}

func writeNativeAuthError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}
