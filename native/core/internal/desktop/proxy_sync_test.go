//go:build windows || darwin

package desktop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/netproxy"
)

func TestSyncDesktopProxyUsesCoreSetting(t *testing.T) {
	t.Cleanup(func() { _, _ = netproxy.Configure("") })
	var authenticated atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		authenticated.Store(ok && user == "musicops" && password == "desktop-test-password")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"proxy":{"url":"http://192.168.31.11:7890"}}`))
	}))
	defer server.Close()
	handle := &CoreHandle{URL: server.URL, Password: "desktop-test-password"}
	if err := syncDesktopProxy(context.Background(), handle); err != nil {
		t.Fatal(err)
	}
	if !authenticated.Load() {
		t.Fatal("desktop did not authenticate to the local core")
	}
	if got, enabled := netproxy.Current(); !enabled || got != "http://192.168.31.11:7890" {
		t.Fatalf("proxy = %q, %v", got, enabled)
	}
}
