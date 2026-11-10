package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- bencode test helpers -------------------------------------------------

func benc(v any) []byte {
	switch v := v.(type) {
	case []byte:
		return []byte(strconv.Itoa(len(v)) + ":" + string(v))
	case string:
		return benc([]byte(v))
	case int:
		return []byte("i" + strconv.Itoa(v) + "e")
	case int64:
		return []byte("i" + strconv.FormatInt(v, 10) + "e")
	case []any:
		out := []byte("l")
		for _, item := range v {
			out = append(out, benc(item)...)
		}
		return append(out, 'e')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := []byte("d")
		for _, k := range keys {
			out = append(out, benc(k)...)
			out = append(out, benc(v[k])...)
		}
		return append(out, 'e')
	default:
		panic("unsupported bencode test value")
	}
}

func buildV1Single() []byte {
	info := map[string]any{
		"length":       int64(100),
		"name":         "song.flac",
		"piece length": int64(16384),
		"pieces":       make([]byte, 20),
	}
	return benc(map[string]any{"info": info})
}

func buildV1Multi() []byte {
	files := []any{
		map[string]any{"length": int64(100), "path": []any{"01.flac"}},
		map[string]any{"length": int64(200), "path": []any{"02.flac"}},
	}
	info := map[string]any{
		"files":        files,
		"name":         "album",
		"piece length": int64(16384),
		"pieces":       make([]byte, 20),
	}
	return benc(map[string]any{"info": info})
}

func buildV1MultiWithPadding() []byte {
	files := []any{
		map[string]any{"length": int64(100), "path": []any{"01.flac"}},
		map[string]any{"length": int64(200), "path": []any{"02.flac"}},
		map[string]any{"length": int64(16384), "path": []any{"pad"}, "attr": []byte("p")},
	}
	// Total 16684 bytes -> 2 pieces.
	info := map[string]any{
		"files":        files,
		"name":         "album",
		"piece length": int64(16384),
		"pieces":       make([]byte, 40),
	}
	return benc(map[string]any{"info": info})
}

func buildV2() []byte {
	root := make([]byte, 32)
	for i := range root {
		root[i] = 7
	}
	tree := map[string]any{
		"song.flac": map[string]any{
			"": map[string]any{"length": int64(50), "pieces root": root},
		},
	}
	info := map[string]any{
		"file tree":    tree,
		"meta version": int64(2),
		"name":         "album",
		"piece length": int64(16384),
	}
	return benc(map[string]any{"info": info, "piece layers": map[string]any{}})
}

func buildHybrid() []byte {
	root1 := make([]byte, 32)
	root2 := make([]byte, 32)
	for i := range root1 {
		root1[i] = 1
		root2[i] = 2
	}
	tree := map[string]any{
		"01.flac": map[string]any{
			"": map[string]any{"length": int64(100), "pieces root": root1},
		},
		"02.flac": map[string]any{
			"": map[string]any{"length": int64(200), "pieces root": root2},
		},
	}
	files := []any{
		map[string]any{"length": int64(100), "path": []any{"01.flac"}},
		map[string]any{"length": int64(200), "path": []any{"02.flac"}},
	}
	info := map[string]any{
		"files":        files,
		"file tree":    tree,
		"meta version": int64(2),
		"name":         "album",
		"piece length": int64(16384),
		"pieces":       make([]byte, 20),
	}
	return benc(map[string]any{"info": info, "piece layers": map[string]any{}})
}

// --- fake qBittorrent server ----------------------------------------------

type addCapture struct {
	fields      map[string]string
	fileName    string
	fileBytes   []byte
	fileContent string
}

type fakeTorrent struct {
	hash         string
	state        string
	progress     float64
	files        []TorrentFile
	savePath     string
	downloadPath string
	contentPath  string
	category     string
	tags         string
	autoTMM      bool
}

type fakeQBT struct {
	mu               sync.Mutex
	server           *httptest.Server
	version          string
	webAPIVersion    string
	torrents         map[string]*fakeTorrent
	addCaptures      []addCapture
	deletedTags      []string
	startEndpoints   []string
	setLocations     [][2]string
	setDownloadPaths [][2]string
	failInfoStatus   int
}

func newFakeQBT(t *testing.T, version string) *fakeQBT {
	t.Helper()
	f := &fakeQBT{
		version:       version,
		webAPIVersion: "2.11.3",
		torrents:      map[string]*fakeTorrent{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", f.handleLogin)
	mux.HandleFunc("/api/v2/app/version", f.handleVersion)
	mux.HandleFunc("/api/v2/app/webapiVersion", f.handleWebAPIVersion)
	mux.HandleFunc("/api/v2/torrents/add", f.handleAdd)
	mux.HandleFunc("/api/v2/torrents/info", f.handleInfo)
	mux.HandleFunc("/api/v2/torrents/files", f.handleFiles)
	mux.HandleFunc("/api/v2/torrents/start", f.handleStart)
	mux.HandleFunc("/api/v2/torrents/resume", f.handleStart)
	mux.HandleFunc("/api/v2/torrents/setLocation", f.handleSetLocation)
	mux.HandleFunc("/api/v2/torrents/setDownloadPath", f.handleSetDownloadPath)
	mux.HandleFunc("/api/v2/torrents/addTags", f.handleAddTags)
	mux.HandleFunc("/api/v2/torrents/removeTags", f.handleRemoveTags)
	mux.HandleFunc("/api/v2/torrents/deleteTags", f.handleDeleteTags)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeQBT) authed(r *http.Request) bool {
	cookie, err := r.Cookie("SID")
	return err == nil && cookie.Value == "fake-sid"
}

func (f *fakeQBT) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if !f.authed(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func (f *fakeQBT) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if r.FormValue("username") == "user" && r.FormValue("password") == "correct-pass" {
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "fake-sid", Path: "/"})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Ok."))
		return
	}
	http.Error(w, "Fails.", http.StatusForbidden)
}

