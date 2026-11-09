package musicservers

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const plexSectionsFixture = `<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer size="5">
  <Directory key="1" type="artist" title="Music"><Location path="/music"/><Location path="/music2"/></Directory>
  <Directory key="12" type="artist" title="Lossless"><Location path="/flac"/></Directory>
  <Directory key="3" type="movie" title="Movies"><Location path="/movies"/></Directory>
  <Directory key="abc" type="artist" title="Bad Key"><Location path="/bad"/></Directory>
  <Directory key="1234567890123" type="artist" title="Too Long Key"><Location path="/long"/></Directory>
</MediaContainer>`

func TestPlexSectionsParsesArtistSections(t *testing.T) {
	var gotToken, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/library/sections" {
			t.Errorf("path = %q, want /library/sections", r.URL.Path)
		}
		gotToken = r.Header.Get("X-Plex-Token")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(plexSectionsFixture))
	}))
	defer server.Close()

	sections, err := PlexSections(context.Background(), server.URL, "secret-token")
	if err != nil {
		t.Fatalf("PlexSections: %v", err)
	}
	if gotToken != "secret-token" {
		t.Errorf("X-Plex-Token = %q, want secret-token", gotToken)
	}
	if gotAccept != "application/xml" {
		t.Errorf("Accept = %q, want application/xml", gotAccept)
	}
	want := []Section{
		{ID: "1", Name: "Music", Paths: []string{"/music", "/music2"}},
		{ID: "12", Name: "Lossless", Paths: []string{"/flac"}},
	}
	if !reflect.DeepEqual(sections, want) {
		t.Fatalf("sections = %+v, want %+v", sections, want)
	}
}

func TestPlexSectionsStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrAuthFailed},
		{http.StatusForbidden, ErrAuthFailed},
		{http.StatusInternalServerError, ErrAccessBlocked},
		{http.StatusFound, ErrAccessBlocked}, // redirect is not followed
		{http.StatusNoContent, ErrAccessBlocked},
	}
	for _, tc := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		_, err := PlexSections(context.Background(), server.URL, "token")
		server.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want errors.Is %v", tc.status, err, tc.want)
		}
	}
}

func TestPlexSectionsInvalidPayloads(t *testing.T) {
	cases := map[string]string{
		"non MediaContainer root": `<html><body>not plex</body></html>`,
		"malformed XML":           `<MediaContainer><Directory`,
		"empty body":              ``,
		"lowercase doctype":       `<!doctype html><html></html>`,
		"uppercase doctype":       `<!DOCTYPE html><html></html>`,
	}
	for name, body := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		_, err := PlexSections(context.Background(), server.URL, "token")
		server.Close()
		if !errors.Is(err, ErrInvalidPayload) {
			t.Errorf("%s: err = %v, want errors.Is ErrInvalidPayload", name, err)
		}
	}
}

func TestPlexSectionsOversizedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", plexMaxResponseBytes+1)))
	}))
	defer server.Close()
	_, err := PlexSections(context.Background(), server.URL, "token")
	if !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("err = %v, want errors.Is ErrInvalidPayload", err)
	}
}

func TestPlexSectionsEmptyToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request must not be sent without a token")
	}))
	defer server.Close()
	_, err := PlexSections(context.Background(), server.URL, "")
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("err = %v, want errors.Is ErrAuthRequired", err)
	}
}

func TestPlexSectionsNetworkError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	server.Close() // closed: connection refused
	_, err := PlexSections(context.Background(), server.URL, "token")
	if !errors.Is(err, ErrNetwork) {
		t.Fatalf("err = %v, want errors.Is ErrNetwork", err)
	}
}

