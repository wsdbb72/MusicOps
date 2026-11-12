package app

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The CSS and JS are embedded in the binary, so they only change when the
// version changes. They were served with the blanket "no-store" and no
// validator, which forced a full re-download on every navigation (P2-1).
func TestStaticAssetsAreCacheableAndRevalidate(t *testing.T) {
	application := newApp(t)
	first := getAuth(t, application, "/app.css")
	if first.Code != http.StatusOK {
		t.Fatal(first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	if cache := first.Header().Get("Cache-Control"); strings.Contains(cache, "no-store") {
		t.Fatal(cache)
	}

	request := httptest.NewRequest(http.MethodGet, "/app.css", nil)
	request.SetBasicAuth("musicops", "test-password-16b")
	request.Header.Set("If-None-Match", etag)
	recorder := httptest.NewRecorder()
	application.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotModified {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	if recorder.Body.Len() != 0 {
		t.Fatal("304 carried a body")
	}

	// A different validator must not be answered with 304.
	request = httptest.NewRequest(http.MethodGet, "/app.js", nil)
	request.SetBasicAuth("musicops", "test-password-16b")
	request.Header.Set("If-None-Match", `"stale"`)
	recorder = httptest.NewRecorder()
	application.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.Len() == 0 {
		t.Fatal(recorder.Code)
	}
}

func TestStaticAssetsAreCompressedWhenAccepted(t *testing.T) {
	application := newApp(t)
	request := httptest.NewRequest(http.MethodGet, "/app.js", nil)
	request.SetBasicAuth("musicops", "test-password-16b")
	request.Header.Set("Accept-Encoding", "gzip")
	recorder := httptest.NewRecorder()
	application.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code)
	}
	if encoding := recorder.Header().Get("Content-Encoding"); encoding != "gzip" {
		t.Fatal(encoding)
	}
	if vary := recorder.Header().Get("Vary"); !strings.Contains(vary, "Accept-Encoding") {
		t.Fatal(vary)
	}
	reader, err := gzip.NewReader(recorder.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != len(appJS) || string(decoded) != appJS {
		t.Fatal("gzip body does not decode to the asset")
	}

	// A client that does not accept gzip keeps the plain body.
	plain := getAuth(t, application, "/app.js")
	if plain.Header().Get("Content-Encoding") != "" || plain.Body.String() != appJS {
		t.Fatal(plain.Header().Get("Content-Encoding"))
	}
}
