package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOperationsBoardReadsStoredJobsAndHidesSecrets(t *testing.T) {
	application := newApp(t)
	denied := httptest.NewRequest(http.MethodGet, "/", nil)
	deniedRecorder := httptest.NewRecorder()
	application.Handler().ServeHTTP(deniedRecorder, denied)
	if deniedRecorder.Code != http.StatusUnauthorized || strings.Contains(deniedRecorder.Body.String(), "<h1>") || strings.Contains(deniedRecorder.Body.String(), "cookie-secret") {
		t.Fatal(deniedRecorder.Code, deniedRecorder.Body.String())
	}

	home := getAuth(t, application, "/")
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), "任务") || !strings.Contains(home.Body.String(), "核对") || !strings.Contains(home.Body.String(), "此进程不启动 Python") || strings.Contains(home.Body.String(), "调用方已经确认免费") || strings.Contains(home.Body.String(), "cookie-secret") {
		t.Fatal(home.Code, home.Body.String())
	}
	script := getAuth(t, application, "/app.js")
	style := getAuth(t, application, "/app.css")
	if script.Code != http.StatusOK || style.Code != http.StatusOK || !strings.Contains(style.Body.String(), "max-width:720px") || strings.Contains(script.Body.String(), "cookie-secret") || strings.Contains(script.Body.String(), "sentinel-dic-auth") {
		t.Fatal(script.Code, style.Code)
	}
	if got := scrub("authkey=sentinel-dic-auth&torrent_pass=sentinel-orpheus-pass"); strings.Contains(got, "sentinel") || !strings.Contains(got, "authkey=redacted") {
		t.Fatal(got)
	}

	empty := getAuth(t, application, "/v1/jobs")
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"jobs":[]`) || strings.Contains(empty.Body.String(), "Example") {
		t.Fatal(empty.Body.String())
	}
	bad := getAuth(t, application, "/v1/jobs?after=@@@")
	if bad.Code != http.StatusBadRequest {
		t.Fatal(bad.Code, bad.Body.String())
	}

	body := searchBody("board-1", "http://127.0.0.1:9", []string{}, false)
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	body["orpheus_torrent_pass"] = "sentinel-orpheus-pass"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "search:board-1", "needs_review")
	if !strings.Contains(job.Message, "尚未下载") {
		t.Fatal(job.Message)
	}
	listed := getAuth(t, application, "/v1/jobs")
	raw := listed.Body.String()
	if listed.Code != http.StatusOK || !strings.Contains(raw, `"job_id":"board-1"`) || !strings.Contains(raw, `"job_id":"search:board-1"`) || !strings.Contains(raw, "needs_review") || strings.Contains(raw, "cookie-secret") || strings.Contains(raw, "sentinel-") || strings.Contains(raw, `"downloaded":true`) {
		t.Fatal(raw)
	}
	var decoded struct {
		Jobs []jobView `json:"jobs"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range decoded.Jobs {
		if item.ID == "search:board-1" && item.Stage == "needs_review" && item.ParentID == "board-1" {
			found = true
		}
	}
	if !found {
		t.Fatal(decoded.Jobs)
	}
}

func getAuth(t *testing.T, application *App, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.SetBasicAuth("musicops", "test-password-16b")
	recorder := httptest.NewRecorder()
	application.Handler().ServeHTTP(recorder, request)
	return recorder
}
