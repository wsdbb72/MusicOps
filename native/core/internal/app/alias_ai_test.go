package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func configureAliasAI(t *testing.T, application *App, suggestion string) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer alias-key" {
			http.Error(w, "unexpected AI request", http.StatusUnauthorized)
			return
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["model"] != "alias" {
			http.Error(w, "invalid AI request", http.StatusBadRequest)
			return
		}
		parts := []any{map[string]any{"type": "output_text", "text": suggestion}}
		body, err := json.Marshal(map[string]any{
			"status": "completed",
			"output": []any{map[string]any{"type": "message", "content": parts}},
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	application.aiHTTPClient = server.Client()
	post(t, application, "/v1/settings/ai", map[string]any{
		"enabled": true, "base_url": server.URL, "model": "alias", "api_key": "alias-key",
	})
	return server
}
