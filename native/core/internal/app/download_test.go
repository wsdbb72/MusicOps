package app

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/qbittorrent"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

// testTorrentFile is one file inside a synthetic torrent.
type testTorrentFile struct {
	name string
	data []byte
}

// buildTestTorrent builds a valid v1 multi-file torrent whose root directory
// is "album". The bencode keys are emitted in sorted order (the strict
// decoder rejects unordered keys) and the piece hashes are real SHA-1s over
// the concatenated file bytes in listed order. The result is checked with
// the real ParseMetainfo before it is used.
func buildTestTorrent(t *testing.T, files []testTorrentFile) []byte {
	t.Helper()
	var all bytes.Buffer
	for _, f := range files {
		all.Write(f.data)
	}
	data := all.Bytes()
	var pieces []byte
	for off := 0; off < len(data); off += 16384 {
		end := off + 16384
		if end > len(data) {
			end = len(data)
		}
		sum := sha1.Sum(data[off:end])
		pieces = append(pieces, sum[:]...)
	}
	var info bytes.Buffer
	info.WriteString("d5:filesl")
	for _, f := range files {
		fmt.Fprintf(&info, "d6:lengthi%de4:pathl%d:%see", len(f.data), len(f.name), f.name)
	}
	fmt.Fprintf(&info, "e4:name5:album12:piece lengthi16384e6:pieces%d:", len(pieces))
	info.Write(pieces)
	info.WriteString("e")
	var root bytes.Buffer
	root.WriteString("d8:announce12:http://a.com4:info")
	root.Write(info.Bytes())
	root.WriteString("e")
	torrent := root.Bytes()
	meta, err := qbittorrent.ParseMetainfo(torrent)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Files) != len(files) {
		t.Fatal(meta.Files)
	}
	for i, f := range files {
		if meta.Files[i].Path != "album/"+f.name || meta.Files[i].Length != int64(len(f.data)) {
			t.Fatal(meta.Files[i])
		}
	}
	return torrent
}

// fakeQBTorrent is one torrent held by the fake qBittorrent server.
type fakeQBTorrent struct {
	hash         string
	savePath     string
	downloadPath string
	category     string
	tags         string
	started      bool
	paused       bool
}

// fakeQBT is an in-process qBittorrent Web API that really stores the
// torrent files on disk: adding plants the files under the save path,
// starting flips the state to downloading and then to complete after
// completeAfter info calls, and setLocation really renames the save
// directory. It counts every add so a test can prove a retry resumed
// instead of adding twice.
type fakeQBT struct {
	t             *testing.T
	server        *httptest.Server
	mu            sync.Mutex
	addCalls      int
	files         map[string][]byte
	row           *fakeQBTorrent
	fileNames     []string
	infoCalls     int
	completeAfter int
	rejectLogin   bool
	pausedCalls   int
	deleteCalls   int
	recheckCalls  int
	deleteFiles   bool
}

func newFakeQBT(t *testing.T, files map[string][]byte, completeAfter int) *fakeQBT {
	t.Helper()
	fake := &fakeQBT{t: t, files: files, completeAfter: completeAfter}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeQBT) url() string { return f.server.URL }

func (f *fakeQBT) addCallsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addCalls
}

func (f *fakeQBT) setCompleteAfter(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completeAfter = n
}

