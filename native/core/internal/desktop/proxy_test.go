package desktop

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyInjectsAuthAndPreservesResponse(t *testing.T) {
	var seenAuth, seenProxyAuth, seenConnection string
	var seenBody string
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenProxyAuth = r.Header.Get("Proxy-Authorization")
		seenConnection = r.Header.Get("Connection")
		body, _ := io.ReadAll(r.Body)
		seenBody = string(body)
		w.Header().Set("X-Custom", "kept")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello core"))
	}))
	defer core.Close()

	proxy := NewProxy(core.URL, "test-password-123456")
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs?page=2", strings.NewReader(`{"x":1}`))
	req.Header.Set("Authorization", "Basic decoy")
	req.Header.Set("Proxy-Authorization", "Basic decoy")
	req.Header.Set("Connection", "close")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if rec.Body.String() != "hello core" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("X-Custom") != "kept" {
		t.Fatalf("custom response header lost")
	}
	if rec.Header().Get("Connection") != "" {
		t.Fatalf("hop-by-hop Connection header leaked to the client: %q", rec.Header().Get("Connection"))
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("musicops:test-password-123456"))
	if seenAuth != wantAuth {
		t.Fatalf("core saw Authorization %q, want %q", seenAuth, wantAuth)
	}
	if seenProxyAuth != "" {
		t.Fatalf("Proxy-Authorization leaked to the core: %q", seenProxyAuth)
	}
	if seenConnection == "close" {
		t.Fatalf("hop-by-hop Connection header leaked to the core")
	}
	if seenBody != `{"x":1}` {
		t.Fatalf("core saw body %q", seenBody)
	}
}

func TestProxyCoreUnavailableIs502WithoutLeaks(t *testing.T) {
	// Port 1 is unroutable for TCP connects from an unprivileged process.
	proxy := NewProxy("http://127.0.0.1:1", "test-password-123456")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body struct {
		OK   bool   `json:"ok"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body.OK || body.Code != "CORE_UNAVAILABLE" {
		t.Fatalf("body = %+v", body)
	}
	if strings.Contains(rec.Body.String(), "test-password-123456") || strings.Contains(rec.Body.String(), "127.0.0.1") {
		t.Fatalf("502 body leaks credentials or the core address: %q", rec.Body.String())
	}
}

func TestProxyInvalidCoreURLIs502(t *testing.T) {
	proxy := NewProxy("://bad url", "test-password-123456")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestRemoveHopHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Connection", "X-Custom-Hop, Keep-Alive")
	h.Set("X-Custom-Hop", "1")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("Proxy-Authorization", "Basic x")
	h.Set("Te", "trailers")
	h.Set("X-Keep", "yes")
	removeHopHeaders(h)
	for _, name := range []string{"Connection", "X-Custom-Hop", "Keep-Alive", "Proxy-Authorization", "Te"} {
		if h.Get(name) != "" {
			t.Fatalf("header %s survived stripping", name)
		}
	}
	if h.Get("X-Keep") != "yes" {
		t.Fatalf("ordinary header was stripped")
	}
}
