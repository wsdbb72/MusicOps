package main

import "testing"

func TestParseVersionOutput(t *testing.T) {
	v, err := parseVersionOutput([]byte("{\"version\": \"0.4.33\"}\n"))
	if err != nil || v != "0.4.33" {
		t.Fatalf("parseVersionOutput = %q, %v; want 0.4.33, nil", v, err)
	}
	if _, err := parseVersionOutput([]byte("not json")); err == nil {
		t.Fatal("parseVersionOutput accepted non-JSON")
	}
	if _, err := parseVersionOutput([]byte("{\"version\": \"\"}")); err == nil {
		t.Fatal("parseVersionOutput accepted an empty version")
	}
	if _, err := parseVersionOutput([]byte("{}")); err == nil {
		t.Fatal("parseVersionOutput accepted a missing version")
	}
}

func TestParseInspect(t *testing.T) {
	raw := []byte(`[{"State":{"Running":true,"ExitCode":0,"Health":{"Status":"healthy"}},` +
		`"NetworkSettings":{"Ports":{"8080/tcp":[{"HostPort":"32768"}]}}}]`)
	info, err := parseInspect(raw)
	if err != nil {
		t.Fatalf("parseInspect: %v", err)
	}
	if !info.Running || info.ExitCode != 0 || info.Healthy == nil || !*info.Healthy || info.HostPort != "32768" {
		t.Fatalf("parseInspect = %+v, want running healthy port 32768", info)
	}

	// No Health block: Healthy stays nil and the caller falls back to
	// probing `musicops healthcheck` directly.
	raw = []byte(`[{"State":{"Running":true,"ExitCode":0},` +
		`"NetworkSettings":{"Ports":{"8080/tcp":[]}}}]`)
	info, err = parseInspect(raw)
	if err != nil {
		t.Fatalf("parseInspect without health: %v", err)
	}
	if info.Healthy != nil || info.HostPort != "" {
		t.Fatalf("parseInspect without health = %+v, want nil health and no port", info)
	}

	// Exited container reports its exit code.
	raw = []byte(`[{"State":{"Running":false,"ExitCode":1,"Health":{"Status":"unhealthy"}},` +
		`"NetworkSettings":{"Ports":{}}}]`)
	info, err = parseInspect(raw)
	if err != nil {
		t.Fatalf("parseInspect exited: %v", err)
	}
	if info.Running || info.ExitCode != 1 || info.Healthy == nil || *info.Healthy {
		t.Fatalf("parseInspect exited = %+v, want not running, exit 1, unhealthy", info)
	}

	if _, err := parseInspect([]byte("[]")); err == nil {
		t.Fatal("parseInspect accepted an empty result")
	}
	if _, err := parseInspect([]byte("nope")); err == nil {
		t.Fatal("parseInspect accepted non-JSON")
	}
}

func TestHealthzOK(t *testing.T) {
	good := []byte(`{"ok":true,"application":"musicops","python":false}`)
	if !healthzOK(good) {
		t.Fatal("healthzOK rejected a good body")
	}
	for _, bad := range [][]byte{
		[]byte(`{"ok":false,"application":"musicops","python":false}`),
		[]byte(`{"ok":true,"application":"other","python":false}`),
		[]byte(`{"ok":true,"application":"musicops","python":true}`),
		[]byte(`{"ok":true,"application":"musicops"}`),
		[]byte(`not json`),
	} {
		if healthzOK(bad) {
			t.Fatalf("healthzOK accepted %s", bad)
		}
	}
}

func TestRedact(t *testing.T) {
	if got := redact("pw secret-here end", "secret-here"); got != "pw [MASKED] end" {
		t.Fatalf("redact = %q", got)
	}
	if got := redact("nothing to hide", ""); got != "nothing to hide" {
		t.Fatalf("redact with empty secret = %q", got)
	}
	if got := redact("no secret here", "absent"); got != "no secret here" {
		t.Fatalf("redact with absent secret = %q", got)
	}
}

func TestModeIs600(t *testing.T) {
	if !modeIs600("600") || !modeIs600("600\n") {
		t.Fatal("modeIs600 rejected 600")
	}
	for _, mode := range []string{"644", "0600", "400", ""} {
		if modeIs600(mode) {
			t.Fatalf("modeIs600 accepted %q", mode)
		}
	}
}

func TestAuthHeader(t *testing.T) {
	if got := authHeader("musicops", "secret"); got != "Basic bXVzaWNvcHM6c2VjcmV0" {
		t.Fatalf("authHeader = %q", got)
	}
}

func TestContainerNameFor(t *testing.T) {
	if got := containerNameFor(123); got != "musicops-docker-smoke-123" {
		t.Fatalf("containerNameFor = %q", got)
	}
}