func (f *fakeQBT) handleVersion(w http.ResponseWriter, r *http.Request) {
	if !f.requireAuth(w, r) {
		return
	}
	_, _ = w.Write([]byte(f.version))
}

func (f *fakeQBT) handleWebAPIVersion(w http.ResponseWriter, r *http.Request) {
	if !f.requireAuth(w, r) {
		return
	}
	_, _ = w.Write([]byte(f.webAPIVersion))
}

func (f *fakeQBT) handleAdd(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.requireAuth(w, r) {
		return
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, "bad multipart", http.StatusBadRequest)
		return
	}
	capture := addCapture{fields: map[string]string{}}
	for k, v := range r.MultipartForm.Value {
		if len(v) > 0 {
			capture.fields[k] = v[0]
		}
	}
	uploads := r.MultipartForm.File["torrents"]
	if len(uploads) != 1 {
		http.Error(w, "missing torrents file", http.StatusBadRequest)
		return
	}
	capture.fileName = uploads[0].Filename
	capture.fileContent = uploads[0].Header.Get("Content-Type")
	file, err := uploads[0].Open()
	if err != nil {
		http.Error(w, "open", http.StatusBadRequest)
		return
	}
	defer file.Close()
	capture.fileBytes, err = io.ReadAll(file)
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	f.addCaptures = append(f.addCaptures, capture)
	meta, err := ParseMetainfo(capture.fileBytes)
	if err != nil {
		http.Error(w, "bad torrent", http.StatusBadRequest)
		return
	}
	hash := meta.InfoHashV1
	if hash == "" {
		hash = meta.InfoHashV2[:40]
	}
	files := make([]TorrentFile, 0, len(meta.Files))
	for _, mf := range meta.Files {
		files = append(files, TorrentFile{Name: mf.Path, Size: mf.Length, Progress: 0, Priority: 1})
	}
	savePath := capture.fields["savepath"]
	// A real server reports the stopped state matching the stop parameter
	// the client sent: pausedDL on 4.x, stoppedDL on 5.x.
	state := "stoppedDL"
	if _, ok := capture.fields["paused"]; ok {
		state = "pausedDL"
	}
	f.torrents[hash] = &fakeTorrent{
		hash:         hash,
		state:        state,
		progress:     0,
		files:        files,
		savePath:     savePath,
		downloadPath: capture.fields["downloadPath"],
		contentPath:  savePath,
		category:     capture.fields["category"],
		tags:         capture.fields["tags"],
		autoTMM:      capture.fields["autoTMM"] == "true",
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Ok."))
}

