package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/netproxy"
)

func TestOutboundProxyPersistsAndAppliesBeforeRestart(t *testing.T) {
	vaultKey(t)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The probe uses HTTPS through CONNECT. A clean proxy rejection keeps
		// this test deterministic while still proving the setting was saved.
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Cleanup(func() { _, _ = netproxy.Configure("") })
	root := t.TempDir()
	state := filepath.Join(root, "state")
	backup := filepath.Join(root, "backups")

	application, err := Open(state, backup, "test-password-16b")
	if err != nil {
		t.Fatal(err)
	}
	result := post(t, application, "/v1/settings/proxy", map[string]any{"url": proxy.URL})
	if result["ok"] != true {
		t.Fatal(result)
	}
	if got, enabled := netproxy.Current(); !enabled || got != proxy.URL {
		t.Fatalf("runtime proxy = %q, %v", got, enabled)
	}
	if err = application.Close(); err != nil {
		t.Fatal(err)
	}

	_, _ = netproxy.Configure("")
	reopened, err := Open(state, backup, "test-password-16b")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, enabled := netproxy.Current(); !enabled || got != proxy.URL {
		t.Fatalf("restored proxy = %q, %v", got, enabled)
	}
	page := getJSON(t, reopened, "/v1/settings/proxy")
	proxyView, _ := page["proxy"].(map[string]any)
	if proxyView == nil || proxyView["url"] != proxy.URL || proxyView["runtime_enabled"] != true {
		t.Fatal(page)
	}
}

func TestOutboundProxyRejectsUnsafeAddresses(t *testing.T) {
	application := newApp(t)
	t.Cleanup(func() { _, _ = netproxy.Configure("") })
	for _, raw := range []string{
		"socks5://proxy.example:1080",
		"http://user:secret@proxy.example:8080",
		"http://proxy.example:8080/path",
	} {
		recorder := postCode(t, application, "/v1/settings/proxy", map[string]any{"url": raw})
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", raw, recorder.Code, recorder.Body.String())
		}
	}
}

func TestConfiguredOutboundProxyKeepsLocalFixturesDirect(t *testing.T) {
	application := newApp(t)
	t.Cleanup(func() { _, _ = netproxy.Configure("") })
	var calls atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
	}))
	defer fixture.Close()
	saved := post(t, application, "/v1/settings/proxy", map[string]any{"url": "http://127.0.0.1:1"})
	if saved["ok"] != true || saved["reachable"] != false {
		t.Fatal(saved)
	}
	post(t, application, "/v1/search", searchBody("proxy-loopback", fixture.URL, []string{"dicmusic"}, false))
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatal("loopback tracker fixture was incorrectly sent through the configured proxy")
	}
}

func TestProxyChangeRetiresStreamingEngineOnlyAfterActiveRequest(t *testing.T) {
	const base = "http://127.0.0.1:45678/music"
	application := &App{engines: map[engineName]*engineProcess{
		engineStreaming: {base: base, proxy: "http://old.example:8080"},
	}}
	if _, err := netproxy.Configure("http://new.example:8080"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = netproxy.Configure("") })

	finish := application.beginEngineRequest(base)
	application.retireStreamingEngineForProxyChange()
	application.engineMu.Lock()
	entry := application.engines[engineStreaming]
	application.engineMu.Unlock()
	if entry == nil || entry.active != 1 {
		t.Fatalf("active engine was retired too early: %+v", entry)
	}

	finish()
	application.engineMu.Lock()
	_, stillRunning := application.engines[engineStreaming]
	application.engineMu.Unlock()
	if stillRunning {
		t.Fatal("idle engine was not retired after the proxy generation changed")
	}
}