func (f *fakeQBT) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/v2/auth/login":
		if f.rejectLogin {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if err := r.ParseForm(); err != nil || r.Form.Get("username") == "" || r.Form.Get("password") == "" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "fake-sid"})
		io.WriteString(w, "Ok.")
	case r.URL.Path == "/api/v2/app/version":
		io.WriteString(w, "5.0.0")
	case r.URL.Path == "/api/v2/app/webapiVersion":
		io.WriteString(w, "2.11.0")
	case r.URL.Path == "/api/v2/app/preferences":
		io.WriteString(w, `{"save_path":"/downloads"}`)
	case r.URL.Path == "/api/v2/torrents/add":
		f.addTorrent(w, r)
	case r.URL.Path == "/api/v2/torrents/info":
		f.info(w, r)
	case r.URL.Path == "/api/v2/torrents/files":
		f.fileList(w, r)
	case r.URL.Path == "/api/v2/torrents/start" || r.URL.Path == "/api/v2/torrents/resume":
		f.mu.Lock()
		if f.row != nil {
			f.row.started = true
			f.row.paused = false
		}
		f.mu.Unlock()
	case r.URL.Path == "/api/v2/torrents/stop" || r.URL.Path == "/api/v2/torrents/pause":
		f.mu.Lock()
		f.pausedCalls++
		if f.row != nil {
			f.row.paused = true
		}
		f.mu.Unlock()
	case r.URL.Path == "/api/v2/torrents/delete":
		f.mu.Lock()
		f.deleteCalls++
		f.deleteFiles = r.FormValue("deleteFiles") == "true"
		f.row = nil
		f.mu.Unlock()
	case r.URL.Path == "/api/v2/torrents/recheck":
		f.mu.Lock()
		f.recheckCalls++
		f.mu.Unlock()
	case r.URL.Path == "/api/v2/torrents/setLocation":
		f.setLocation(w, r)
	case r.URL.Path == "/api/v2/torrents/setDownloadPath":
		f.mu.Lock()
		if f.row != nil {
			f.row.downloadPath = r.FormValue("path")
		}
		f.mu.Unlock()
	case r.URL.Path == "/api/v2/torrents/addTags":
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (f *fakeQBT) addTorrent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	part, _, err := r.FormFile("torrents")
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	defer part.Close()
	torrent, err := io.ReadAll(part)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	meta, err := qbittorrent.ParseMetainfo(torrent)
	if err != nil {
		http.Error(w, "bad torrent", http.StatusBadRequest)
		return
	}
	savePath := r.FormValue("savepath")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addCalls++
	if f.row != nil {
		// The executor must never add twice; count the call and keep the
		// first row so the test can see the duplicate.
		return
	}
	for _, file := range meta.Files {
		content, ok := f.files[file.Path]
		if !ok {
			http.Error(w, "unknown file", http.StatusBadRequest)
			return
		}
		target := filepath.Join(savePath, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			http.Error(w, "cannot plant file", http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(target, content, 0o600); err != nil {
			http.Error(w, "cannot plant file", http.StatusInternalServerError)
			return
		}
	}
	f.row = &fakeQBTorrent{
		hash:         meta.InfoHashV1,
		savePath:     savePath,
		downloadPath: filepath.Join(savePath, ".incomplete"),
		category:     r.FormValue("category"),
		tags:         r.FormValue("tags"),
	}
	f.fileNames = nil
	for _, file := range meta.Files {
		f.fileNames = append(f.fileNames, file.Path)
	}
}

func (f *fakeQBT) rowJSON() map[string]any {
	row := f.row
	state, progress := "stoppedDL", 0.0
	if row.started {
		f.infoCalls++
		switch {
		case row.paused:
			state, progress = "pausedDL", 0.5
		case f.infoCalls >= f.completeAfter:
			state, progress = "uploading", 1.0
		default:
			state, progress = "downloading", 0.5
		}
	}
	return map[string]any{
		"hash": row.hash, "infohash_v1": row.hash, "infohash_v2": "", "name": "album",
		"state": state, "progress": progress, "category": row.category, "tags": row.tags,
		"save_path": row.savePath, "content_path": filepath.Join(row.savePath, "album"),
		"download_path": row.downloadPath, "auto_tmm": false, "size": 0,
	}
}

func (f *fakeQBT) info(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := []map[string]any{}
	if f.row != nil {
		hashes := r.URL.Query().Get("hashes")
		if hashes == "" || strings.Contains(hashes, f.row.hash) {
			rows = append(rows, f.rowJSON())
		}
	}
	writeJSON(w, http.StatusOK, rows)
}

func (f *fakeQBT) fileList(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []map[string]any{}
	progress := 0.0
	if f.row != nil && f.row.started && f.infoCalls >= f.completeAfter {
		progress = 1.0
	}
	for _, name := range f.fileNames {
		out = append(out, map[string]any{"name": name, "size": len(f.files[name]), "progress": progress, "priority": 1})
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *fakeQBT) setLocation(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.row == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	location := r.FormValue("location")
	// A real client moves the torrent into the target; an empty target
	// directory left by an earlier attempt is removed first, like a
	// client that merges into an existing directory.
	if info, err := os.Stat(location); err == nil && info.IsDir() {
		if entries, err := os.ReadDir(location); err == nil && len(entries) == 0 {
			os.Remove(location)
		}
	}
	if err := os.Rename(f.row.savePath, location); err != nil {
		f.t.Logf("setLocation rename failed: %v (from %q to %q)", err, f.row.savePath, location)
		http.Error(w, "cannot move", http.StatusInternalServerError)
		return
	}
	// Only the save path moves here: the download path stays at the stale
	// old location until the executor calls setDownloadPath, so the test
	// proves that call happened instead of the move implying it.
	f.row.savePath = location
}

// downloadTracker serves one free open.cd hit whose torrent is the given
// bytes, following the fake-tracker pattern of the free-download test.
func downloadTracker(t *testing.T, torrent []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/download.php") {
			w.Write(torrent)
			return
		}
		if r.URL.Path == "/details.php" {
			io.WriteString(w, `<tr id="torrent11"><td><strong class="tl_free">免费</strong></td></tr>`)
			return
		}
		io.WriteString(w, `<html><a href="logout.php">logout</a><table><tr><td><table><tr><td><a href="details.php?id=11" title="Album">Album</a></td></tr></table></td><td>100 MiB</td></tr></table></html>`)
	}))
	t.Cleanup(server.Close)
	return server
}

