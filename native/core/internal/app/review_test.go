package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func getJSON(t *testing.T, application *App, path string) map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.SetBasicAuth("musicops", "test-password-16b")
	recorder := httptest.NewRecorder()
	application.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func postReview(t *testing.T, application *App, id string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return postCode(t, application, "/v1/jobs/"+id+"/review", body)
}

// Two qualified sites leave the search job in needs_review (candidate,
// selection_required): the machine will not pick between two exact seeded
// hits. Accepting one must complete the search and queue the process child.
func TestReviewAcceptTwoQualifiedSitesViaHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The torrent detail page: this candidate is the account's own
		// freeleech, so the acquire fetches it without spending a token.
		if r.URL.Path == "/ajax.php" && r.URL.Query().Get("action") == "torrent" {
			io.WriteString(w, `{"status":"success","response":{"torrent":{"id":11,"isPersonalFreeleech":true}}}`)
			return
		}
		// The torrent download itself: a minimal valid metainfo file.
		if r.URL.Path == "/torrents.php" && r.URL.Query().Get("action") == "download" {
			w.Header().Set("Content-Type", "application/x-bittorrent")
			io.WriteString(w, "d8:announce12:http://a.com4:infod6:lengthi1e4:name4:test12:piece lengthi16384e6:pieces20:01234567890123456789ee")
			return
		}
		switch r.Header.Get("X-MusicOps-Site") {
		case "dicmusic":
			// 2023: the same title as the orpheus hit, a different release
			// year, so the machine must ask a human to choose.
			io.WriteString(w, gazelleHitYear("11", 2023))
		case "orpheus":
			io.WriteString(w, gazelleHitYear("21", 2024))
		case "open.cd":
			io.WriteString(w, `<html><a href="logout.php">logout</a><div>没有种子</div></html>`)
		default:
			io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
		}
	}))
	defer server.Close()
	application := newApp(t)
	body := searchBody("album-two", server.URL, []string{"open.cd", "dicmusic", "orpheus", "redacted"}, false)
	body["dicmusic_authkey"] = "authkey-test"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "search:album-two", "needs_review")
	if job.ReviewKind != "candidate" || job.SearchAction != "review_candidate" {
		t.Fatal(job)
	}

	data := getJSON(t, application, "/v1/jobs/search:album-two/review")
	if data["ok"] != true || data["review_kind"] != "candidate" {
		t.Fatal(data)
	}
	var candidateID string
	candidates, _ := data["candidates"].([]any)
	for _, item := range candidates {
		row, _ := item.(map[string]any)
		if row["site"] == "dicmusic" {
			candidateID, _ = row["candidate_id"].(string)
		}
	}
	if candidateID == "" || len(candidates) != 2 {
		t.Fatal(data["candidates"])
	}
	allowed := map[string]bool{}
	actions, _ := data["actions"].([]any)
	for _, item := range actions {
		row, _ := item.(map[string]any)
		if ok, _ := row["allowed"].(bool); ok {
			allowed[row["action"].(string)] = true
		}
	}
	if !allowed["accept"] || !allowed["retry"] || !allowed["skip"] {
		t.Fatal(data["actions"])
	}

	recorder := postReview(t, application, "search:album-two", map[string]any{"action": "accept", "site": "dicmusic", "candidate_id": candidateID})
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	var accepted struct {
		Job      jobView   `json:"job"`
		Children []jobView `json:"children"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.Job.Stage != "completed" || len(accepted.Children) != 1 || accepted.Children[0].ID != "process:search:album-two" {
		t.Fatal(accepted)
	}
	child := waitJob(t, application, "process:search:album-two", "needs_review")
	if child.ReviewKind != "candidate" {
		t.Fatal(child)
	}
	inspection := inspect(t, application, "process:search:album-two")
	if inspection.Task == nil {
		t.Fatal("process child has no task")
	}
	raw, found, err := application.tasks.Data(inspection.Task.ID, "arguments", 0)
	if err != nil || !found {
		t.Fatal(err, found)
	}
	var plan SearchPlan
	if err := json.Unmarshal(raw, &plan); err != nil || plan.SelectedSite != "dicmusic" || plan.SelectedCandidateID != candidateID {
		t.Fatal(plan, err)
	}

	// Accepting a selection outside the stored evidence is refused.
	recorder = postReview(t, application, "search:album-two", map[string]any{"action": "accept", "site": "redacted", "candidate_id": "redacted:99"})
	if recorder.Code != http.StatusConflict && recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}

// An auth failure leaves the search job in needs_review (search): retry
// re-queues it, and skip abandons it.
func TestReviewRetryAndSkipViaHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	application := newApp(t)
	post(t, application, "/v1/search", searchBody("album-auth-review", server.URL, []string{"dicmusic"}, false))
	job := waitJob(t, application, "search:album-auth-review", "needs_review")
	if job.ReviewKind != "search" {
		t.Fatal(job)
	}

	data := getJSON(t, application, "/v1/jobs/search:album-auth-review/review")
	allowed := map[string]bool{}
	actions, _ := data["actions"].([]any)
	for _, item := range actions {
		row, _ := item.(map[string]any)
		if ok, _ := row["allowed"].(bool); ok {
			allowed[row["action"].(string)] = true
		}
	}
	if allowed["accept"] || !allowed["retry"] || !allowed["skip"] {
		t.Fatal(data["actions"])
	}

	recorder := postReview(t, application, "search:album-auth-review", map[string]any{"action": "retry"})
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	var retryBody struct {
		Job   jobView `json:"job"`
		Stage string  `json:"stage"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &retryBody); err != nil {
		t.Fatal(err)
	}
	if retryBody.Stage != "searching" || retryBody.Job.Stage != "searching" {
		t.Fatal(retryBody)
	}
	// The retry is synchronous, but the re-run may already be back in review
	// by the time we poll, so the durable evidence is the incremented count.
	retried := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if job := inspect(t, application, "search:album-auth-review").Job; job.SearchRetryCount == 1 {
			retried = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !retried {
		t.Fatal("retry did not increment the search retry count")
	}
	waitJob(t, application, "search:album-auth-review", "needs_review")
	recorder = postReview(t, application, "search:album-auth-review", map[string]any{"action": "skip"})
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	skipped := inspect(t, application, "search:album-auth-review").Job
	if skipped.Stage != "skipped" || skipped.SearchAction != "skip" {
		t.Fatal(skipped)
	}
	// A skipped job cannot be retried.
	recorder = postReview(t, application, "search:album-auth-review", map[string]any{"action": "retry"})
	if recorder.Code != http.StatusConflict {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	// An unknown action is a bad request, not a silent no-op.
	recorder = postReview(t, application, "search:album-auth-review", map[string]any{"action": "explode"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}

// gazelleHitYear is gazelleHit with a release year. Two hits with the same
// title but different non-zero years are not the same release, so the
// machine must ask a human to choose between them.
func gazelleHitYear(id string, year int) string {
	return `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"Example &amp; Test","groupYear":` + strconv.Itoa(year) + `,"artist":"Artist","torrents":[{"torrentId":` + id + `,"format":"FLAC","encoding":"Lossless","seeders":3}]}]}}`
}

// distinctEvidence returns a search body with a per-job identity evidence
// id: the workflow rejects a second job that reuses an evidence id.
func distinctEvidence(body map[string]any, evidenceID string) map[string]any {
	identity, _ := body["identity"].(map[string]any)
	identity["evidence_id"] = evidenceID
	return body
}

// Retrying a search really searches again: the first run fails auth, the
// retry re-runs with the kept cookie and finds an exact hit. This proves the
// retry is not a replay: the cookie survives the review, the second attempt
// attaches its own evidence, and the search completes.
func TestReviewRetryResearchesAgainViaHTTP(t *testing.T) {
	var dicmusicCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The torrent detail page: this candidate is the account's own
		// freeleech, so the acquire fetches it without spending a token.
		if r.URL.Path == "/ajax.php" && r.URL.Query().Get("action") == "torrent" {
			io.WriteString(w, `{"status":"success","response":{"torrent":{"id":11,"isPersonalFreeleech":true}}}`)
			return
		}
		// The torrent download itself: a minimal valid metainfo file.
		if r.URL.Path == "/torrents.php" && r.URL.Query().Get("action") == "download" {
			w.Header().Set("Content-Type", "application/x-bittorrent")
			io.WriteString(w, "d8:announce12:http://a.com4:infod6:lengthi1e4:name4:test12:piece lengthi16384e6:pieces20:01234567890123456789ee")
			return
		}
		switch r.Header.Get("X-MusicOps-Site") {
		case "dicmusic":
			if dicmusicCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			io.WriteString(w, gazelleHit("11"))
		case "open.cd":
			io.WriteString(w, `<html><a href="logout.php">logout</a><div>没有种子</div></html>`)
		default:
			io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
		}
	}))
	defer server.Close()
	application := newApp(t)
	body := searchBody("album-again", server.URL, []string{"open.cd", "dicmusic", "orpheus", "redacted"}, false)
	body["dicmusic_authkey"] = "authkey-test"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "search:album-again", "needs_review")
	if job.ReviewKind != "search" {
		t.Fatal(job)
	}
	recorder := postReview(t, application, "search:album-again", map[string]any{"action": "retry"})
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	searchJob := waitJob(t, application, "search:album-again", "completed")
	if searchJob.SearchAction != "queue_request" {
		t.Fatal(searchJob)
	}
	child := waitJob(t, application, "process:search:album-again", "needs_review")
	if child.ReviewKind != "candidate" {
		t.Fatal(child)
	}
	if dicmusicCalls.Load() != 2 {
		t.Fatal("dicmusic calls:", dicmusicCalls.Load())
	}
	// The review data for the completed search shows the hit from the second
	// run, not the auth failure from the first.
	data := getJSON(t, application, "/v1/jobs/search:album-again/review")
	candidates, _ := data["candidates"].([]any)
	if len(candidates) != 1 {
		t.Fatal(data["candidates"])
	}
}

