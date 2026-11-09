package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/desktop"
	"github.com/wsdbb72/MusicOps/native/core/internal/vault"
)

const testCorePassword = "test-core-password-1234"

// fakeCore is a minimal stand-in for the real core HTTP API: it enforces
// Basic auth and serves job list, job inspection and control.
type fakeCore struct {
	mu        sync.Mutex
	jobStage  string
	taskState string
	taskCalls int
	// stateSequence, when set, is the task state returned on successive
	// GET /v1/jobs/{id} calls (for wait tests).
	stateSequence []string
	actions       []string
}

func (f *fakeCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("musicops:"+testCorePassword)) {
		w.Header().Set("WWW-Authenticate", `Basic realm="MusicOps", charset="UTF-8"`)
		writeTestJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "code": "AUTH_REQUIRED"})
		return
	}
	write := func(status int, v any) { writeTestJSON(w, status, v) }
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs":
		write(http.StatusOK, map[string]any{"ok": true, "jobs": []map[string]any{
			{"job_id": "j1", "stage": f.jobStage},
		}})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
		f.mu.Lock()
		state := f.taskState
		if len(f.stateSequence) > 0 {
			i := f.taskCalls
			if i >= len(f.stateSequence) {
				i = len(f.stateSequence) - 1
			}
			state = f.stateSequence[i]
			f.taskCalls++
		}
		f.mu.Unlock()
		inspection := map[string]any{"job": map[string]any{"job_id": id, "stage": f.jobStage}}
		if state != "" {
			inspection["task"] = map[string]any{"state": state}
		}
		write(http.StatusOK, map[string]any{"ok": true, "inspection": inspection})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/control"):
		var body struct {
			Action string `json:"action"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.actions = append(f.actions, body.Action)
		f.mu.Unlock()
		if body.Action == "retry" {
			write(http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"), "/control")
		write(http.StatusOK, map[string]any{"ok": true, "job": map[string]any{"job_id": id, "stage": f.jobStage}})
	default:
		write(http.StatusNotFound, map[string]any{"ok": false, "code": "NOT_FOUND"})
	}
}

func writeTestJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func testClient(t *testing.T, core *fakeCore) (*coreClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(core)
	t.Cleanup(srv.Close)
	return &coreClient{base: srv.URL, password: testCorePassword}, srv
}

func TestClientGetJobs(t *testing.T) {
	core := &fakeCore{jobStage: "searching"}
	client, _ := testClient(t, core)
	jobs, err := client.getJobs(context.Background())
	if err != nil {
		t.Fatalf("getJobs: %v", err)
	}
	if len(jobs) != 1 || !strings.Contains(string(jobs[0]), "j1") {
		t.Fatalf("jobs = %s, want one job j1", jobs)
	}
	// A wrong password is an authentication error, not a job problem.
	bad := &coreClient{base: client.base, password: "wrong-password-1234"}
	if _, err := bad.getJobs(context.Background()); err == nil || !strings.Contains(err.Error(), "AUTH_REQUIRED") {
		t.Fatalf("getJobs with wrong password = %v, want AUTH_REQUIRED", err)
	}
}

func TestClientControlRetryRejected(t *testing.T) {
	core := &fakeCore{jobStage: "searching"}
	client, _ := testClient(t, core)
	if _, err := client.control(context.Background(), "j1", "retry"); err == nil || !strings.Contains(err.Error(), "INVALID_REQUEST") {
		t.Fatalf("control retry = %v, want INVALID_REQUEST", err)
	}
	if _, err := client.control(context.Background(), "j1", "pause"); err != nil {
		t.Fatalf("control pause: %v", err)
	}
	if len(core.actions) != 2 || core.actions[0] != "retry" || core.actions[1] != "pause" {
		t.Fatalf("actions = %v", core.actions)
	}
}

func TestExtractState(t *testing.T) {
	// Task state wins over the job stage.
	raw := json.RawMessage(`{"ok":true,"inspection":{"job":{"job_id":"j1","stage":"needs_review"},"task":{"state":"blocked"}}}`)
	if got := extractState(raw); got != "blocked" {
		t.Fatalf("extractState = %q, want blocked", got)
	}
	// Without a task, the job stage is the state.
	raw = json.RawMessage(`{"ok":true,"inspection":{"job":{"job_id":"j1","stage":"searching"}}}`)
	if got := extractState(raw); got != "searching" {
		t.Fatalf("extractState = %q, want searching", got)
	}
	// A control response carries the job view directly.
	raw = json.RawMessage(`{"ok":true,"job":{"job_id":"j1","stage":"cancelled"}}`)
	if got := extractState(raw); got != "cancelled" {
		t.Fatalf("extractState = %q, want cancelled", got)
	}
}

func TestExitCodeForState(t *testing.T) {
	for state, want := range map[string]int{
		"failed": 2, "blocked": 2, "cancelled": 2, "recovery_required": 2,
		"completed": 0, "needs_review": 0, "searching": 0, "": 0,
	} {
		if got := exitCodeForState(state); got != want {
			t.Errorf("exitCodeForState(%q) = %d, want %d", state, got, want)
		}
	}
}

func TestWaitSettled(t *testing.T) {
	core := &fakeCore{jobStage: "searching", stateSequence: []string{"running", "running", "completed"}}
	client, _ := testClient(t, core)
	raw, state, timedOut, err := client.waitSettled(context.Background(), "j1", 10)
	if err != nil || timedOut {
		t.Fatalf("waitSettled = err %v timedOut %v", err, timedOut)
	}
	if state != "completed" || !strings.Contains(string(raw), "completed") {
		t.Fatalf("waitSettled state = %q raw = %s", state, raw)
	}
	if core.taskCalls != 3 {
		t.Fatalf("waitSettled polled %d times, want 3", core.taskCalls)
	}
	// A job that never settles times out.
	stuck := &fakeCore{jobStage: "searching", stateSequence: []string{"running"}}
	stuckClient, _ := testClient(t, stuck)
	_, state, timedOut, err = stuckClient.waitSettled(context.Background(), "j1", 0.5)
	if err != nil || !timedOut || state != "running" {
		t.Fatalf("waitSettled(timeout) = state %q timedOut %v err %v", state, timedOut, err)
	}
}

func TestParseCLIFlags(t *testing.T) {
	flags, err := parseCLIFlags("job", []string{"j1", "--wait", "--wait-timeout", "30"})
	if err != nil {
		t.Fatalf("parseCLIFlags: %v", err)
	}
	if !flags.wait || flags.waitTimeout != 30 || len(flags.args) != 1 || flags.args[0] != "j1" {
		t.Fatalf("flags = %+v", flags)
	}
	if _, err := parseCLIFlags("job", []string{"j1", "--payload", "[1,2]"}); err == nil {
		t.Fatal("parseCLIFlags(non-object payload): want error")
	}
	if _, err := parseCLIFlags("job", []string{"j1", "--wait-timeout", "-1"}); err == nil {
		t.Fatal("parseCLIFlags(negative timeout): want error")
	}
}

func TestRunCLIUnknownCommand(t *testing.T) {
	if code := runCLI([]string{"bogus"}); code != 7 {
		t.Fatalf("runCLI(bogus) = %d, want 7", code)
	}
}

// --- Subprocess end-to-end test against a real core -----------------------

func buildCLIBinary(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go binary not on PATH")
	}
	out := filepath.Join(t.TempDir(), "musicops")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, goBin, "build", "-o", out, ".")
	cmd.Dir, _ = os.Getwd()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build: %v\n%s", err, stderr.String())
	}
	return out
}

func runCLIBinary(t *testing.T, binary string, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run %v: %v (stderr: %s)", args, err, stderr.String())
		}
	}
	if stderr.Len() > 0 {
		t.Logf("stderr for %v: %s", args, stderr.String())
	}
	return code, stdout.String()
}

// TestCLISubprocessEndToEnd runs the real CLI binary against a real core.
//
// The core is owned by a desktop.Supervisor for the duration of the test:
// that exercises the supervisor-owned path (the supervisor may restart
// the core after `stop`, which is why the test stops supervision too).
// The CLI-started detached path is covered by
// TestCLIDetachedCoreSurvivesAcrossInvocations below.
func TestCLISubprocessEndToEnd(t *testing.T) {
	binary := buildCLIBinary(t)
	profile := filepath.Join(t.TempDir(), "profile")
	t.Setenv("MUSICOPS_VAULT_KEY_FILE", filepath.Join(t.TempDir(), "master.key"))

	supCtx, supCancel := context.WithCancel(context.Background())
	defer supCancel()
	sup := desktop.New(profile, binary)
	if err := sup.Start(supCtx); err != nil {
		t.Fatalf("supervisor Start: %v", err)
	}
	handle := sup.Handle()
	if handle == nil || handle.URL == "" || handle.Password == "" {
		t.Fatalf("supervisor handle = %+v, want a live core URL and password", handle)
	}
	t.Cleanup(func() {
		_ = sup.Stop()
		if status, err := desktop.CoreStatus(profile); err == nil && status.Running && status.PID > 0 {
			if proc, err := os.FindProcess(status.PID); err == nil {
				_ = proc.Kill()
			}
		}
	})
	baseArgs := []string{"--base", profile, "--core-binary", binary}

	// version needs no core.
	if code, out := runCLIBinary(t, binary, "version"); code != 0 || !strings.Contains(out, `"version": "0.0.0-dev"`) {
		t.Fatalf("version: code %d out %s", code, out)
	}
	// migrate on a profile without Python state reports so.
	if code, out := runCLIBinary(t, binary, append([]string{"migrate"}, baseArgs...)...); code != 0 || !strings.Contains(out, "no python profile") {
		t.Fatalf("migrate: code %d out %s", code, out)
	}
	// The supervisor wrote the rendezvous password file; it must decrypt
	// to the handle password the CLI will use.
	rawPassword, err := os.ReadFile(filepath.Join(profile, "state", "core-password"))
	if err != nil {
		t.Fatalf("core-password file missing: %v", err)
	}
	password, err := vault.Unprotect(rawPassword)
	if err != nil {
		t.Fatalf("core-password undecryptable: %v", err)
	}
	if string(password) != handle.Password {
		t.Fatal("core-password file does not match the supervisor handle password")
	}
	// status attaches to the supervisor-owned core and lists jobs (empty).
	if code, out := runCLIBinary(t, binary, append([]string{"status"}, baseArgs...)...); code != 0 || !strings.Contains(out, `"jobs": []`) {
		t.Fatalf("status: code %d out %s", code, out)
	}
	coreURL := handle.URL
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("musicops:"+handle.Password))
	post := func(path string, body any) (int, string) {
		encoded, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, coreURL+path, bytes.NewReader(encoded))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}

	// Create a job through the real core API, then drive it via the CLI.
	// The fake tracker answers after 12s (inside the 15s client
	// timeout), so the search task stays running long enough to pause,
	// resume and cancel deterministically instead of finishing in
	// milliseconds. The 8s budget was not enough: CLI subprocess spawns
	// on a loaded runner are slow enough that the search finished
	// between resume and cancel on macOS, and the cancel was then
	// rejected as stale (WORKFLOW_REJECTED).
	slowTracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(12 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
	}))
	t.Cleanup(slowTracker.Close)
	jobID := "e2e-job-1"
	status, body := post("/v1/search", map[string]any{
		"job_id": jobID, "batch_id": "e2e-batch-1", "scene": "playlist", "query": "test",
		"expected_title": "Test", "expected_artist": "Artist",
		"source_track_ids": []string{"t1"}, "synthetic": false,
		"enabled_sites": []string{"dicmusic"}, "base_url": slowTracker.URL,
		"cookie":   "session=cookie-secret-not-stored",
		"identity": map[string]any{"evidence_id": "ev-catalog", "kind": "catalog", "source": "musicbrainz", "subject_id": "release-1", "tracklist_verified": true, "identity_confirmed": true, "payload": "catalogue-row"},
	})
	if status != http.StatusAccepted {
		t.Fatalf("POST /v1/search: status %d body %s", status, body)
	}
	// Wait until the search task itself is running: identify finishes in
	// milliseconds, and the running work lives on the child job
	// search:<id>, not on the root job (whose own task is identify).
	searchJobID := "search:" + jobID
	get := func(path string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, coreURL+path, nil)
		req.Header.Set("Authorization", auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}
	searching := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		_, jobBody := get("/v1/jobs/" + searchJobID)
		if strings.Contains(jobBody, `"stage": "searching"`) || strings.Contains(jobBody, `"stage":"searching"`) {
			searching = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !searching {
		t.Fatalf("job %s never reached searching stage", searchJobID)
	}
	if code, out := runCLIBinary(t, binary, append([]string{"control", searchJobID, "pause"}, baseArgs...)...); code != 0 {
		t.Fatalf("control pause: code %d out %s", code, out)
	}
	// jobs lists the new job.
	if code, out := runCLIBinary(t, binary, append([]string{"jobs"}, baseArgs...)...); code != 0 || !strings.Contains(out, jobID) {
		t.Fatalf("jobs: code %d out %s", code, out)
	}
	// job shows it paused (exit 0: paused is not terminal).
	if code, out := runCLIBinary(t, binary, append([]string{"job", searchJobID}, baseArgs...)...); code != 0 || !strings.Contains(out, searchJobID) || !strings.Contains(out, `"paused": true`) {
		t.Fatalf("job(paused): code %d out %s", code, out)
	}
	// resume, then pause again before cancelling: the search only runs
	// while unpaused, so re-pausing freezes it and the cancel below
	// cannot lose a race with the search finishing.
	if code, out := runCLIBinary(t, binary, append([]string{"control", searchJobID, "resume"}, baseArgs...)...); code != 0 {
		t.Fatalf("control resume: code %d out %s", code, out)
	}
	if code, out := runCLIBinary(t, binary, append([]string{"control", searchJobID, "pause"}, baseArgs...)...); code != 0 {
		t.Fatalf("control pause again: code %d out %s", code, out)
	}
	if code, out := runCLIBinary(t, binary, append([]string{"control", searchJobID, "cancel"}, baseArgs...)...); code != 0 && code != 2 {
		t.Fatalf("control cancel: code %d out %s", code, out)
	}
	// Cancel is asynchronous: the running search observes the request and
	// then unwinds, so poll until the job reports a terminal state.
	cancelled := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if code, _ := runCLIBinary(t, binary, append([]string{"job", searchJobID}, baseArgs...)...); code == 2 {
			cancelled = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !cancelled {
		t.Fatalf("job(cancelled) never reached exit 2")
	}
	// --wait on an already terminal job returns at once with exit 2.
	if code, _ := runCLIBinary(t, binary, append([]string{"job", searchJobID, "--wait"}, baseArgs...)...); code != 2 {
		t.Fatalf("job --wait(cancelled): code %d, want 2", code)
	}
	// retry is rejected by the core at job level: CLI error, exit 7.
	if code, out := runCLIBinary(t, binary, append([]string{"control", searchJobID, "retry"}, baseArgs...)...); code != 7 || !strings.Contains(out, "INVALID_REQUEST") {
		t.Fatalf("control retry: code %d out %s, want 7 + INVALID_REQUEST", code, out)
	}
	// stop shuts the core down. The supervisor may restart it first, so
	// stop supervision as well, then the core must be gone for good.
	if code, out := runCLIBinary(t, binary, append([]string{"stop"}, baseArgs...)...); code != 0 || !strings.Contains(out, `"ok": true`) {
		t.Fatalf("stop: code %d out %s", code, out)
	}
	if err := sup.Stop(); err != nil {
		t.Fatalf("supervisor Stop: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := desktop.CoreStatus(profile)
		if err != nil {
			t.Fatalf("CoreStatus after stop: %v", err)
		}
		if !status.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("core still running after stop: %+v", status)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// stop is idempotent.
	if code, out := runCLIBinary(t, binary, append([]string{"stop"}, baseArgs...)...); code != 0 || !strings.Contains(out, `"ok": true`) {
		t.Fatalf("stop again: code %d out %s", code, out)
	}
}

// TestCLIDetachedCoreSurvivesAcrossInvocations proves the CLI's detached
// start: the first `status` starts a core and exits, yet the core is
// still alive for the next invocation (same PID), and `stop` ends it.
func TestCLIDetachedCoreSurvivesAcrossInvocations(t *testing.T) {
	binary := buildCLIBinary(t)
	profile := filepath.Join(t.TempDir(), "profile")
	t.Setenv("MUSICOPS_VAULT_KEY_FILE", filepath.Join(t.TempDir(), "master.key"))
	baseArgs := []string{"--base", profile, "--core-binary", binary}

	readPID := func() int {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(profile, "state", "core.json"))
		if err != nil {
			t.Fatalf("core.json: %v", err)
		}
		var rec struct {
			URL string `json:"url"`
			PID int    `json:"pid"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatalf("core.json: %v", err)
		}
		return rec.PID
	}
	t.Cleanup(func() {
		if status, err := desktop.CoreStatus(profile); err == nil && status.Running && status.PID > 0 {
			if proc, err := os.FindProcess(status.PID); err == nil {
				_ = proc.Kill()
			}
		}
	})

	// First status starts a detached core and exits.
	if code, out := runCLIBinary(t, binary, append([]string{"status"}, baseArgs...)...); code != 0 || !strings.Contains(out, `"jobs": []`) {
		t.Fatalf("status (first): code %d out %s", code, out)
	}
	firstPID := readPID()

	// The core survived the CLI process exiting: still running, same PID.
	status, err := desktop.CoreStatus(profile)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Running || status.PID != firstPID {
		t.Fatalf("CoreStatus after first CLI exit = %+v, want running pid %d", status, firstPID)
	}

	// A second status attaches to the same core instead of starting one.
	if code, out := runCLIBinary(t, binary, append([]string{"status"}, baseArgs...)...); code != 0 || !strings.Contains(out, `"jobs": []`) {
		t.Fatalf("status (second): code %d out %s", code, out)
	}
	if pid := readPID(); pid != firstPID {
		t.Fatalf("second status started a new core: pid %d, want %d", pid, firstPID)
	}

	// stop ends the detached core for good.
	if code, out := runCLIBinary(t, binary, append([]string{"stop"}, baseArgs...)...); code != 0 || !strings.Contains(out, `"ok": true`) {
		t.Fatalf("stop: code %d out %s", code, out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := desktop.CoreStatus(profile)
		if err != nil {
			t.Fatalf("CoreStatus after stop: %v", err)
		}
		if !status.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("core still running after stop: %+v", status)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
