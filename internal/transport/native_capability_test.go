package transport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeCapabilityHTTPRejectsBrowserProxyAndWrongKey(t *testing.T) {
	key := strings.Repeat("n", 32)
	admitted := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		admitted++
		w.WriteHeader(http.StatusOK)
	})
	if _, err := NewNativeCapabilityHTTP("0.0.0.0:0", key, handler); err == nil {
		t.Fatal("a non-loopback address was accepted")
	}
	server, err := NewNativeCapabilityHTTP("127.0.0.1:0", key, handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.listener.Close() })
	for _, test := range []struct {
		name, host, remote, bearer string
		header, value              string
		want                       int
	}{
		{"loopback", "127.0.0.1", "127.0.0.1:4567", key, "", "", http.StatusOK},
		{"missing key", "localhost", "127.0.0.1:4567", "", "", "", http.StatusUnauthorized},
		{"model key", "localhost", "127.0.0.1:4567", strings.Repeat("m", 32), "", "", http.StatusUnauthorized},
		{"remote caller", "127.0.0.1", "192.0.2.1:4567", key, "", "", http.StatusForbidden},
		{"public host", "public.example", "127.0.0.1:4567", key, "", "", http.StatusForbidden},
		{"browser", "127.0.0.1", "127.0.0.1:4567", key, "Origin", "https://studev.example", http.StatusForbidden},
		{"empty browser origin", "127.0.0.1", "127.0.0.1:4567", key, "Origin", "", http.StatusForbidden},
		{"reverse proxy", "127.0.0.1", "127.0.0.1:4567", key, "Forwarded", "for=192.0.2.1", http.StatusForbidden},
		{"cloudflare", "127.0.0.1", "127.0.0.1:4567", key, "CF-Connecting-IP", "192.0.2.1", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/capabilities", nil)
			r.Host, r.RemoteAddr = test.host, test.remote
			if test.bearer != "" {
				r.Header.Set("Authorization", "Bearer "+test.bearer)
			}
			if test.header != "" {
				r.Header.Set(test.header, test.value)
			}
			w := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(w, r)
			if w.Code != test.want || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), key) {
				t.Fatalf("unexpected authenticated response %d %s", w.Code, w.Body)
			}
		})
	}
	if admitted != 1 {
		t.Fatalf("unauthorized caller reached capability handler: %d invocations", admitted)
	}
}