func TestPlexScanSuccess(t *testing.T) {
	var refreshHits int
	var refreshToken string
	mux := http.NewServeMux()
	mux.HandleFunc("/library/sections", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(plexSectionsFixture))
	})
	mux.HandleFunc("/library/sections/1/refresh", func(w http.ResponseWriter, r *http.Request) {
		refreshHits++
		refreshToken = r.Header.Get("X-Plex-Token")
		if r.ContentLength != 0 {
			t.Errorf("refresh ContentLength = %d, want 0 (no body)", r.ContentLength)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	if err := PlexScan(context.Background(), server.URL, "secret-token", "1"); err != nil {
		t.Fatalf("PlexScan: %v", err)
	}
	if refreshHits != 1 {
		t.Fatalf("refresh hits = %d, want 1", refreshHits)
	}
	if refreshToken != "secret-token" {
		t.Errorf("refresh X-Plex-Token = %q, want secret-token", refreshToken)
	}
}

func TestPlexScanUnknownSection(t *testing.T) {
	var refreshHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/library/sections", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(plexSectionsFixture))
	})
	mux.HandleFunc("/library/sections/", func(w http.ResponseWriter, r *http.Request) {
		refreshHits++
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	for _, sectionID := range []string{"99", "3", "abc", ""} {
		err := PlexScan(context.Background(), server.URL, "token", sectionID)
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("section %q: err = %v, want errors.Is ErrInvalidInput", sectionID, err)
		}
	}
	if refreshHits != 0 {
		t.Errorf("refresh hits = %d, want 0 (no refresh for unknown sections)", refreshHits)
	}
}

func TestPlexScanRefreshRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/library/sections", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(plexSectionsFixture))
	})
	mux.HandleFunc("/library/sections/12/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	err := PlexScan(context.Background(), server.URL, "token", "12")
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want errors.Is ErrAuthFailed", err)
	}
}

// navidromeServer verifies the Subsonic auth scheme server-side: the salt
// must be 32 hex characters and t must be the hex MD5 of password+salt.
func navidromeServer(t *testing.T, password string, status string, code int) (*httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		q := r.URL.Query()
		if q.Get("v") != "1.16.1" || q.Get("c") != "MusicOps" || q.Get("f") != "json" {
			t.Errorf("query = %v, want v=1.16.1 c=MusicOps f=json", q)
		}
		salt := q.Get("s")
		if len(salt) != 32 {
			t.Errorf("salt = %q (len %d), want 32 hex chars", salt, len(salt))
		}
		for _, c := range salt {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Errorf("salt = %q, want lowercase hex only", salt)
				break
			}
		}
		sum := md5.Sum([]byte(password + salt))
		if want := hex.EncodeToString(sum[:]); q.Get("t") != want {
			t.Errorf("t = %q, want md5(password+salt) = %q", q.Get("t"), want)
		}
		if q.Get("u") != "alice" {
			t.Errorf("u = %q, want alice", q.Get("u"))
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"subsonic-response": map[string]any{"status": status, "version": "1.16.1"},
		})
	}))
	return server, &paths
}

func TestNavidromePingAndScan(t *testing.T) {
	server, paths := navidromeServer(t, "hunter2", "ok", http.StatusOK)
	defer server.Close()

	if err := NavidromePing(context.Background(), server.URL, "alice", "hunter2"); err != nil {
		t.Fatalf("NavidromePing: %v", err)
	}
	if err := NavidromeScan(context.Background(), server.URL, "alice", "hunter2"); err != nil {
		t.Fatalf("NavidromeScan: %v", err)
	}
	if want := []string{"/rest/ping", "/rest/startScan"}; !reflect.DeepEqual(*paths, want) {
		t.Fatalf("paths = %v, want %v", *paths, want)
	}
}

func TestNavidromeStatusNotOK(t *testing.T) {
	server, _ := navidromeServer(t, "hunter2", "failed", http.StatusOK)
	defer server.Close()
	for name, fn := range map[string]func(context.Context, string, string, string) error{
		"ping": NavidromePing,
		"scan": NavidromeScan,
	} {
		if err := fn(context.Background(), server.URL, "alice", "hunter2"); !errors.Is(err, ErrAccessBlocked) {
			t.Errorf("%s with status=failed: err = %v, want errors.Is ErrAccessBlocked", name, err)
		}
	}
}

func TestNavidromeNon200AndBadJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/ping" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"subsonic-response":{"status":"ok"}}`))
			return
		}
		_, _ = w.Write([]byte(`this is not json`))
	}))
	defer server.Close()

	if err := NavidromePing(context.Background(), server.URL, "alice", "pw"); !errors.Is(err, ErrAccessBlocked) {
		t.Errorf("ping non-200: err = %v, want errors.Is ErrAccessBlocked", err)
	}
	if err := NavidromeScan(context.Background(), server.URL, "alice", "pw"); !errors.Is(err, ErrAccessBlocked) {
		t.Errorf("scan bad JSON: err = %v, want errors.Is ErrAccessBlocked", err)
	}
}

