package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The streaming catalogues are additional sources, not confirmations: the
// endpoint reports candidates and never writes anything itself.
func TestSourcesLookupReturnsCandidates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search/get/web" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "result": map[string]any{"songs": []map[string]any{
			{"id": 1, "name": "Super Lady", "duration": 200000, "album": map[string]any{"name": "2"},
				"artists": []map[string]any{{"name": "i-dle"}}},
		}}})
	}))
	defer server.Close()
	application := newApp(t)
	application.neteaseBases = map[string]string{"api": server.URL}

	body := post(t, application, "/v1/sources/lookup", map[string]any{
		"title": "Super Lady", "artist": "i-dle", "sources": []string{"netease"},
	})
	if body["count"] != float64(1) {
		t.Fatal(body)
	}
	rows, _ := body["candidates"].([]any)
	row, _ := rows[0].(map[string]any)
	if row["source"] != "netease" || row["album"] != "2" || row["exact"] != true {
		t.Fatal(row)
	}
	// Nothing was written or queued by looking up.
	jobs := getAuth(t, application, "/v1/jobs")
	if jobs.Code != http.StatusOK || !containsQuiet(jobs.Body.String(), `"jobs":[]`) {
		t.Fatal(jobs.Body.String())
	}
}

func containsQuiet(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