// candidateJob drives one search through the fake tracker until its process
// job is a needs_review candidate with a stored torrent receipt.
func candidateJob(t *testing.T, application *App, jobID string, torrent []byte) workflow.Job {
	t.Helper()
	tracker := downloadTracker(t, torrent)
	body := searchBody(jobID, tracker.URL, []string{"open.cd"}, false)
	body["expected_title"] = "Album"
	body["free_confirmed"] = false
	post(t, application, "/v1/search", body)
	waitJob(t, application, "search:"+jobID, "completed")
	return waitJob(t, application, "process:search:"+jobID, "needs_review")
}

// postDownload calls the download handler through a mux with the real
// route pattern (http.go is wired by the parent; the handler itself is
// exercised here without it).
func postDownload(t *testing.T, application *App, id string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, "/v1/jobs/"+id+"/download", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/jobs/{id}/download", application.downloadJob)
	mux.ServeHTTP(recorder, request)
	return recorder
}

func downloadBody(baseURL, password string) map[string]any {
	return map[string]any{"base_url": baseURL, "username": "qb", "password": password}
}

func countRegularFiles(t *testing.T, dir string) int {
	t.Helper()
	count := 0
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.Mode().IsRegular() {
			count++
		}
		return nil
	})
	return count
}

// waitRootPublished waits for the root job's publish task (created by
// RecordRelease) to complete and the root job to reach completed: the
// process job finishes terminally in finishValidating while the publish
// verification runs on the root, so waiting only for the process job
// would not prove the release was verified.
func waitRootPublished(t *testing.T, application *App, rootID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		task, err := application.tasks.Get("stage:" + rootID + ":publishing")
		if err == nil && task != nil && task.State == "completed" {
			root := inspect(t, application, rootID)
			if root.Job.Stage == "completed" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("root publish did not complete for", rootID)
}

func TestDownloadRecordIDIsStableAndSafe(t *testing.T) {
	first := downloadRecordID("process:search:album-dl")
	if first != downloadRecordID("process:search:album-dl") {
		t.Fatal("record id is not deterministic")
	}
	if strings.ContainsAny(first, ":") || len(first) > 120 {
		t.Fatal(first)
	}
	second := downloadRecordID("process:search:album-other")
	if first == second {
		t.Fatal("different jobs share a record id")
	}
	if _, err := qbittorrent.NewRecord(first, t.TempDir(), "musicops"); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadExecutorFullChain(t *testing.T) {
	wav1, wav2 := pcmWAV(100), pcmWAV(200)
	torrent := buildTestTorrent(t, []testTorrentFile{{"01.wav", wav1}, {"02.wav", wav2}})
	application := newApp(t)
	job := candidateJob(t, application, "album-dl", torrent)
	if job.ReviewKind != "candidate" {
		t.Fatal(job)
	}
	fake := newFakeQBT(t, map[string][]byte{"album/01.wav": wav1, "album/02.wav": wav2}, 3)
	interval := downloadPollInterval
	downloadPollInterval = 50 * time.Millisecond
	t.Cleanup(func() { downloadPollInterval = interval })
	recorder := postDownload(t, application, job.ID, downloadBody(fake.url(), "secret-pass"))
	if recorder.Code != http.StatusAccepted {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	var started map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &started); err != nil || started["stage"] != "downloading" {
		t.Fatal(recorder.Body.String())
	}
	done := waitJob(t, application, job.ID, "completed")
	if done.Stage != "completed" {
		t.Fatal(done)
	}
	waitRootPublished(t, application, job.RootID)
	library, err := filepath.Glob(filepath.Join(application.Dir, "library-audio", "*", "*", "*.flac"))
	if err != nil || len(library) != 2 {
		t.Fatal(library, err)
	}
	seeds, err := filepath.Glob(filepath.Join(application.Dir, "seeds", "*", "album", "*.wav"))
	if err != nil || len(seeds) != 2 {
		t.Fatal(seeds, err)
	}
	if countRegularFiles(t, filepath.Join(application.Dir, "downloads")) != 0 {
		t.Fatal("old download directory still holds files")
	}
	if fake.addCallsCount() != 1 {
		t.Fatal("torrent added more than once", fake.addCallsCount())
	}
	if dirContains(t, application.Dir, "secret-pass") {
		t.Fatal("qBittorrent password was written to disk")
	}
	raw, found, err := application.tasks.Data(workflow.DownloadTaskID(job.ID), "arguments", 0)
	if err != nil || !found || bytes.Contains(raw, []byte("secret-pass")) {
		t.Fatal("download plan carries the password", found, err)
	}
}

func TestDownloadTimeoutResumesWithoutAddingTwice(t *testing.T) {
	wav1, wav2 := pcmWAV(100), pcmWAV(200)
	torrent := buildTestTorrent(t, []testTorrentFile{{"01.wav", wav1}, {"02.wav", wav2}})
	application := newApp(t)
	job := candidateJob(t, application, "album-resume", torrent)
	fake := newFakeQBT(t, map[string][]byte{"album/01.wav": wav1, "album/02.wav": wav2}, 1<<30)
	interval, timeout := downloadPollInterval, downloadPollTimeout
	downloadPollInterval, downloadPollTimeout = 50*time.Millisecond, 600*time.Millisecond
	t.Cleanup(func() { downloadPollInterval, downloadPollTimeout = interval, timeout })
	if recorder := postDownload(t, application, job.ID, downloadBody(fake.url(), "secret-pass")); recorder.Code != http.StatusAccepted {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	failed := waitJob(t, application, job.ID, "failed")
	if failed.ErrorClass != "DOWNLOAD_TIMEOUT" {
		t.Fatal(failed)
	}
	if fake.addCallsCount() != 1 {
		t.Fatal("first attempt added more than once", fake.addCallsCount())
	}
	fake.setCompleteAfter(0)
	if recorder := postDownload(t, application, job.ID, downloadBody(fake.url(), "secret-pass")); recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	done := waitJob(t, application, job.ID, "completed")
	if done.Stage != "completed" {
		t.Fatal(done)
	}
	waitRootPublished(t, application, job.RootID)
	if fake.addCallsCount() != 1 {
		t.Fatal("retry added the torrent again", fake.addCallsCount())
	}
	if countRegularFiles(t, filepath.Join(application.Dir, "downloads")) != 0 {
		t.Fatal("old download directory still holds files")
	}
}

func TestDownloadJobHandlerMatrix(t *testing.T) {
	wav1, wav2 := pcmWAV(100), pcmWAV(200)
	torrent := buildTestTorrent(t, []testTorrentFile{{"01.wav", wav1}, {"02.wav", wav2}})
	application := newApp(t)
	job := candidateJob(t, application, "album-matrix", torrent)
	fake := newFakeQBT(t, map[string][]byte{"album/01.wav": wav1, "album/02.wav": wav2}, 1<<30)
	interval, timeout := downloadPollInterval, downloadPollTimeout
	downloadPollInterval, downloadPollTimeout = 50*time.Millisecond, 600*time.Millisecond
	t.Cleanup(func() { downloadPollInterval, downloadPollTimeout = interval, timeout })

	if recorder := postDownload(t, application, "no-such-job", downloadBody(fake.url(), "secret-pass")); recorder.Code != http.StatusNotFound {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	if recorder := postDownload(t, application, "search:album-matrix", downloadBody(fake.url(), "secret-pass")); recorder.Code != http.StatusConflict {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	bad := downloadBody(fake.url(), "secret-pass")
	bad["base_url"] = "not a url"
	if recorder := postDownload(t, application, job.ID, bad); recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	bad = downloadBody(fake.url(), "")
	if recorder := postDownload(t, application, job.ID, bad); recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	bad = downloadBody(fake.url(), "secret-pass")
	bad["save_path"] = "relative/path"
	if recorder := postDownload(t, application, job.ID, bad); recorder.Code != http.StatusBadRequest {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	if recorder := postDownload(t, application, job.ID, downloadBody(fake.url(), "secret-pass")); recorder.Code != http.StatusAccepted {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	if recorder := postDownload(t, application, job.ID, downloadBody(fake.url(), "secret-pass")); recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	failed := waitJob(t, application, job.ID, "failed")
	if failed.ErrorClass != "DOWNLOAD_TIMEOUT" {
		t.Fatal(failed)
	}
}

func TestDownloadMissingCredentialsFailsHonestly(t *testing.T) {
	wav1, wav2 := pcmWAV(100), pcmWAV(200)
	torrent := buildTestTorrent(t, []testTorrentFile{{"01.wav", wav1}, {"02.wav", wav2}})
	application := newApp(t)
	job := candidateJob(t, application, "album-nocreds", torrent)
	plan, err := application.buildDownloadPlan(job, "", "musicops", "MusicOps")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.flows.StartDownload("download-test-nocreds", job.ID, workflow.DownloadInput{
		Plan: encoded, Site: plan.Site, ResourceID: plan.ResourceID, InfoHash: plan.InfoHash, FreeConfirmed: plan.FreeConfirmed,
	}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	failed := waitJob(t, application, job.ID, "failed")
	if failed.ErrorClass != "MISSING_CREDENTIALS" {
		t.Fatal(failed)
	}
}

func TestDownloadAuthFailureFailsHonestly(t *testing.T) {
	wav1, wav2 := pcmWAV(100), pcmWAV(200)
	torrent := buildTestTorrent(t, []testTorrentFile{{"01.wav", wav1}, {"02.wav", wav2}})
	application := newApp(t)
	job := candidateJob(t, application, "album-authfail", torrent)
	fake := newFakeQBT(t, map[string][]byte{"album/01.wav": wav1, "album/02.wav": wav2}, 3)
	fake.rejectLogin = true
	if recorder := postDownload(t, application, job.ID, downloadBody(fake.url(), "secret-pass")); recorder.Code != http.StatusAccepted {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	failed := waitJob(t, application, job.ID, "failed")
	if failed.ErrorClass != "AUTH_FAILED" {
		t.Fatal(failed)
	}
	if fake.addCallsCount() != 0 {
		t.Fatal("torrent added before login", fake.addCallsCount())
	}
}

func TestDownloadArchiveRefusesExistingTarget(t *testing.T) {
	wav1, wav2 := pcmWAV(100), pcmWAV(200)
	torrent := buildTestTorrent(t, []testTorrentFile{{"01.wav", wav1}, {"02.wav", wav2}})
	application := newApp(t)
	job := candidateJob(t, application, "album-arch", torrent)
	taskID := downloadRecordID(job.ID)
	target := filepath.Join(application.Dir, "seeds", taskID, "album", "01.wav")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("someone else's file"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := newFakeQBT(t, map[string][]byte{"album/01.wav": wav1, "album/02.wav": wav2}, 3)
	interval := downloadPollInterval
	downloadPollInterval = 50 * time.Millisecond
	t.Cleanup(func() { downloadPollInterval = interval })
	if recorder := postDownload(t, application, job.ID, downloadBody(fake.url(), "secret-pass")); recorder.Code != http.StatusAccepted {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	failed := waitJob(t, application, job.ID, "failed")
	if failed.ErrorClass != "ARCHIVE_FAILED" {
		t.Fatal(failed)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, []byte("someone else's file")) {
		t.Fatal("existing seed file was overwritten", err)
	}
}

func TestDownloadUnreachableClientFailsHonestly(t *testing.T) {
	wav1, wav2 := pcmWAV(100), pcmWAV(200)
	torrent := buildTestTorrent(t, []testTorrentFile{{"01.wav", wav1}, {"02.wav", wav2}})
	application := newApp(t)
	job := candidateJob(t, application, "album-down", torrent)
	unreachable := (&url.URL{Scheme: "http", Host: "127.0.0.1:1"}).String()
	if recorder := postDownload(t, application, job.ID, downloadBody(unreachable, "secret-pass")); recorder.Code != http.StatusAccepted {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	failed := waitJob(t, application, job.ID, "failed")
	if failed.ErrorClass != "QBITTORENT_UNAVAILABLE" {
		t.Fatal(failed)
	}
}

// TestDownloadAcceptanceOverHTTP is the slice-5 acceptance walk over a real
// listening HTTP server (not the in-process handler): search, the acquire
// storing the torrent, POST download, polling the process job through
// downloading and validating to completed, and the root job through
// publishing to completed with the release verified. The download route is
// wired here because http.go is wired by the parent; everything else goes
// through the app's own handler, behind the same basic auth.
func TestDownloadAcceptanceOverHTTP(t *testing.T) {
	wav1, wav2 := pcmWAV(100), pcmWAV(200)
	torrent := buildTestTorrent(t, []testTorrentFile{{"01.wav", wav1}, {"02.wav", wav2}})
	application := newApp(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/jobs/{id}/download", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "musicops" || pass != "test-password-16b" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		application.downloadJob(w, r)
	})
	mux.Handle("/", application.Handler())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	fake := newFakeQBT(t, map[string][]byte{"album/01.wav": wav1, "album/02.wav": wav2}, 3)
	interval := downloadPollInterval
	downloadPollInterval = 50 * time.Millisecond
	t.Cleanup(func() { downloadPollInterval = interval })
	client := &http.Client{Timeout: 5 * time.Second}
	do := func(method, path string, body map[string]any) (int, map[string]any) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			reader = bytes.NewReader(raw)
		}
		request, err := http.NewRequest(method, server.URL+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		request.SetBasicAuth("musicops", "test-password-16b")
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var decoded map[string]any
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, decoded
	}
	jobStage := func(id string) string {
		t.Helper()
		_, decoded := do(http.MethodGet, "/v1/jobs/"+id, nil)
		inspection, _ := decoded["inspection"].(map[string]any)
		job, _ := inspection["job"].(map[string]any)
		stage, _ := job["stage"].(string)
		return stage
	}
	waitStage := func(id string, stages ...string) string {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		last := ""
		for time.Now().Before(deadline) {
			stage := jobStage(id)
			if stage != last {
				t.Logf("job %s -> %s", id, stage)
				last = stage
			}
			for _, want := range stages {
				if stage == want {
					return stage
				}
			}
			if stage == "failed" {
				t.Fatal("job", id, "failed while waiting for", stages)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("job", id, "did not reach", stages, "last stage", last)
		return ""
	}

	tracker := downloadTracker(t, torrent)
	body := searchBody("album-accept", tracker.URL, []string{"open.cd"}, false)
	body["expected_title"] = "Album"
	body["free_confirmed"] = false
	if code, _ := do(http.MethodPost, "/v1/search", body); code != http.StatusAccepted {
		t.Fatal("search status", code)
	}
	waitStage("search:album-accept", "completed")
	waitStage("process:search:album-accept", "needs_review")
	matches, err := filepath.Glob(filepath.Join(application.Dir, "torrents", "*.torrent"))
	if err != nil || len(matches) != 1 {
		t.Fatal("torrent was not stored", matches, err)
	}
	code, started := do(http.MethodPost, "/v1/jobs/process:search:album-accept/download", downloadBody(fake.url(), "secret-pass"))
	if code != http.StatusAccepted || started["stage"] != "downloading" {
		t.Fatal("download status", code, started)
	}
	waitStage("process:search:album-accept", "downloading", "validating", "completed")
	waitStage("process:search:album-accept", "completed")
	waitStage("album-accept", "publishing", "completed")
	waitStage("album-accept", "completed")
	library, err := filepath.Glob(filepath.Join(application.Dir, "library-audio", "*", "*", "*.flac"))
	if err != nil || len(library) != 2 {
		t.Fatal(library, err)
	}
	seeds, err := filepath.Glob(filepath.Join(application.Dir, "seeds", "*", "album", "*.wav"))
	if err != nil || len(seeds) != 2 {
		t.Fatal(seeds, err)
	}
	if countRegularFiles(t, filepath.Join(application.Dir, "downloads")) != 0 {
		t.Fatal("old download directory still holds files")
	}
	if fake.addCallsCount() != 1 {
		t.Fatal("torrent added more than once", fake.addCallsCount())
	}
	if dirContains(t, application.Dir, "secret-pass") {
		t.Fatal("qBittorrent password was written to disk")
	}
}
