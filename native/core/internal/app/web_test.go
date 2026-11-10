package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
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
	script := getAuth(t, application, "/app.js")
	style := getAuth(t, application, "/app.css")
	if home.Code != http.StatusOK || script.Code != http.StatusOK || style.Code != http.StatusOK {
		t.Fatal(home.Code, script.Code, style.Code)
	}
	pageText := home.Body.String() + script.Body.String()
	for _, leaked := range []string{"Python", "python", "种子字节", "执行器", "此进程", "站点回执", "authkey", "Pebble", "usetoken", "cookie-secret", "使用测试服务器", "Vault", "AGPL"} {
		if strings.Contains(pageText, leaked) {
			t.Fatal(leaked)
		}
	}
	if strings.Contains(home.Body.String(), "还不能修改连接") || strings.Contains(home.Body.String(), "还不能查看或整理") || strings.Contains(home.Body.String(), "还没有单独的运行记录") || !strings.Contains(home.Body.String(), `data-view="sites"`) || !strings.Contains(home.Body.String(), `data-view="library"`) || !strings.Contains(home.Body.String(), `data-view="logs"`) || !strings.Contains(script.Body.String(), "job.headline") || !strings.Contains(script.Body.String(), "job.progress") || !strings.Contains(script.Body.String(), "body.downloaded") || !strings.Contains(script.Body.String(), "body.library_published") || !strings.Contains(script.Body.String(), "/v1/sites") || !strings.Contains(script.Body.String(), "/v1/library") || !strings.Contains(script.Body.String(), "/v1/logs") || !strings.Contains(style.Body.String(), "max-width:720px") {
		t.Fatal("page controls")
	}
	if got := scrub("authkey=sentinel-dic-auth&torrent_pass=sentinel-orpheus-pass"); strings.Contains(got, "sentinel") || !strings.Contains(got, "authkey=redacted") {
		t.Fatal(got)
	}
	column := scrubColumn(`T{"cookie":"session=sentinel-cookie","note":"authkey=sentinel-dic-auth&torrent_pass=sentinel-orpheus-pass","tokens":252}`)
	if strings.Contains(column, "sentinel") || !strings.Contains(column, `"cookie":"redacted"`) || !strings.Contains(column, "authkey=redacted") || !strings.Contains(column, "252") {
		t.Fatal(column)
	}
	redactedTasks := publicTasks([]taskrepo.Task{{Message: "fetch authkey=sentinel-dic-auth", ErrorClass: "ok"}})
	if strings.Contains(redactedTasks[0].Message, "sentinel") || redactedTasks[0].Message == "" {
		t.Fatal(redactedTasks)
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
		if item.ID == "board-1" {
			if item.Progress != "待核对" || strings.Contains(item.Headline, "等待四站") || strings.Contains(item.Headline, "已完成") || !strings.Contains(item.Headline, "没有下载") {
				t.Fatal(item)
			}
		}
		if item.ID == "search:board-1" && item.Stage == "needs_review" && item.ParentID == "board-1" && item.Progress == "待核对" {
			found = true
		}
	}
	if !found {
		t.Fatal(decoded.Jobs)
	}
	detail := getAuth(t, application, "/v1/jobs/board-1")
	var shown struct {
		Progress string `json:"progress"`
		Headline string `json:"headline"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &shown); err != nil || shown.Progress != "待核对" || strings.Contains(shown.Headline, "等待四站") || strings.Contains(shown.Headline, "已完成") || !strings.Contains(shown.Headline, "没有下载") || strings.Contains(detail.Body.String(), "cookie-secret") {
		t.Fatal(shown, detail.Body.String())
	}
}

func TestAlbumProgressUsesOneUnderstandableState(t *testing.T) {
	shown := presentJobs([]jobView{
		{ID: "board-1", RootID: "board-1", Stage: "completed", Message: "身份证据已接收，等待四站只读检索；尚未下载", UpdatedNS: 1},
		{ID: "search:board-1", RootID: "board-1", ParentID: "board-1", Stage: "needs_review", Message: "检索需要核对；尚未下载", UpdatedNS: 2},
	})
	if len(shown) != 2 {
		t.Fatal(shown)
	}
	for _, item := range shown {
		if item.Progress != "待核对" || item.Headline != "需要核对。音乐还没有下载。" || item.Stage == "completed" && strings.Contains(item.Headline, "等待") {
			t.Fatal(item)
		}
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