func TestNavidromeNoPassword(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request must not be sent without a password")
	}))
	defer server.Close()
	if err := NavidromePing(context.Background(), server.URL, "alice", ""); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("err = %v, want errors.Is ErrAuthRequired", err)
	}
}

func TestValidatePlexURL(t *testing.T) {
	valid := []string{
		"http://localhost:32400",
		"http://localhost:32400/",
		"http://127.0.0.1:32400",
		"http://[::1]:32400",
		"http://192.168.1.10:32400",
		"http://10.0.0.5",
		"http://172.16.0.9",
		"http://100.64.0.1",
		"http://169.254.1.1",
		"http://plex",
		"http://plex.local",
		"http://host.docker.internal:32400",
		"https://plex.example.com:32400",
		"https://8.8.8.8",
	}
	for _, raw := range valid {
		if err := ValidatePlexURL(raw); err != nil {
			t.Errorf("ValidatePlexURL(%q) = %v, want nil", raw, err)
		}
	}
	invalid := []string{
		"",
		"ftp://localhost:32400",
		"http://example.com:32400",     // public host over HTTP
		"http://8.8.8.8",               // public IP over HTTP
		"http://203.0.113.1",           // public IP over HTTP
		"http://user@localhost:32400",  // userinfo
		"http://:pass@localhost:32400", // userinfo
		"http://localhost:32400/?x=1",  // query
		"http://localhost:32400/#frag", // fragment
		"http://localhost:32400/a b",   // whitespace
		"http:///no-host",
	}
	for _, raw := range invalid {
		if err := ValidatePlexURL(raw); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("ValidatePlexURL(%q) = %v, want errors.Is ErrInvalidInput", raw, err)
		}
	}
}

func TestValidateNavidromeURL(t *testing.T) {
	valid := []string{
		"", // not configured
		"   ",
		"http://localhost:4533",
		"http://localhost:4533/",
		"http://127.0.0.1:4533",
		"http://192.168.1.1:4533",
		"http://navidrome", // single-label Docker/LAN name
		"http://nas.lan:4533",
		"https://navidrome.example.com",
	}
	for _, raw := range valid {
		if err := ValidateNavidromeURL(raw); err != nil {
			t.Errorf("ValidateNavidromeURL(%q) = %v, want nil", raw, err)
		}
	}
	invalid := []string{
		"http://example.com:4533",       // public host over HTTP
		"http://8.8.8.8:4533",           // public IP over HTTP
		"http://192.168.1.1:4533/sub",   // path: not a clean origin
		"http://user@192.168.1.1:4533",  // userinfo
		"http://192.168.1.1:4533/?x=1",  // query
		"http://192.168.1.1:4533/#frag", // fragment
		"ftp://192.168.1.1:4533",        // wrong scheme
	}
	for _, raw := range invalid {
		if err := ValidateNavidromeURL(raw); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("ValidateNavidromeURL(%q) = %v, want errors.Is ErrInvalidInput", raw, err)
		}
	}
}

func TestLocalServiceHost(t *testing.T) {
	local := []string{
		"localhost", "LOCALHOST", "localhost.",
		"host.docker.internal",
		"plex.local", "nas.lan", "box.internal", "box.home.arpa", "app.localhost",
		"127.0.0.1", "127.0.0.2", "::1",
		"10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.0.1",
		"169.254.10.20", "fe80::1", "100.64.0.1", "fd00::1",
		"qbittorrent", "navidrome-1", "music_ops",
	}
	for _, host := range local {
		if !localServiceHost(host) {
			t.Errorf("localServiceHost(%q) = false, want true", host)
		}
	}
	public := []string{
		"", "example.com", "plex.example.com",
		"8.8.8.8", "203.0.113.1", "1.1.1.1", "2001:4860:4860::8888",
		"0.0.0.0", "::", "224.0.0.1", "ff02::1",
		"foo bar", "a.b", "-bad", "bad-",
	}
	for _, host := range public {
		if localServiceHost(host) {
			t.Errorf("localServiceHost(%q) = true, want false", host)
		}
	}
}
