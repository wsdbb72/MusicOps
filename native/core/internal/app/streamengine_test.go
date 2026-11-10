package app

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
)

func oneSecondFLAC(t *testing.T, artist, title string) []byte {
	t.Helper()
	info := make([]byte, 34)
	binary.BigEndian.PutUint16(info[0:2], 16)
	binary.BigEndian.PutUint16(info[2:4], 16)
	packed := uint64(44100)<<44 | uint64(1)<<41 | uint64(15)<<36 | 44100
	binary.BigEndian.PutUint64(info[10:18], packed)
	raw := syntheticFLAC(t, false)
	copy(raw[8:42], info)
	raw = append(raw, make([]byte, 1200)...)
	out, err := flactags.WriteBytes(raw, flactags.Tags{Artist: artist, Title: title, Album: "First Album"})
	if err != nil {
		t.Fatal(err)
	}
	if err := validStreamAudio(out, artist, title, 1); err != nil {
		t.Fatal(err, len(out))
	}
	return out
}

func TestStreamEngineAcceptsOnlyValidatedAudio(t *testing.T) {
	audio := oneSecondFLAC(t, "Artist", "Example & Test")
	var tidalCalls, appleCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			if r.URL.Query().Get("sources") == "tidal" {
				tidalCalls++
				_, _ = w.Write([]byte(`{"songs":[{"id":"t1","source":"tidal","name":"Example & Test","artist":"Artist","album":"First Album","duration":1}]}`))
				return
			}
			http.NotFound(w, r)
		case "/download":
			if r.URL.Query().Get("source") != "tidal" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(audio)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	tracker := emptyResultsServer(t)
	defer tracker.Close()
	application := newApp(t)
	post(t, application, "/v1/settings/download", map[string]any{"streaming_enabled": true, "engine_url": server.URL})
	post(t, application, "/v1/search", searchBody("album-engine", tracker.URL, []string{"dicmusic", "orpheus"}, true))
	job := waitJob(t, application, "process:search:album-engine", "needs_review")
	if !strings.Contains(job.Message, "tidal 音频已通过校验并保存") || !strings.Contains(job.Message, "算作下载") || !strings.Contains(job.Message, "尚未入库") || strings.Contains(job.Message, "已入库") {
		t.Fatal(job.Message)
	}
	saved := filepath.Join(application.Dir, "incoming", "stream-album-engine-tidal.flac")
	if _, err := os.Stat(saved); err != nil || tidalCalls == 0 || appleCalls != 0 {
		t.Fatal(err, tidalCalls, appleCalls)
	}
	rejected := postCode(t, application, "/v1/settings/download", map[string]any{"streaming_enabled": true, "engine_url": "http://example.com"})
	if rejected.Code != http.StatusBadRequest {
		t.Fatal(rejected.Code, rejected.Body.String())
	}
	metadata := postCode(t, application, "/v1/settings/download", map[string]any{"streaming_enabled": true, "engine_url": "https://169.254.169.254/"})
	if metadata.Code != http.StatusBadRequest {
		t.Fatal(metadata.Code, metadata.Body.String())
	}
}

func TestStreamEngineSkipsAudioThatFailsValidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search" && r.URL.Query().Get("sources") == "tidal" {
			_, _ = w.Write([]byte(`{"songs":[{"id":"bad","source":"tidal","name":"Example & Test","artist":"Artist","duration":1}]}`))
			return
		}
		if r.URL.Path == "/download" {
			_, _ = w.Write(bytesRepeat(2000, 'x'))
			return
		}
		_, _ = w.Write([]byte(`{"songs":[]}`))
	}))
	defer server.Close()
	tracker := emptyResultsServer(t)
	defer tracker.Close()
	application := newApp(t)
	post(t, application, "/v1/settings/download", map[string]any{"streaming_enabled": true, "engine_url": server.URL})
	post(t, application, "/v1/search", searchBody("album-bad-audio", tracker.URL, []string{"dicmusic", "orpheus"}, true))
	job := waitJob(t, application, "process:search:album-bad-audio", "needs_review")
	if !strings.Contains(job.Message, "没有通过音频校验") || !strings.Contains(job.Message, "尚未下载") {
		t.Fatal(job.Message)
	}
	if matches, _ := filepath.Glob(filepath.Join(application.Dir, "incoming", "stream-*.flac")); len(matches) != 0 {
		t.Fatal(matches)
	}
}

func TestStreamEngineContinuesAfterTidalMisses(t *testing.T) {
	audio := oneSecondFLAC(t, "Artist", "Example & Test")
	var sources []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download" {
			if r.URL.Query().Get("source") != "apple" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(audio)
			return
		}
		sources = append(sources, r.URL.Query().Get("sources"))
		if r.URL.Query().Get("sources") == "apple" {
			_, _ = w.Write([]byte(`{"songs":[{"id":"a1","source":"apple","name":"Example & Test","artist":"Artist","album":"First Album","duration":1}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"songs":[]}`))
	}))
	defer server.Close()
	tracker := emptyResultsServer(t)
	defer tracker.Close()
	application := newApp(t)
	post(t, application, "/v1/settings/download", map[string]any{"streaming_enabled": true, "engine_url": server.URL})
	post(t, application, "/v1/search", searchBody("album-apple", tracker.URL, []string{"dicmusic", "orpheus"}, true))
	job := waitJob(t, application, "process:search:album-apple", "needs_review")
	if !strings.Contains(job.Message, "applemusic 音频已通过校验并保存") || strings.Contains(job.Message, "tidal 音频已通过校验") {
		t.Fatal(job.Message, sources)
	}
}

func bytesRepeat(n int, b byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