func (f *fakeQBT) rowsLocked() []TorrentRow {
	hashes := make([]string, 0, len(f.torrents))
	for h := range f.torrents {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	rows := make([]TorrentRow, 0, len(hashes))
	for _, h := range hashes {
		t := f.torrents[h]
		rows = append(rows, TorrentRow{
			Hash: t.hash, InfoHashV1: t.hash, State: t.state, Progress: t.progress,
			Category: t.category, Tags: t.tags, SavePath: t.savePath,
			ContentPath: t.contentPath, DownloadPath: t.downloadPath,
			AutoTMM: boolPtr(t.autoTMM),
		})
	}
	return rows
}

func boolPtr(b bool) *bool { return &b }

func (f *fakeQBT) handleInfo(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.requireAuth(w, r) {
		return
	}
	if f.failInfoStatus != 0 {
		http.Error(w, "boom", f.failInfoStatus)
		return
	}
	rows := f.rowsLocked()
	if hashes := r.URL.Query().Get("hashes"); hashes != "" {
		want := map[string]bool{}
		for _, h := range strings.Split(hashes, "|") {
			want[strings.ToLower(h)] = true
		}
		filtered := []TorrentRow{}
		for _, row := range rows {
			if want[strings.ToLower(row.Hash)] {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	if tag := r.URL.Query().Get("tag"); tag != "" {
		kept := []TorrentRow{}
		for _, row := range rows {
			if parseTags(row.Tags)[tag] {
				kept = append(kept, row)
			}
		}
		rows = kept
	}
	if rows == nil {
		rows = []TorrentRow{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = jsonEncode(w, rows)
}

func (f *fakeQBT) handleFiles(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.requireAuth(w, r) {
		return
	}
	t, ok := f.torrents[r.URL.Query().Get("hash")]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = jsonEncode(w, t.files)
}

func (f *fakeQBT) handleStart(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.requireAuth(w, r) {
		return
	}
	f.startEndpoints = append(f.startEndpoints, r.URL.Path)
	hash := r.FormValue("hashes")
	if t, ok := f.torrents[hash]; ok {
		t.state = "downloading"
		t.progress = 0.4
	}
	w.WriteHeader(http.StatusOK)
}

func (f *fakeQBT) handleSetLocation(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.requireAuth(w, r) {
		return
	}
	hash, location := r.FormValue("hashes"), r.FormValue("location")
	f.setLocations = append(f.setLocations, [2]string{hash, location})
	if t, ok := f.torrents[hash]; ok {
		t.savePath = location
		t.contentPath = location
	}
	w.WriteHeader(http.StatusOK)
}

func (f *fakeQBT) handleSetDownloadPath(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.requireAuth(w, r) {
		return
	}
	id, path := r.FormValue("id"), r.FormValue("path")
	f.setDownloadPaths = append(f.setDownloadPaths, [2]string{id, path})
	if t, ok := f.torrents[id]; ok {
		t.downloadPath = path
	}
	w.WriteHeader(http.StatusOK)
}

func (f *fakeQBT) updateTags(hash, tags string, add bool) {
	t, ok := f.torrents[hash]
	if !ok {
		return
	}
	current := parseTags(t.tags)
	for _, tag := range strings.Split(tags, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if add {
			current[tag] = true
		} else {
			delete(current, tag)
		}
	}
	names := make([]string, 0, len(current))
	for tag := range current {
		names = append(names, tag)
	}
	sort.Strings(names)
	t.tags = strings.Join(names, ",")
}

func (f *fakeQBT) handleAddTags(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.requireAuth(w, r) {
		return
	}
	f.updateTags(r.FormValue("hashes"), r.FormValue("tags"), true)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeQBT) handleRemoveTags(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.requireAuth(w, r) {
		return
	}
	f.updateTags(r.FormValue("hashes"), r.FormValue("tags"), false)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeQBT) handleDeleteTags(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.requireAuth(w, r) {
		return
	}
	f.deletedTags = append(f.deletedTags, r.FormValue("tags"))
	w.WriteHeader(http.StatusOK)
}

// setState moves a fake torrent to a lifecycle state for poll tests.
func (f *fakeQBT) setState(hash, state string, progress float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.torrents[hash]; ok {
		t.state = state
		t.progress = progress
	}
}

// setFileProgress sets every fake file's progress (complete) or leaves the
// first file partial.
func (f *fakeQBT) setFileProgress(hash string, progress float64, partialFirst bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.torrents[hash]; ok {
		for i := range t.files {
			t.files[i].Progress = progress
		}
		if partialFirst && len(t.files) > 0 {
			t.files[0].Progress = 0.5
		}
	}
}

// seed inserts a torrent directly, e.g. a second torrent sharing a tag.
func (f *fakeQBT) seed(t *fakeTorrent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.torrents[t.hash] = t
}

func jsonEncode(w http.ResponseWriter, v any) error {
	return json.NewEncoder(w).Encode(v)
}

// --- client construction helpers ------------------------------------------

func testClient(t *testing.T, fake *fakeQBT, password string) *Client {
	t.Helper()
	client, err := NewClient(Config{BaseURL: fake.server.URL, Username: "user", Password: password})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func loggedInClient(t *testing.T, fake *fakeQBT) *Client {
	t.Helper()
	client := testClient(t, fake, "correct-pass")
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	return client
}

func mustMeta(t *testing.T, data []byte) Metainfo {
	t.Helper()
	meta, err := ParseMetainfo(data)
	if err != nil {
		t.Fatalf("ParseMetainfo: %v", err)
	}
	return meta
}

// testRoot returns an absolute directory for record paths. The fixtures
// used to hardcode Unix roots ("/data"), which are not absolute on
// Windows (filepath.IsAbs needs a volume), so every record creation and
// ownership check failed there. Build all fixture paths from t.TempDir
// instead, with filepath.Join.
func testRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func addStoppedRecord(t *testing.T, client *Client, torrent []byte) Record {
	t.Helper()
	meta := mustMeta(t, torrent)
	rec, err := NewRecord("task-1", testRoot(t), "")
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	evidence := FreeEvidence{Confirmed: true, InfoHash: meta.InfoHashV1, Free: FreePtr(true)}
	state, err := client.AddStopped(context.Background(), &rec, torrent, evidence)
	if err != nil {
		t.Fatalf("AddStopped: %v", err)
	}
	if state != StateStopped {
		t.Fatalf("AddStopped state = %q, want STOPPED", state)
	}
	return rec
}

// --- tests ----------------------------------------------------------------

func TestValidateBaseURL(t *testing.T) {
	valid := map[string]string{
		"http://127.0.0.1:8080":           "http://127.0.0.1:8080",
		"http://localhost:8080/":          "http://localhost:8080",
		"http://192.168.1.10:8080":        "http://192.168.1.10:8080",
		"http://10.0.0.5":                 "http://10.0.0.5",
		"http://172.16.4.2:8080":          "http://172.16.4.2:8080",
		"http://qbittorrent:8080":         "http://qbittorrent:8080",
		"http://host.docker.internal:80":  "http://host.docker.internal:80",
		"http://nas.local:8080":           "http://nas.local:8080",
		"https://qbittorrent.example.com": "https://qbittorrent.example.com",
		"https://8.8.8.8:8080":            "https://8.8.8.8:8080",
	}
	for raw, want := range valid {
		got, err := ValidateBaseURL(raw)
		if err != nil {
			t.Errorf("ValidateBaseURL(%q) error: %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("ValidateBaseURL(%q) = %q, want %q", raw, got, want)
		}
	}
	invalid := []string{
		"",
		"ftp://127.0.0.1:8080",
		"http://user:pass@127.0.0.1:8080",
		"http://127.0.0.1:8080/?x=1",
		"http://127.0.0.1:8080/#frag",
		"http://127.0.0.1:8080/api",
		"http://example.com:8080",
		"http://8.8.8.8:8080",
		"http://[2001:db8::1]:8080",
		"http:///path",
		"http://127.0.0.1:99999",
		"http://127.0.0.1:notaport",
		"http://127.0.0.1:8080 with space",
		"127.0.0.1:8080",
	}
	for _, raw := range invalid {
		if got, err := ValidateBaseURL(raw); err == nil {
			t.Errorf("ValidateBaseURL(%q) = %q, want error", raw, got)
		}
	}
}

func TestLocalServiceHost(t *testing.T) {
	cases := map[string]bool{
		"localhost": true, "127.0.0.1": true, "::1": true,
		"192.168.0.5": true, "10.1.2.3": true, "172.16.0.1": true,
		"172.31.255.255": true, "169.254.1.1": true,
		"8.8.8.8": false, "example.com": false, "2001:db8::1": false,
		"224.0.0.1": false, "0.0.0.0": false,
		"qbittorrent": true, "host.docker.internal": true,
		"nas.local": true, "nas.lan": true, "svc.internal": true,
		"": false,
	}
	for host, want := range cases {
		if got := LocalServiceHost(host); got != want {
			t.Errorf("LocalServiceHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestLoginFailure(t *testing.T) {
	fake := newFakeQBT(t, "5.0.4")
	client := testClient(t, fake, "wrong-pass")
	err := client.Login(context.Background())
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("Login error = %v, want ErrAuthFailed", err)
	}
	if strings.Contains(err.Error(), "wrong-pass") {
		t.Fatalf("error leaks the password: %v", err)
	}
	// A failed login must not authenticate later calls.
	if _, _, err := client.Version(context.Background()); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("Version after failed login = %v, want ErrNotAuthenticated", err)
	}
}

func TestLoginSuccessVersionCapabilities(t *testing.T) {
	fake := newFakeQBT(t, "5.0.4")
	client := loggedInClient(t, fake)
	version, webAPI, err := client.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if version != (Version{Major: 5, Minor: 0, Patch: 4}) {
		t.Fatalf("version = %v, want 5.0.4", version)
	}
	if webAPI != "2.11.3" {
		t.Fatalf("webAPI = %q, want 2.11.3", webAPI)
	}
	caps, err := client.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.Supported || caps.InitialStopParameter != "stopped" || caps.AutoTMM {
		t.Fatalf("caps = %+v, want supported, stopped, autoTMM=false", caps)
	}
}

func TestCapabilitiesByVersion(t *testing.T) {
	for _, tc := range []struct {
		version   string
		stop      string
		supported bool
	}{
		{"4.6.0", "paused", true},
		{"4.6.5", "paused", true},
		{"5.2.2", "stopped", true},
		{"5.3.0", "stopped", false},
		{"4.5.9", "paused", false},
	} {
		fake := newFakeQBT(t, tc.version)
		client := loggedInClient(t, fake)
		caps, err := client.Capabilities(context.Background())
		if tc.supported {
			if err != nil {
				t.Errorf("version %s Capabilities error: %v", tc.version, err)
				continue
			}
			if caps.InitialStopParameter != tc.stop {
				t.Errorf("version %s stop param = %q, want %q", tc.version, caps.InitialStopParameter, tc.stop)
			}
		} else {
			if !errors.Is(err, ErrUnsupportedVersion) {
				t.Errorf("version %s error = %v, want ErrUnsupportedVersion", tc.version, err)
			}
			if caps.Supported {
				t.Errorf("version %s caps.Supported = true", tc.version)
			}
		}
	}
}

func TestAddStoppedRequestParameters(t *testing.T) {
	fake := newFakeQBT(t, "5.0.4")
	client := loggedInClient(t, fake)
	torrent := buildV1Multi()
	meta := mustMeta(t, torrent)
	root := testRoot(t)
	rec, err := NewRecord("task-1", root, "")
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	state, err := client.AddStopped(context.Background(), &rec, torrent,
		FreeEvidence{Confirmed: true, InfoHash: meta.InfoHashV1, Free: FreePtr(true)})
	if err != nil {
		t.Fatalf("AddStopped: %v", err)
	}
	if state != StateStopped {
		t.Fatalf("state = %q, want STOPPED", state)
	}
	if !rec.CreationIntent || rec.ClientID != meta.InfoHashV1 {
		t.Fatalf("record = %+v, want creation intent and client id %s", rec, meta.InfoHashV1)
	}
	if len(fake.addCaptures) != 1 {
		t.Fatalf("add captures = %d, want 1", len(fake.addCaptures))
	}
	cap := fake.addCaptures[0]
	wantFields := map[string]string{
		"savepath":                 filepath.Join(root, "task-1"),
		"category":                 "musicops",
		"tags":                     "MusicOps",
		"autoTMM":                  "false",
		"skip_checking":            "false",
		"contentLayout":            "Original",
		"stopped":                  "true",
		"ratioLimit":               "-1",
		"seedingTimeLimit":         "-1",
		"inactiveSeedingTimeLimit": "-1",
		"downloadPath":             filepath.Join(root, "task-1", ".incomplete"),
		"useDownloadPath":          "true",
	}
	for k, want := range wantFields {
		if got := cap.fields[k]; got != want {
			t.Errorf("multipart field %s = %q, want %q", k, got, want)
		}
	}
	if _, ok := cap.fields["paused"]; ok {
		t.Errorf("v5 add must not send paused field")
	}
	if cap.fileName != "asset.torrent" {
		t.Errorf("file name = %q, want asset.torrent", cap.fileName)
	}
	if cap.fileContent != "application/x-bittorrent" {
		t.Errorf("file content type = %q, want application/x-bittorrent", cap.fileContent)
	}
	if string(cap.fileBytes) != string(torrent) {
		t.Errorf("uploaded torrent bytes differ from input")
	}
}

func TestAddStoppedV4UsesPaused(t *testing.T) {
	fake := newFakeQBT(t, "4.6.5")
	client := loggedInClient(t, fake)
	rec := addStoppedRecord(t, client, buildV1Single())
	_ = rec
	if len(fake.addCaptures) != 1 {
		t.Fatalf("add captures = %d, want 1", len(fake.addCaptures))
	}
	if got := fake.addCaptures[0].fields["paused"]; got != "true" {
		t.Errorf("paused field = %q, want true", got)
	}
	if _, ok := fake.addCaptures[0].fields["stopped"]; ok {
		t.Errorf("v4 add must not send stopped field")
	}
}

func TestAddStoppedRequiresFreeEvidence(t *testing.T) {
	fake := newFakeQBT(t, "5.0.4")
	client := loggedInClient(t, fake)
	torrent := buildV1Single()
	meta := mustMeta(t, torrent)
	rec, _ := NewRecord("task-1", testRoot(t), "")
	errCases := []FreeEvidence{
		{Confirmed: false, InfoHash: meta.InfoHashV1, Free: FreePtr(true)},
		{Confirmed: true, InfoHash: "deadbeef", Free: FreePtr(true)},
		{Confirmed: true, InfoHash: meta.InfoHashV1, Free: FreePtr(false)},
	}
	for i, evidence := range errCases {
		_, err := client.AddStopped(context.Background(), &rec, torrent, evidence)
		if !errors.Is(err, ErrFreeEvidenceMismatch) {
			t.Errorf("case %d error = %v, want ErrFreeEvidenceMismatch", i, err)
		}
	}
	if len(fake.addCaptures) != 0 {
		t.Errorf("add captures = %d, want 0 after rejected evidence", len(fake.addCaptures))
	}
}

func TestFreeEvidenceMatch(t *testing.T) {
	meta := Metainfo{InfoHashV1: "aaa", InfoHashV2: "bbb"}
	cases := []struct {
		name     string
		evidence FreeEvidence
		want     bool
	}{
		{"confirmed free v1", FreeEvidence{true, "aaa", FreePtr(true)}, true},
		{"confirmed free v2", FreeEvidence{true, "bbb", FreePtr(true)}, true},
		{"confirmed free unstated", FreeEvidence{true, "aaa", nil}, true},
		{"unconfirmed", FreeEvidence{false, "aaa", FreePtr(true)}, false},
		{"wrong hash", FreeEvidence{true, "ccc", FreePtr(true)}, false},
		{"empty hash", FreeEvidence{true, "", FreePtr(true)}, false},
		{"non-free has no consent store", FreeEvidence{true, "aaa", FreePtr(false)}, false},
	}
	for _, tc := range cases {
		if got := FreeEvidenceMatch(tc.evidence, meta); got != tc.want {
			t.Errorf("%s: FreeEvidenceMatch = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCheckOwnershipRejections(t *testing.T) {
	fake := newFakeQBT(t, "5.0.4")
	client := loggedInClient(t, fake)
	if _, _, err := client.Version(context.Background()); err != nil {
		t.Fatalf("Version: %v", err)
	}
	root := testRoot(t)
	taskPath := filepath.Join(root, "task-1")
	baseRow := func() *TorrentRow {
		return &TorrentRow{
			Hash: "abc", State: "stoppedDL", Progress: 0, Category: "musicops",
			Tags: "MusicOps", SavePath: taskPath,
			ContentPath: taskPath, DownloadPath: filepath.Join(taskPath, ".incomplete"),
			AutoTMM: boolPtr(false),
		}
	}
	baseRec := Record{
		TaskID: "task-1", ClientID: "abc", SourcePath: taskPath,
		TaskTag: "MusicOps", Category: "musicops", CreationIntent: true,
	}
	if reason := client.CheckOwnership(baseRow(), baseRec, true); reason != "" {
		t.Fatalf("valid row rejected: %s", reason)
	}
	cases := []struct {
		name      string
		mutateRow func(*TorrentRow)
		mutateRec func(*Record)
		stopped   bool
	}{
		{"wrong tag", func(r *TorrentRow) { r.Tags = "other" }, nil, false},
		{"wrong category", func(r *TorrentRow) { r.Category = "other" }, nil, false},
		{"save path outside source", func(r *TorrentRow) { r.SavePath = filepath.Join(root, "other") }, nil, false},
		{"save path sibling prefix", func(r *TorrentRow) { r.SavePath = filepath.Join(root, "task-1-evil") }, nil, false},
		{"autoTMM on", func(r *TorrentRow) { r.AutoTMM = boolPtr(true) }, nil, false},
		{"autoTMM unknown", func(r *TorrentRow) { r.AutoTMM = nil }, nil, false},
		{"download path missing", func(r *TorrentRow) { r.DownloadPath = "" }, nil, false},
		{"download path outside", func(r *TorrentRow) { r.DownloadPath = filepath.Join(root, "outside") }, nil, false},
		{"content path outside", func(r *TorrentRow) { r.ContentPath = filepath.Join(root, "outside") }, nil, false},
		{"no creation intent", nil, func(r *Record) { r.CreationIntent = false }, false},
		{"not stopped", func(r *TorrentRow) { r.State = "downloading" }, nil, true},
	}
	for _, tc := range cases {
		row, rec := baseRow(), baseRec
		if tc.mutateRow != nil {
			tc.mutateRow(row)
		}
		if tc.mutateRec != nil {
			tc.mutateRec(&rec)
		}
		if reason := client.CheckOwnership(row, rec, tc.stopped); reason == "" {
			t.Errorf("%s: accepted, want rejection", tc.name)
		}
	}
	if reason := client.CheckOwnership(nil, baseRec, false); reason == "" {
		t.Errorf("nil row accepted, want rejection")
	}
}

func TestPollLifecycle(t *testing.T) {
	fake := newFakeQBT(t, "5.0.4")
	client := loggedInClient(t, fake)
	torrent := buildV1Multi()
	rec := addStoppedRecord(t, client, torrent)
	meta := mustMeta(t, torrent)
	evidence := FreeEvidence{Confirmed: true, InfoHash: meta.InfoHashV1, Free: FreePtr(true)}
	ctx := context.Background()

	// Added torrent is stopped with progress 0.
	result, err := client.Poll(ctx, &rec, nil)
	if err != nil {
		t.Fatalf("Poll stopped: %v", err)
	}
	if result.Outcome != OutcomeStopped || result.Complete {
		t.Fatalf("Poll stopped = %+v, want stopped, incomplete", result)
	}

	// Start it: v5 must use /torrents/start.
	if err := client.StartDownload(ctx, &rec, evidence); err != nil {
		t.Fatalf("StartDownload: %v", err)
	}
	if len(fake.startEndpoints) != 1 || fake.startEndpoints[0] != "/api/v2/torrents/start" {
		t.Fatalf("start endpoints = %v, want [/api/v2/torrents/start]", fake.startEndpoints)
	}
	result, err = client.Poll(ctx, &rec, nil)
	if err != nil {
		t.Fatalf("Poll downloading: %v", err)
	}
	if result.Outcome != OutcomeDownloading || result.Complete {
		t.Fatalf("Poll downloading = %+v, want downloading, incomplete", result)
	}

	// Finished state and progress 1, but one file still partial.
	fake.setState(rec.ClientID, "uploading", 1)
	fake.setFileProgress(rec.ClientID, 1, true)
	result, err = client.Poll(ctx, &rec, nil)
	if err != nil {
		t.Fatalf("Poll partial: %v", err)
	}
	if result.Outcome != OutcomeDownloading || result.Complete {
		t.Fatalf("Poll partial = %+v, want downloading, incomplete", result)
	}

	// All files complete.
	fake.setFileProgress(rec.ClientID, 1, false)
	result, err = client.Poll(ctx, &rec, nil)
	if err != nil {
		t.Fatalf("Poll complete: %v", err)
	}
	if result.Outcome != OutcomeComplete || !result.Complete {
		t.Fatalf("Poll complete = %+v, want complete", result)
	}
	if rec.State != StateComplete {
		t.Fatalf("record state = %q, want COMPLETE", rec.State)
	}
}

func TestPollBlockedAndWaiting(t *testing.T) {
	fake := newFakeQBT(t, "5.0.4")
	client := loggedInClient(t, fake)
	rec := addStoppedRecord(t, client, buildV1Single())
	ctx := context.Background()
	for state, want := range map[string]PollOutcome{
		"checkingDL":   OutcomeWaiting,
		"moving":       OutcomeWaiting,
		"error":        OutcomeBlocked,
		"missingFiles": OutcomeBlocked,
	} {
		fake.setState(rec.ClientID, state, 0)
		result, err := client.Poll(ctx, &rec, nil)
		if err != nil {
			t.Fatalf("Poll %s: %v", state, err)
		}
		if result.Outcome != want {
			t.Errorf("Poll %s = %v, want %v", state, result.Outcome, want)
		}
	}
}

func TestNormalizeTags(t *testing.T) {
	fake := newFakeQBT(t, "5.0.4")
	client := loggedInClient(t, fake)
	ctx := context.Background()
	torrent := buildV1Single()
	meta := mustMeta(t, torrent)
	root := testRoot(t)
	taskPath := filepath.Join(root, "task-1")
	// Seed the torrent directly with the legacy per-task tag: AddStopped
	// would already normalize tags via ReconcileAdd, so the legacy state
	// has to be set up without going through an add.
	fake.seed(&fakeTorrent{
		hash: meta.InfoHashV1, state: "stoppedDL",
		savePath: taskPath, downloadPath: filepath.Join(taskPath, ".incomplete"),
		contentPath: taskPath, category: "musicops",
		tags: "musicops-task-1", autoTMM: false,
		files: []TorrentFile{{Name: "song.flac", Size: 100, Progress: 0, Priority: 1}},
	})
	rec := Record{
		TaskID: "task-1", ClientID: meta.InfoHashV1, InfoHashV1: meta.InfoHashV1,
		SourcePath: taskPath, TaskTag: "musicops-task-1",
		Category: "musicops", CreationIntent: true, State: StateStopped,
		Files: meta.Files,
	}
	changed, err := client.NormalizeTags(ctx, &rec)
	if err != nil {
		t.Fatalf("NormalizeTags: %v", err)
	}
	if !changed {
		t.Fatalf("NormalizeTags changed=false, want true")
	}
	if rec.TaskTag != ProjectTag || rec.LegacyTaskTag != "" {
		t.Fatalf("record after normalize: taskTag=%q legacy=%q", rec.TaskTag, rec.LegacyTaskTag)
	}
	rows, err := client.Info(ctx, rec.ClientID)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if len(rows) != 1 || rows[0].Tags != "MusicOps" {
		t.Fatalf("tags after normalize = %v, want [MusicOps]", rows)
	}
	found := false
	for _, tag := range fake.deletedTags {
		if tag == "musicops-task-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("deleted tags = %v, want musicops-task-1 deleted", fake.deletedTags)
	}
}

func TestNormalizeTagsKeepsTagStillInUse(t *testing.T) {
	fake := newFakeQBT(t, "5.0.4")
	client := loggedInClient(t, fake)
	ctx := context.Background()
	torrent := buildV1Single()
	meta := mustMeta(t, torrent)
	root := testRoot(t)
	taskPath := filepath.Join(root, "task-1")
	// Seed the legacy-tagged torrent directly (see TestNormalizeTags).
	fake.seed(&fakeTorrent{
		hash: meta.InfoHashV1, state: "stoppedDL",
		savePath: taskPath, downloadPath: filepath.Join(taskPath, ".incomplete"),
		contentPath: taskPath, category: "musicops",
		tags: "musicops-task-1", autoTMM: false,
		files: []TorrentFile{{Name: "song.flac", Size: 100, Progress: 0, Priority: 1}},
	})
	rec := Record{
		TaskID: "task-1", ClientID: meta.InfoHashV1, InfoHashV1: meta.InfoHashV1,
		SourcePath: taskPath, TaskTag: "musicops-task-1",
		Category: "musicops", CreationIntent: true, State: StateStopped,
		Files: meta.Files,
	}
	// A second torrent still uses the legacy tag.
	otherPath := filepath.Join(root, "task-2")
	fake.seed(&fakeTorrent{
		hash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", state: "stoppedDL",
		savePath: otherPath, downloadPath: filepath.Join(otherPath, ".incomplete"),
		contentPath: otherPath, category: "musicops",
		tags: "musicops-task-1", autoTMM: false,
		files: []TorrentFile{{Name: "other.flac", Size: 10, Progress: 0, Priority: 1}},
	})
	if _, err := client.NormalizeTags(ctx, &rec); err != nil {
		t.Fatalf("NormalizeTags: %v", err)
	}
	for _, tag := range fake.deletedTags {
		if tag == "musicops-task-1" {
			t.Fatalf("legacy tag deleted while still in use: %v", fake.deletedTags)
		}
	}
	rows, err := client.Info(ctx, rec.ClientID)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if len(rows) != 1 || rows[0].Tags != "MusicOps" {
		t.Fatalf("tags after normalize = %v, want [MusicOps]", rows)
	}
}

func TestSetLocationAndDownloadPath(t *testing.T) {
	fake := newFakeQBT(t, "5.0.4")
	client := loggedInClient(t, fake)
	ctx := context.Background()
	rec := addStoppedRecord(t, client, buildV1Single())
	newPath := rec.SourcePath + "-new"
	newDownloadPath := filepath.Join(newPath, ".incomplete")
	if err := client.SetLocation(ctx, rec.ClientID, newPath); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	if err := client.SetDownloadPath(ctx, rec.ClientID, newDownloadPath); err != nil {
		t.Fatalf("SetDownloadPath: %v", err)
	}
	rows, err := client.Info(ctx, rec.ClientID)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if len(rows) != 1 || rows[0].SavePath != newPath || rows[0].DownloadPath != newDownloadPath {
		t.Fatalf("rows after move = %+v", rows)
	}
}

func TestStartEndpointByVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    string
	}{
		{"5.0.4", "/api/v2/torrents/start"},
		{"4.6.5", "/api/v2/torrents/resume"},
	} {
		fake := newFakeQBT(t, tc.version)
		client := loggedInClient(t, fake)
		rec := addStoppedRecord(t, client, buildV1Single())
		meta := mustMeta(t, buildV1Single())
		if err := client.StartDownload(context.Background(), &rec,
			FreeEvidence{Confirmed: true, InfoHash: meta.InfoHashV1, Free: FreePtr(true)}); err != nil {
			t.Fatalf("version %s StartDownload: %v", tc.version, err)
		}
		if len(fake.startEndpoints) != 1 || fake.startEndpoints[0] != tc.want {
			t.Errorf("version %s start endpoints = %v, want [%s]", tc.version, fake.startEndpoints, tc.want)
		}
	}
}

func TestInfoUnexpectedStatus(t *testing.T) {
	fake := newFakeQBT(t, "5.0.4")
	client := loggedInClient(t, fake)
	fake.mu.Lock()
	fake.failInfoStatus = http.StatusInternalServerError
	fake.mu.Unlock()
	_, err := client.Info(context.Background())
	var respErr *ResponseError
	if !errors.As(err, &respErr) {
		t.Fatalf("Info error = %T %v, want *ResponseError", err, err)
	}
	if respErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", respErr.StatusCode)
	}
}

func TestStartDownloadAttemptsAndCooldown(t *testing.T) {
	fake := newFakeQBT(t, "5.0.4")
	client := loggedInClient(t, fake)
	ctx := context.Background()
	rec := addStoppedRecord(t, client, buildV1Single())
	meta := mustMeta(t, buildV1Single())
	evidence := FreeEvidence{Confirmed: true, InfoHash: meta.InfoHashV1, Free: FreePtr(true)}
	// First start succeeds; a second start is blocked by state, and a
	// re-stopped record with attempts already spent is blocked by cooldown.
	if err := client.StartDownload(ctx, &rec, evidence); err != nil {
		t.Fatalf("first StartDownload: %v", err)
	}
	rec.State = StateStopped
	fake.setState(rec.ClientID, "stoppedDL", 0)
	if err := client.StartDownload(ctx, &rec, evidence); !errors.Is(err, ErrStartCooldown) {
		t.Fatalf("second StartDownload error = %v, want ErrStartCooldown", err)
	}
	rec.StartAttempts = 3
	rec.StartRequestedAt = rec.StartRequestedAt.Add(-time.Hour)
	if err := client.StartDownload(ctx, &rec, evidence); !errors.Is(err, ErrStartAttemptsExhausted) {
		t.Fatalf("exhausted StartDownload error = %v, want ErrStartAttemptsExhausted", err)
	}
}

func TestClassifyState(t *testing.T) {
	cases := []struct {
		state    string
		progress float64
		want     PollOutcome
	}{
		{"checkingDL", 0, OutcomeWaiting},
		{"moving", 0.5, OutcomeWaiting},
		{"error", 0, OutcomeBlocked},
		{"missingFiles", 0, OutcomeBlocked},
		{"unknown", 0, OutcomeBlocked},
		{"stoppedDL", 0.5, OutcomeStopped},
		{"pausedDL", 0, OutcomeStopped},
		{"stoppedDL", 1, OutcomeDownloading},
		{"downloading", 0.5, OutcomeDownloading},
		{"uploading", 1, OutcomeDownloading},
		{"stalledDL", 0.1, OutcomeDownloading},
	}
	for _, tc := range cases {
		if got := ClassifyState(tc.state, tc.progress); got != tc.want {
			t.Errorf("ClassifyState(%q, %v) = %v, want %v", tc.state, tc.progress, got, tc.want)
		}
	}
}

func TestUnifyLegacyTagsAddsTheProjectTag(t *testing.T) {
	fake := newFakeQBT(t, "5.0.4")
	client := loggedInClient(t, fake)
	torrent := buildV1Single()
	meta := mustMeta(t, torrent)
	fake.seed(&fakeTorrent{
		hash: meta.InfoHashV1, state: "stoppedDL", tags: "musicops-task-1",
		files: []TorrentFile{{Name: "song.flac", Size: 100, Progress: 0, Priority: 1}},
	})
	changed, err := client.UnifyLegacyTags(context.Background())
	if err != nil || changed != 1 {
		t.Fatal(changed, err)
	}
	rows, err := client.Info(context.Background())
	if err != nil || len(rows) != 1 || !parseTags(rows[0].Tags)[ProjectTag] || parseTags(rows[0].Tags)["musicops-task-1"] {
		t.Fatal(rows, err)
	}
	if len(fake.deletedTags) != 1 || fake.deletedTags[0] != "musicops-task-1" {
		t.Fatal(fake.deletedTags)
	}
}

func TestParseVersion(t *testing.T) {
	v, err := ParseVersion("v5.0.4")
	if err != nil || v != (Version{5, 0, 4}) {
		t.Fatalf("ParseVersion(v5.0.4) = %v, %v", v, err)
	}
	v, err = ParseVersion("4.6")
	if err != nil || v != (Version{4, 6, 0}) {
		t.Fatalf("ParseVersion(4.6) = %v, %v", v, err)
	}
	for _, bad := range []string{"", "5", "5.0.4.1", "vX.Y.Z", "5.0.x"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) = no error", bad)
		}
	}
	v500 := Version{5, 0, 0}
	v465 := Version{4, 6, 5}
	if !v500.AtLeast(5, 0, 0) || v465.AtLeast(5, 0, 0) {
		t.Errorf("AtLeast broken")
	}
	v460 := Version{4, 6, 0}
	v523 := Version{5, 2, 3}
	if !v460.Supported() || v523.Supported() {
		t.Errorf("Supported range broken")
	}
}
