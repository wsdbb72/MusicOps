package aiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/reviewai"
)

// fakeAI records the last request and answers with a canned Responses payload.
type fakeAI struct {
	mu       sync.Mutex
	status   int
	body     string
	hits     int
	lastPath string
	lastAuth string
	lastCT   string
	lastBody []byte
}

func (f *fakeAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.hits++
	f.lastPath = r.URL.Path
	f.lastAuth = r.Header.Get("Authorization")
	f.lastCT = r.Header.Get("Content-Type")
	f.lastBody = append([]byte(nil), body...)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.status)
	_, _ = w.Write([]byte(f.body))
}

func (f *fakeAI) request(t *testing.T) (path, auth, contentType string, body []byte, hits int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPath, f.lastAuth, f.lastCT, append([]byte(nil), f.lastBody...), f.hits
}

// responsesBody builds a Responses payload whose output_text parts carry text.
func responsesBody(status string, texts ...string) string {
	parts := make([]any, 0, len(texts))
	for _, text := range texts {
		parts = append(parts, map[string]any{"type": "output_text", "text": text})
	}
	raw, err := json.Marshal(map[string]any{
		"status": status,
		"output": []any{map[string]any{"type": "message", "content": parts}},
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func choiceJSON(candidateID any, confidence float64, ambiguous bool, reason string) string {
	raw, err := json.Marshal(map[string]any{
		"candidate_id": candidateID,
		"confidence":   confidence,
		"ambiguous":    ambiguous,
		"reason":       reason,
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	client, err := New(srv.URL, "test-model", "test-key")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client.HTTPClient = srv.Client()
	return client
}

func testPrompt() []byte {
	raw, err := json.Marshal(reviewai.Request{
		ExpectedTitle:  "Blue",
		ExpectedArtist: "Artist",
		Candidates: []reviewai.Candidate{
			{ID: "c1", Title: "蓝", Artists: []string{"Artist"}, Aliases: []string{"Blue"}},
			{ID: "c2", Title: "Blue", Artists: []string{"Other"}},
		},
	})
	if err != nil {
		panic(err)
	}
	return raw
}

// decodeVerdict unmarshals with DisallowUnknownFields, the way
// reviewai.parseVerdict does, so the test proves the verdict shape is
// exactly the three accepted fields.
func decodeVerdict(t *testing.T, body []byte) verdict {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	var parsed verdict
	if err := decoder.Decode(&parsed); err != nil {
		t.Fatalf("verdict does not decode with DisallowUnknownFields: %v (body %s)", err, body)
	}
	return parsed
}

func TestNewValidatesAndNormalizes(t *testing.T) {
	client, err := New("https://ai.example.com/v1/", "gpt-x", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if client.BaseURL != "https://ai.example.com/v1" {
		t.Fatalf("BaseURL = %q, want trailing slash removed", client.BaseURL)
	}
	if client.Timeout != 60*time.Second || client.MaxOutputTokens != 2048 || client.Temperature != 0.2 {
		t.Fatalf("defaults = %v/%d/%v", client.Timeout, client.MaxOutputTokens, client.Temperature)
	}
	if client.HTTPClient != http.DefaultClient {
		t.Fatal("HTTPClient should default to http.DefaultClient")
	}
	for _, bad := range []struct {
		name   string
		base   string
		model  string
		apiKey string
	}{
		{"empty base", "", "m", "k"},
		{"no host", "https://", "m", "k"},
		{"userinfo", "https://user:pass@ai.example.com", "m", "k"},
		{"query", "https://ai.example.com/?q=1", "m", "k"},
		{"fragment", "https://ai.example.com/#f", "m", "k"},
		{"empty model", "https://ai.example.com", "", "k"},
		{"blank model", "https://ai.example.com", "  ", "k"},
		{"empty key", "https://ai.example.com", "m", ""},
		{"blank key", "https://ai.example.com", "m", "   "},
	} {
		if _, err := New(bad.base, bad.model, bad.apiKey); err == nil {
			t.Errorf("%s: New should fail", bad.name)
		}
	}
	// HTTP is accepted by New; it is rejected at request time instead.
	if _, err := New("http://ai.example.com", "m", "k"); err != nil {
		t.Fatalf("New should accept http (HTTPS is enforced per request): %v", err)
	}
}

func TestCompletePass(t *testing.T) {
	fake := &fakeAI{status: http.StatusOK, body: responsesBody("completed", choiceJSON("c1", 0.95, false, "alias matches"))}
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	client := newTestClient(t, srv)
	prompt := testPrompt()

	body, err := client.Complete(context.Background(), prompt)
	if err != nil {
		t.Fatal(err)
	}
	parsed := decodeVerdict(t, body)
	if parsed.Decision != "pass" || parsed.CandidateID != "c1" || parsed.Reason != "alias matches" {
		t.Fatalf("verdict = %+v", parsed)
	}
	if !strings.Contains(string(body), `"candidate_id":"c1"`) {
		t.Fatalf("pass verdict must carry candidate_id: %s", body)
	}
	path, auth, contentType, lastBody, hits := fake.request(t)
	if hits != 1 || path != "/responses" || auth != "Bearer test-key" || contentType != "application/json" {
		t.Fatalf("request = path %q auth %q content-type %q hits %d", path, auth, contentType, hits)
	}
	var sent map[string]any
	if err := json.Unmarshal(lastBody, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["model"] != "test-model" || sent["max_output_tokens"] != float64(2048) || sent["temperature"] != 0.2 {
		t.Fatalf("request fields = %v", sent)
	}
	format, ok := sent["text"].(map[string]any)["format"].(map[string]any)
	if !ok || format["name"] != "musicops_recognition_choice" || format["type"] != "json_schema" {
		t.Fatalf("text.format = %v", sent["text"])
	}
	input, ok := sent["input"].([]any)
	if !ok || len(input) != 2 {
		t.Fatalf("input = %v", sent["input"])
	}
	user, ok := input[1].(map[string]any)
	if !ok || user["role"] != "user" || user["content"] != string(prompt) {
		t.Fatalf("user message must carry the prompt verbatim: %v", input[1])
	}
	system, ok := input[0].(map[string]any)
	if !ok || system["role"] != "system" || !strings.Contains(system["content"].(string), "只从给定 observed_candidates 里选择") {
		t.Fatalf("system message must explain choosing only from the candidates: %v", input[0])
	}
}

func TestCompleteEndpointSuffix(t *testing.T) {
	fake := &fakeAI{status: http.StatusOK, body: responsesBody("completed", choiceJSON(nil, 0.1, true, "no evidence"))}
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()

	// Base URL already ending in /responses must not gain a second one.
	client, err := New(srv.URL+"/responses", "m", "k")
	if err != nil {
		t.Fatal(err)
	}
	client.HTTPClient = srv.Client()
	if _, err := client.Complete(context.Background(), testPrompt()); err != nil {
		t.Fatal(err)
	}
	if path, _, _, _, _ := fake.request(t); path != "/responses" {
		t.Fatalf("path = %q, want /responses", path)
	}

	// A versioned base URL gains /responses after its own path.
	client2, err := New(srv.URL+"/v1", "m", "k")
	if err != nil {
		t.Fatal(err)
	}
	client2.HTTPClient = srv.Client()
	if _, err := client2.Complete(context.Background(), testPrompt()); err != nil {
		t.Fatal(err)
	}
	if path, _, _, _, _ := fake.request(t); path != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", path)
	}
}

func TestCompleteReviewCases(t *testing.T) {
	cases := []struct {
		name   string
		choice string
	}{
		{"null candidate", choiceJSON(nil, 0.2, true, "no evidence")},
		{"unknown candidate", choiceJSON("invented", 0.99, false, "model invented an id")},
		{"ambiguous candidate", choiceJSON("c1", 0.99, true, "two releases match")},
		{"low confidence", choiceJSON("c1", 0.5, false, "weak evidence")},
		{"fenced json", "```json\n" + choiceJSON(nil, 0.1, true, "no evidence") + "\n```"},
		{"split output parts", ""}, // replaced below: text arrives in two parts
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body string
			if tc.name == "split output parts" {
				body = responsesBody("completed", `{"candidate_id":null,"confidence":0.1,`, `"ambiguous":true,"reason":"no evidence"}`)
			} else {
				body = responsesBody("completed", tc.choice)
			}
			fake := &fakeAI{status: http.StatusOK, body: body}
			srv := httptest.NewTLSServer(fake)
			defer srv.Close()
			out, err := newTestClient(t, srv).Complete(context.Background(), testPrompt())
			if err != nil {
				t.Fatalf("review must not be an error: %v", err)
			}
			parsed := decodeVerdict(t, out)
			if parsed.Decision != "review" || parsed.CandidateID != "" {
				t.Fatalf("verdict = %+v, want review without candidate_id", parsed)
			}
			if strings.Contains(string(out), "candidate_id") {
				t.Fatalf("review verdict must omit candidate_id: %s", out)
			}
		})
	}
}

func TestCompleteStatusErrors(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrAuthFailed},
		{http.StatusForbidden, ErrAuthFailed},
		{http.StatusTooManyRequests, ErrRateLimited},
		{http.StatusInternalServerError, ErrTransient},
		{http.StatusBadGateway, ErrTransient},
		{http.StatusTeapot, ErrContract},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			fake := &fakeAI{status: tc.status, body: `{}`}
			srv := httptest.NewTLSServer(fake)
			defer srv.Close()
			_, err := newTestClient(t, srv).Complete(context.Background(), testPrompt())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCompleteOutputErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{"refusal", responsesBody("completed", ""), ErrRefused}, // replaced below
		{"empty output list", `{"status":"completed","output":[]}`, ErrEmptyOutput},
		{"empty text", responsesBody("completed", "   "), ErrEmptyOutput},
		{"missing output", `{"status":"completed"}`, ErrEmptyOutput},
		{"failed status", responsesBody("failed", choiceJSON(nil, 0.1, true, "x")), ErrContract},
		{"invalid response json", `not json`, ErrContract},
		{"output not a list", `{"status":"completed","output":{}}`, ErrContract},
		{"choice not json", responsesBody("completed", "not json"), ErrContract},
		{"choice missing field", responsesBody("completed", `{"candidate_id":null,"confidence":0.1,"ambiguous":true}`), ErrContract},
		{"choice extra field", responsesBody("completed", `{"candidate_id":null,"confidence":0.1,"ambiguous":true,"reason":"x","extra":1}`), ErrContract},
		{"confidence out of range", responsesBody("completed", choiceJSON("c1", 1.5, false, "x")), ErrContract},
		{"confidence wrong type", responsesBody("completed", `{"candidate_id":null,"confidence":"high","ambiguous":true,"reason":"x"}`), ErrContract},
		{"ambiguous wrong type", responsesBody("completed", `{"candidate_id":null,"confidence":0.1,"ambiguous":"yes","reason":"x"}`), ErrContract},
		{"reason too long", responsesBody("completed", choiceJSON(nil, 0.1, true, strings.Repeat("字", 1001))), ErrContract},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if tc.name == "refusal" {
				raw, _ := json.Marshal(map[string]any{
					"status": "completed",
					"output": []any{map[string]any{"type": "message", "content": []any{
						map[string]any{"type": "refusal", "refusal": "I cannot help"},
					}}},
				})
				body = string(raw)
			}
			fake := &fakeAI{status: http.StatusOK, body: body}
			srv := httptest.NewTLSServer(fake)
			defer srv.Close()
			_, err := newTestClient(t, srv).Complete(context.Background(), testPrompt())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCompleteRejectsNonHTTPS(t *testing.T) {
	fake := &fakeAI{status: http.StatusOK, body: responsesBody("completed", choiceJSON("c1", 0.95, false, "x"))}
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	plain := "http://" + strings.TrimPrefix(srv.URL, "https://")
	client, err := New(plain, "m", "k")
	if err != nil {
		t.Fatalf("New must accept http and defer the HTTPS check to the request: %v", err)
	}
	client.HTTPClient = srv.Client()
	_, err = client.Complete(context.Background(), testPrompt())
	if !errors.Is(err, ErrInsecureBaseURL) {
		t.Fatalf("err = %v, want %v", err, ErrInsecureBaseURL)
	}
	if _, _, _, _, hits := fake.request(t); hits != 0 {
		t.Fatal("no request must be sent over plain HTTP")
	}
}

func TestCompleteRejectsOversizeRequest(t *testing.T) {
	fake := &fakeAI{status: http.StatusOK, body: responsesBody("completed", choiceJSON("c1", 0.95, false, "x"))}
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	huge, err := json.Marshal(reviewai.Request{
		ExpectedTitle: "Blue",
		Candidates:    []reviewai.Candidate{{ID: "c1", Title: strings.Repeat("x", 200000)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = newTestClient(t, srv).Complete(context.Background(), huge)
	if !errors.Is(err, ErrRequestTooLarge) {
		t.Fatalf("err = %v, want %v", err, ErrRequestTooLarge)
	}
	if _, _, _, _, hits := fake.request(t); hits != 0 {
		t.Fatal("an oversize request must never be sent")
	}
}

func TestCompleteRejectsBadPrompt(t *testing.T) {
	fake := &fakeAI{status: http.StatusOK, body: responsesBody("completed", choiceJSON("c1", 0.95, false, "x"))}
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	_, err := newTestClient(t, srv).Complete(context.Background(), []byte(`not json`))
	if !errors.Is(err, ErrContract) {
		t.Fatalf("err = %v, want %v", err, ErrContract)
	}
}

func TestTestConnection(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		fake := &fakeAI{status: http.StatusOK, body: responsesBody("completed", "连接成功")}
		srv := httptest.NewTLSServer(fake)
		defer srv.Close()
		client := newTestClient(t, srv)
		if err := client.TestConnection(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, _, _, lastBody, _ := fake.request(t)
		var sent map[string]any
		if err := json.Unmarshal(lastBody, &sent); err != nil {
			t.Fatal(err)
		}
		if _, ok := sent["text"]; ok {
			t.Fatal("connection test must not request a structured format")
		}
		input, ok := sent["input"].([]any)
		if !ok || len(input) != 2 {
			t.Fatalf("input = %v", sent["input"])
		}
		system := input[0].(map[string]any)["content"].(string)
		user := input[1].(map[string]any)["content"].(string)
		if !strings.Contains(system, "connection_test") {
			t.Fatalf("system must be marked connection_test: %q", system)
		}
		if !strings.Contains(user, "这是连接测试。仅返回连接成功，不搜索，不调用工具。") {
			t.Fatalf("user must carry the connection test instruction: %q", user)
		}
		if !strings.Contains(user, `"connection_test":true`) {
			t.Fatalf("user must carry the connection_test context: %q", user)
		}
	})

	t.Run("auth failure", func(t *testing.T) {
		fake := &fakeAI{status: http.StatusUnauthorized, body: `{}`}
		srv := httptest.NewTLSServer(fake)
		defer srv.Close()
		if err := newTestClient(t, srv).TestConnection(context.Background()); !errors.Is(err, ErrAuthFailed) {
			t.Fatalf("err = %v, want %v", err, ErrAuthFailed)
		}
	})

	t.Run("empty output", func(t *testing.T) {
		fake := &fakeAI{status: http.StatusOK, body: responsesBody("completed", "  ")}
		srv := httptest.NewTLSServer(fake)
		defer srv.Close()
		if err := newTestClient(t, srv).TestConnection(context.Background()); !errors.Is(err, ErrEmptyOutput) {
			t.Fatalf("err = %v, want %v", err, ErrEmptyOutput)
		}
	})
}

// TestAdviseDrivesClient proves the Completer wiring end to end: a real
// reviewai.Cache accepts the client's pass verdict for cited evidence.
func TestAdviseDrivesClient(t *testing.T) {
	fake := &fakeAI{status: http.StatusOK, body: responsesBody("completed", choiceJSON("dic:1", 0.95, false, "alias on the record"))}
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	cache := reviewai.NewCache(2)
	result := cache.Advise(context.Background(), newTestClient(t, srv), reviewai.Request{
		ExpectedTitle:  "Blue",
		ExpectedArtist: "Artist",
		Candidates: []reviewai.Candidate{{
			ID: "dic:1", Title: "蓝", Artists: []string{"Artist"}, Aliases: []string{"Blue"},
		}},
	})
	if !result.Pass || result.CandidateID != "dic:1" || result.Calls != 1 {
		t.Fatalf("result = %+v", result)
	}
}