func TestBatchJobsViaHTTP(t *testing.T) {
	application := newApp(t)
	if err := application.runner.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/search", searchBody("batch-1", "http://127.0.0.1:9", []string{}, false))
	post(t, application, "/v1/search", distinctEvidence(searchBody("batch-2", "http://127.0.0.1:9", []string{}, false), "ev-batch-2"))

	recorder := postCode(t, application, "/v1/jobs/batch", map[string]any{"job_ids": []string{"batch-1", "batch-2"}, "action": "pause"})
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	var batch struct {
		Succeeded int `json:"succeeded"`
		Failed    int `json:"failed"`
		Results   []struct {
			JobID string `json:"job_id"`
			OK    bool   `json:"ok"`
		} `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &batch); err != nil || batch.Succeeded != 2 || batch.Failed != 0 {
		t.Fatal(batch, err)
	}
	if paused := inspect(t, application, "batch-1").Job; !paused.Paused {
		t.Fatal(paused)
	}

	// One unknown id does not fail the whole batch: it is reported per job.
	recorder = postCode(t, application, "/v1/jobs/batch", map[string]any{"job_ids": []string{"batch-1", "no-such-job"}, "action": "resume"})
	var mixed struct {
		Succeeded int `json:"succeeded"`
		Failed    int `json:"failed"`
		Results   []struct {
			JobID string `json:"job_id"`
			OK    bool   `json:"ok"`
			Code  string `json:"code"`
		} `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &mixed); err != nil || mixed.Succeeded != 1 || mixed.Failed != 1 {
		t.Fatal(mixed, err)
	}
	if mixed.Results[1].Code != "NOT_FOUND" {
		t.Fatal(mixed.Results)
	}
	// Retry is refused for a live job that is neither blocked nor failed.
	recorder = postCode(t, application, "/v1/jobs/batch", map[string]any{"job_ids": []string{"batch-2"}, "action": "retry"})
	if err := json.Unmarshal(recorder.Body.Bytes(), &mixed); err != nil || mixed.Succeeded != 0 || mixed.Failed != 1 || mixed.Results[0].Code != "WORKFLOW_REJECTED" {
		t.Fatal(mixed, err)
	}

	recorder = postCode(t, application, "/v1/jobs/batch", map[string]any{"job_ids": []string{"batch-1", "batch-2"}, "action": "cancel"})
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	if cancelled := inspect(t, application, "batch-2").Job; cancelled.Stage != "cancelled" {
		t.Fatal(cancelled)
	}

	for _, bad := range []map[string]any{
		{"job_ids": []string{}, "action": "pause"},
		{"job_ids": []string{"batch-1", "batch-1"}, "action": "pause"},
		{"job_ids": []string{"batch-1"}, "action": "explode"},
	} {
		recorder = postCode(t, application, "/v1/jobs/batch", bad)
		if recorder.Code != http.StatusBadRequest {
			t.Fatal(bad, recorder.Code, recorder.Body.String())
		}
	}
	many := make([]string, 51)
	for i := range many {
		many[i] = "batch-1"
	}
	recorder = postCode(t, application, "/v1/jobs/batch", map[string]any{"job_ids": many, "action": "pause"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}

func TestPreflightSyntheticViaHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") == "index" {
			io.WriteString(w, `{"status":"success","response":{"id":7,"username":"tester","userstats":{"tokens":4}}}`)
			return
		}
		io.WriteString(w, gazelleHit("11"))
	}))
	defer server.Close()
	application := newApp(t)

	recorder := postCode(t, application, "/v1/sites/preflight", map[string]any{"site": "dicmusic", "cookie": "session=preflight", "synthetic": true, "base_url": server.URL})
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	var checked struct {
		OK            bool `json:"ok"`
		Authenticated bool `json:"authenticated"`
		TokenBalance  *int `json:"token_balance"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &checked); err != nil || !checked.OK || !checked.Authenticated || checked.TokenBalance == nil || *checked.TokenBalance != 4 {
		t.Fatal(checked, err)
	}
	if dirContains(t, application.Dir, "session=preflight") {
		t.Fatal("preflight cookie was written to the native store")
	}

	for _, bad := range []map[string]any{
		{"site": "unknown", "cookie": "x", "synthetic": true, "base_url": server.URL},
		{"site": "dicmusic", "cookie": "", "synthetic": true, "base_url": server.URL},
		{"site": "dicmusic", "cookie": "x", "synthetic": false, "base_url": server.URL},
	} {
		recorder = postCode(t, application, "/v1/sites/preflight", bad)
		if recorder.Code != http.StatusBadRequest {
			t.Fatal(bad, recorder.Code, recorder.Body.String())
		}
	}
}

func TestDownloadSettingsDefaultAppliesToSearch(t *testing.T) {
	application := newApp(t)
	if err := application.runner.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	data := getJSON(t, application, "/v1/settings/download")
	if data["streaming_enabled"] != false || data["streaming_fallback_wired"] != false {
		t.Fatal(data)
	}
	sources, _ := data["streaming_sources"].([]any)
	if len(sources) == 0 {
		t.Fatal(data)
	}

	recorder := postCode(t, application, "/v1/settings/download", map[string]any{"streaming_enabled": true})
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	if got := getJSON(t, application, "/v1/settings/download"); got["streaming_enabled"] != true {
		t.Fatal(got)
	}

	// No streaming_enabled in the search: the saved default applies.
	body := searchBody("album-default", "http://127.0.0.1:9", []string{}, false)
	delete(body, "streaming_enabled")
	post(t, application, "/v1/search", body)
	plan := storedPlan(t, application, "album-default")
	if !plan.StreamingEnabled {
		t.Fatal(plan)
	}
	// An explicit value wins over the saved default.
	body = distinctEvidence(searchBody("album-explicit", "http://127.0.0.1:9", []string{}, false), "ev-explicit")
	post(t, application, "/v1/search", body)
	if plan := storedPlan(t, application, "album-explicit"); plan.StreamingEnabled {
		t.Fatal(plan)
	}
	// A missing streaming_enabled in the settings body is a bad request.
	recorder = postCode(t, application, "/v1/settings/download", map[string]any{})
	if recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}

func storedPlan(t *testing.T, application *App, jobID string) SearchPlan {
	t.Helper()
	inspection := inspect(t, application, jobID)
	if inspection.Task == nil {
		t.Fatal(jobID, "has no task")
	}
	raw, found, err := application.tasks.Data(inspection.Task.ID, "arguments", 0)
	if err != nil || !found {
		t.Fatal(err, found)
	}
	var plan SearchPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

// The new Phase 2 views and actions are present in the served page and script.
func TestPhase2WebUIFragments(t *testing.T) {
	application := newApp(t)
	home := getAuth(t, application, "/")
	script := getAuth(t, application, "/app.js")
	page := home.Body.String() + script.Body.String()
	for _, fragment := range []string{
		`data-view="search"`, `data-view="settings"`, `id="retry-job"`, `id="review-box"`,
		"setStatus(", "/v1/jobs/batch", "/v1/sites/preflight", "/v1/settings/download",
		"review-candidate", "data-batch-action", "search-form", "preflight-site",
	} {
		if !strings.Contains(page, fragment) {
			t.Fatal("missing Phase 2 fragment", fragment)
		}
	}
}
