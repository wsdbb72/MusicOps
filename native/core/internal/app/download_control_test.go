package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func postDownloadControl(t *testing.T, application *App, id string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, "/v1/jobs/"+id+"/download/control", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/jobs/{id}/download/control", application.controlDownload)
	mux.ServeHTTP(recorder, request)
	return recorder
}

// A running download must be controllable from the outside: pause stops both
// the task and the torrent, resume starts both again, delete removes the
// torrent (keeping its files) and cancels the job. Before this, the torrent
// could only be stopped in qBittorrent itself, which the executor then
// reported as a failed download (P1-9).
func TestDownloadControlPausesResumesAndDeletes(t *testing.T) {
	wav := pcmWAV(100)
	torrent := buildTestTorrent(t, []testTorrentFile{{"01.wav", wav}})
	application := newApp(t)
	job := candidateJob(t, application, "album-control", torrent)
	fake := newFakeQBT(t, map[string][]byte{"album/01.wav": wav}, 1<<30)
	interval, timeout := downloadPollInterval, downloadPollTimeout
	downloadPollInterval, downloadPollTimeout = 30*time.Millisecond, 60*time.Second
	t.Cleanup(func() { downloadPollInterval, downloadPollTimeout = interval, timeout })

	creds := downloadBody(fake.url(), "secret-pass")
	if recorder := postDownload(t, application, job.ID, creds); recorder.Code != http.StatusAccepted {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	control := func(action string) map[string]any {
		t.Helper()
		body := map[string]any{"action": action, "base_url": fake.url(), "username": "qb", "password": "secret-pass"}
		recorder := postDownloadControl(t, application, job.ID, body)
		if recorder.Code != http.StatusOK {
			t.Fatal(action, recorder.Code, recorder.Body.String())
		}
		return decodeBody(t, recorder)
	}

	paused := control("pause")
	if paused["torrent"] != "paused" || paused["paused"] != true {
		t.Fatal(paused)
	}
	fake.mu.Lock()
	pausedCalls := fake.pausedCalls
	fake.mu.Unlock()
	if pausedCalls != 1 {
		t.Fatal("the client was not asked to pause:", pausedCalls)
	}
	if current := inspect(t, application, job.ID); !current.Job.Paused {
		t.Fatal("the job is not paused:", current.Job)
	}

	resumed := control("resume")
	if resumed["torrent"] != "started" || resumed["paused"] != false {
		t.Fatal(resumed)
	}
	if current := inspect(t, application, job.ID); current.Job.Paused {
		t.Fatal("the job is still paused:", current.Job)
	}

	rechecked := control("recheck")
	if rechecked["torrent"] != "rechecking" {
		t.Fatal(rechecked)
	}
	fake.mu.Lock()
	rechecks := fake.recheckCalls
	fake.mu.Unlock()
	if rechecks != 1 {
		t.Fatal("the client was not asked to recheck:", rechecks)
	}

	removed := control("delete")
	if removed["torrent"] != "removed" || removed["files_kept"] != true {
		t.Fatal(removed)
	}
	fake.mu.Lock()
	deletes, deleteFiles := fake.deleteCalls, fake.deleteFiles
	fake.mu.Unlock()
	if deletes != 1 || deleteFiles {
		t.Fatal("delete must remove the torrent and keep its files:", deletes, deleteFiles)
	}
	waitJob(t, application, job.ID, "cancelled")
}

// A job that is not downloading has no torrent to control.
func TestDownloadControlRefusesOtherStages(t *testing.T) {
	application := newApp(t)
	body := searchBody("album-no-download", "http://127.0.0.1:9", []string{}, false)
	post(t, application, "/v1/search", body)
	waitJob(t, application, "album-no-download", "completed")
	recorder := postDownloadControl(t, application, "album-no-download", map[string]any{"action": "pause", "base_url": "http://127.0.0.1:1", "username": "qb", "password": "x"})
	if recorder.Code != http.StatusConflict {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	recorder = postDownloadControl(t, application, "album-no-download", map[string]any{"action": "explode"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}
