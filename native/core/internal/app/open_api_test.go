package app

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openGet(t *testing.T, application *App, path string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	for key, values := range header {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	recorder := httptest.NewRecorder()
	application.Handler().ServeHTTP(recorder, request)
	return recorder
}

func basicHeader(password string) http.Header {
	return http.Header{"Authorization": []string{"Basic " + base64.StdEncoding.EncodeToString([]byte("musicops:"+password))}}
}

// Other software authenticates with a key, not the UI password. Basic keeps
// working, and without the env var the key path does not exist at all.
func TestOpenAPIAcceptsAnAPIKey(t *testing.T) {
	application := newApp(t)
	key := "0123456789abcdef-musicops"
	t.Setenv("MUSICOPS_API_KEY", key)

	if recorder := openGet(t, application, "/v1/open/info", nil); recorder.Code != http.StatusUnauthorized {
		t.Fatal("no credentials must be refused:", recorder.Code)
	}
	if recorder := openGet(t, application, "/v1/open/info", http.Header{"X-API-Key": {key}}); recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	if recorder := openGet(t, application, "/v1/open/info", http.Header{"X-API-Key": {"wrong-key-wrong-key-00"}}); recorder.Code != http.StatusUnauthorized {
		t.Fatal("a wrong key must be refused:", recorder.Code)
	}
	if recorder := openGet(t, application, "/v1/open/info", basicHeader("test-password-16b")); recorder.Code != http.StatusOK {
		t.Fatal("Basic must keep working:", recorder.Code)
	}

	// A short key is not honoured: fail closed instead of accepting a weak one.
	t.Setenv("MUSICOPS_API_KEY", "short")
	if recorder := openGet(t, application, "/v1/open/info", http.Header{"X-API-Key": {"short"}}); recorder.Code != http.StatusUnauthorized {
		t.Fatal("a short key must not be honoured:", recorder.Code)
	}
}

// The machine-readable contract must be valid JSON and describe exactly the
// endpoints the router serves.
func TestOpenAPIDocumentListsTheOpenEndpoints(t *testing.T) {
	application := newApp(t)
	recorder := openGet(t, application, "/v1/openapi.json", basicHeader("test-password-16b"))
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatal(contentType)
	}
	var document struct {
		OpenAPI string `json:"openapi"`
		Paths   map[string]struct {
			Get  map[string]any `json:"get"`
			Post map[string]any `json:"post"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.OpenAPI == "" {
		t.Fatal("missing openapi version")
	}
	for _, path := range []string{
		"/v1/open/info", "/v1/open/playlists/parse", "/v1/open/search",
		"/v1/open/jobs/{id}", "/v1/open/jobs/{id}/review", "/v1/open/jobs/{id}/download",
		"/v1/open/jobs/{id}/download/control", "/v1/open/catalog/identify",
		"/v1/open/library/import", "/v1/open/library/tags",
	} {
		if _, ok := document.Paths[path]; !ok {
			t.Fatal("the document does not describe", path)
		}
	}
	// The capability answer and the contract must agree: a documented endpoint
	// that the instance does not advertise is a client trap.
	info := openGet(t, application, "/v1/open/info", basicHeader("test-password-16b"))
	var capabilities struct {
		Endpoints []string `json:"endpoints"`
	}
	if err := json.Unmarshal(info.Body.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	advertised := map[string]bool{}
	for _, endpoint := range capabilities.Endpoints {
		parts := strings.Fields(endpoint)
		advertised[parts[len(parts)-1]] = true
	}
	for path := range document.Paths {
		if !advertised[path] {
			t.Fatal("documented but not advertised:", path)
		}
	}
}

// The open namespace is an alias of the same handlers: creating a search
// through it produces a job the other routes can read.
func TestOpenSearchAliasCreatesAJob(t *testing.T) {
	application := newApp(t)
	body := searchBody("open-alias", "http://127.0.0.1:9", nil, false)
	delete(body, "batch_id")
	recorder := postRaw(t, application, "/v1/open/search", body)
	if recorder.Code != http.StatusAccepted {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	// The root job finishes identification and hands off to the search child,
	// which is where a human is asked to look.
	waitJob(t, application, "open-alias", "completed")
	status := openGet(t, application, "/v1/open/jobs/search:open-alias", basicHeader("test-password-16b"))
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"job_id":"search:open-alias"`) {
		t.Fatal(status.Code, status.Body.String())
	}
}

// The capability answer is for machines: no password, no credentials.
func TestOpenInfoCarriesNoSecrets(t *testing.T) {
	application := newApp(t)
	recorder := openGet(t, application, "/v1/open/info", basicHeader("test-password-16b"))
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if strings.Contains(body, "test-password-16b") || strings.Contains(body, "password") {
		t.Fatal(body)
	}
	if !strings.Contains(body, `"qbittorrent_configured":false`) {
		t.Fatal(body)
	}
}

// External software that already has the file imports it with the metadata it
// knows, without pretending a workflow job exists. The source must be in
// incoming/, and a second call replays instead of publishing twice.
func TestOpenImportPublishesAnIncomingFile(t *testing.T) {
	application := newApp(t)
	incoming := filepath.Join(application.Dir, "incoming")
	writeToneWAV(t, filepath.Join(incoming, "external.wav"), 440)
	body := map[string]any{
		"source": "incoming/external.wav", "artist": "External", "album": "Album", "title": "Song", "track": 1,
	}
	recorder := postRaw(t, application, "/v1/open/library/import", body)
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	decoded := decodeBody(t, recorder)
	if decoded["written"] != true || decoded["replayed"] != false {
		t.Fatal(decoded)
	}
	placed, _ := decoded["path"].(string)
	if !strings.HasSuffix(placed, filepath.Join("External", "Album", "01 Song.wav")) {
		t.Fatal(placed)
	}
	if _, err := os.Stat(placed); err != nil {
		t.Fatal("the file was not placed:", err)
	}

	again := postRaw(t, application, "/v1/open/library/import", body)
	if again.Code != http.StatusOK {
		t.Fatal(again.Code, again.Body.String())
	}
	if repeated := decodeBody(t, again); repeated["replayed"] != true {
		t.Fatal(repeated)
	}

	// A source outside incoming/ is refused with a reason, not copied.
	outside := postRaw(t, application, "/v1/open/library/import", map[string]any{
		"source": "state/musicops.sqlite", "artist": "A", "album": "B", "title": "C", "track": 1,
	})
	if outside.Code != http.StatusBadRequest || !strings.Contains(outside.Body.String(), "INVALID_REQUEST") {
		t.Fatal(outside.Code, outside.Body.String())
	}
}
