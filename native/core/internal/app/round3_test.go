package app

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestMetadataWritesTagsLyricsAndCoverTogether(t *testing.T) {
	application := newApp(t)
	library := filepath.Join(t.TempDir(), "music")
	if err := os.MkdirAll(library, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(library, "01 opening.flac")
	original := syntheticFLAC(t, false)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	catalog := newFakeMusicBrainz(t, catalogSearchJSON, catalogDetailJSON, http.StatusOK)
	lyrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"lyrics":"[00:01.00]hello"}`)
	}))
	defer lyrics.Close()
	coverBody := append([]byte{0xff, 0xd8}, bytes.Repeat([]byte{0x11}, 64)...)
	cover := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(coverBody)
	}))
	defer cover.Close()
	failedCover := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusBadGateway)
	}))
	defer failedCover.Close()
	missed := postCode(t, application, "/v1/library/metadata", map[string]any{
		"path": path, "artist": "Some Artist", "album": "First Album", "title": "Opening",
		"catalog_base_url": catalog.server.URL, "lyrics_base_url": lyrics.URL, "cover_base_url": failedCover.URL,
	})
	if missed.Code != http.StatusBadGateway {
		t.Fatal(missed.Code, missed.Body.String())
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("failed cover changed the audio", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(path, ".flac") + ".jpg"); !os.IsNotExist(err) {
		t.Fatal("cover sidecar left behind", err)
	}
	written := post(t, application, "/v1/library/metadata", map[string]any{
		"path": path, "artist": "Some Artist", "album": "First Album", "title": "Opening",
		"catalog_base_url": catalog.server.URL, "lyrics_base_url": lyrics.URL, "cover_base_url": cover.URL,
	})
	if written["written"] != true || written["lyrics"] != true || written["cover"] != true {
		t.Fatal(written)
	}
	after, err := os.ReadFile(path)
	if err != nil || bytes.Equal(after, original) || !bytes.Contains(after, []byte("[00:01.00]hello")) {
		t.Fatal("combined write did not keep the lyrics", err)
	}
	sidecar, err := os.ReadFile(strings.TrimSuffix(path, ".flac") + ".jpg")
	if err != nil || !bytes.Equal(sidecar, coverBody) {
		t.Fatal(err, sidecar)
	}
}

func TestWorkflowUsesTheSelectedFileWithoutSpending(t *testing.T) {
	application := newApp(t)
	library := filepath.Join(t.TempDir(), "music")
	if err := os.MkdirAll(library, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(library, "01 opening.flac")
	if err := os.WriteFile(path, []byte("local-audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	missing := postCode(t, application, "/v1/library/workflow", map[string]any{"scene": "upgrade", "path": filepath.Join(library, "missing.flac")})
	if missing.Code != http.StatusBadRequest {
		t.Fatal(missing.Code, missing.Body.String())
	}
	started := post(t, application, "/v1/library/workflow", map[string]any{"scene": "fill", "label": "补缺", "path": path})
	note, _ := started["note"].(string)
	gotPath, _ := started["path"].(string)
	if started["downloaded"] != false || !strings.Contains(note, "令牌规则") || !strings.Contains(note, "尚未下载") || filepath.Base(gotPath) != "01 opening.flac" {
		t.Fatal(started)
	}
}

func TestAliasCanUseAnAliasAsSearchText(t *testing.T) {
	application := newApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"aliases":["Other Name"],"artist":"Canonical"}`)
	}))
	defer server.Close()
	post(t, application, "/v1/settings/ai", map[string]any{"enabled": true, "base_url": server.URL, "model": "alias"})
	query, ok := application.researchAlias(context.Background(), workflow.Job{}, SearchPlan{Query: "old query", ExpectedTitle: "Album", ExpectedArtist: "Artist"}, searchdecision.Decision{Action: searchdecision.ActionReviewSearch})
	if !ok || query != "Other Name" {
		t.Fatal(query, ok)
	}
}

func TestDiscogsReleaseDetailDoesNotConfirmIdentity(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	post(t, application, "/v1/settings/discogs", map[string]any{"token": "discogs-token-123"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Discogs token=discogs-token-123" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/releases/") {
			_, _ = io.WriteString(w, `{"tracklist":[{"title":"Opening"},{"title":"Second"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[{"title":"Some Artist - First Album","id":42}]}`)
	}))
	defer server.Close()
	hits, _, ok := application.discogsSearch(server.URL, "Some Artist", "First Album")
	if !ok || len(hits) != 1 || hits[0].IdentityConfirmed || hits[0].TrackCount != 2 {
		t.Fatal(hits, ok)
	}
}

func TestRoonInfoProbeDoesNotRegister(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 2048)
		n, _ := conn.Read(buf)
		key := headerValue(string(buf[:n]), "Sec-WebSocket-Key")
		sum := sha1.Sum([]byte(key + roonWebsocketGUID))
		accept := base64.StdEncoding.EncodeToString(sum[:])
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+accept+"\r\n\r\n")
		n, _ = conn.Read(buf)
		if bytes.Contains(buf[:n], []byte("register")) && !bytes.Contains(buf[:n], []byte("registry")) {
			return
		}
		payload := []byte("{\"core_id\":\"c1\",\"display_name\":\"Living Room\",\"display_version\":\"2.0\"}")
		frame := append([]byte{0x81, byte(len(payload))}, payload...)
		_, _ = conn.Write(frame)
	}()
	application := newApp(t)
	_, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	body := post(t, application, "/v1/settings/servers/roon/test", map[string]any{"host": "127.0.0.1", "port": port})
	if body["reachable"] != true || body["registered"] != false || body["core_name"] != "Living Room" {
		encoded, _ := json.Marshal(body)
		t.Fatal(string(encoded))
	}
}

func headerValue(raw, name string) string {
	for _, line := range strings.Split(raw, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), strings.ToLower(name)+":") {
			return strings.TrimSpace(line[len(name)+1:])
		}
	}
	return ""
}
