package netproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"":                             "",
		"192.168.31.11:7890":           "http://192.168.31.11:7890",
		" HTTP://Proxy.Example:8080/ ": "http://proxy.example:8080",
		"https://proxy.example:8443":   "https://proxy.example:8443",
	}
	for input, want := range cases {
		got, err := Normalize(input)
		if err != nil || got != want {
			t.Fatalf("Normalize(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{
		"socks5://proxy.example:1080",
		"http://user:pass@proxy.example:8080",
		"http://proxy.example:8080/path",
		"http://proxy.example:8080/?x=1",
		"http://proxy.example:0",
		"http://proxy.example:70000",
	} {
		if _, err := Normalize(input); err == nil {
			t.Fatalf("Normalize(%q) unexpectedly succeeded", input)
		}
	}
}

func TestInstalledTransportUsesProxyAndBypassesLoopback(t *testing.T) {
	Install()
	if _, err := Configure(""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = Configure("") })

	var proxied, direct atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		if r.URL.Host != "tracker.example.invalid" || r.URL.Path != "/probe" {
			t.Errorf("proxy received %s", r.URL.String())
		}
		io.WriteString(w, "through proxy")
	}))
	defer proxy.Close()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		direct.Add(1)
		io.WriteString(w, "local")
	}))
	defer local.Close()

	if _, err := Configure(proxy.URL); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: http.DefaultTransport, Timeout: 5 * time.Second}
	resp, err := client.Get("http://tracker.example.invalid/probe")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "through proxy" || proxied.Load() != 1 {
		t.Fatalf("body=%q proxied=%d", body, proxied.Load())
	}
	resp, err = client.Get(local.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if direct.Load() != 1 || proxied.Load() != 1 {
		t.Fatalf("loopback bypass failed direct=%d proxied=%d", direct.Load(), proxied.Load())
	}
}

func TestEmptyConfigurationDisablesEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://inherited.example:8080")
	Install()
	if _, err := Configure(""); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, "https://tracker.example.invalid/", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := dynamicProxy(req)
	if err != nil || proxy != nil {
		t.Fatalf("empty configuration used inherited proxy: %v %v", proxy, err)
	}
	env := ChildEnvironment([]string{
		"HTTPS_PROXY=http://inherited.example:8080",
		"ALL_PROXY=socks5://inherited.example:1080",
		"PROXY_URL=http://inherited.example:8080",
		"PROXY_USERNAME=old-user",
		"KEEP=yes",
	})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "inherited.example") || !strings.Contains(joined, "KEEP=yes") {
		t.Fatal(joined)
	}
}

func TestProbeAndChildEnvironment(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.String() != "http://probe.example.invalid/meta" {
			t.Errorf("probe used %s", r.URL.String())
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer proxy.Close()
	previousProbeURL := probeURLForTest
	probeURLForTest = "http://probe.example.invalid/meta"
	t.Cleanup(func() { probeURLForTest = previousProbeURL })
	result := Probe(context.Background(), proxy.URL)
	if !result.OK || result.Status != http.StatusOK {
		t.Fatalf("%+v", result)
	}
	if _, err := Configure(proxy.URL); err != nil {
		t.Fatal(err)
	}
	defer Configure("") //nolint:errcheck
	env := ChildEnvironment([]string{"PATH=C:/bin", "HTTP_PROXY=http://old:1", "KEEP=yes"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "old:1") || !strings.Contains(joined, "HTTPS_PROXY="+proxy.URL) || !strings.Contains(joined, "KEEP=yes") {
		t.Fatal(joined)
	}
}
