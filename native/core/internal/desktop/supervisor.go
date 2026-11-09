// Core supervision for the desktop shell.
//
// Ports worker/supervision.py (the Windows PowerShell supervisor: 5s
// patrol, restart, 5 restarts in 10 minutes then 30s backoff),
// worker/lifecycle.py (lifecycle.jsonl with 2MB rotation, worker-health.json
// every 5s) and the ensure_running half of worker/client.py (attach to a
// live core, otherwise start one and wait up to 90s for its ready signal).
//
// Differences from the Python worker, by design: the supervised process is
// the Go core (cmd/musicops), not a Python worker; rendezvous is
// core.json/core-password under <profile>/state instead of an IPC socket;
// the supervisor lives in the desktop process itself, so there is no
// separate PowerShell script.
package desktop

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/vault"
)

// CoreHandle describes a running Go core.
type CoreHandle struct {
	URL         string // e.g. http://127.0.0.1:54321
	PID         int
	Password    string
	StartedByUs bool
}

// CoreStatusInfo is the result of CoreStatus: whether the rendezvous core
// is alive and where it listens.
type CoreStatusInfo struct {
	Running       bool
	URL           string
	PID           int
	StartedAtUnix int64
}

const (
	// readyTimeout is how long to wait for a freshly started core to print
	// its ready JSON and answer /healthz (mirrors client.ensure_running's
	// 90s deadline).
	readyTimeout = 90 * time.Second
	// healthProbeInterval is the /healthz poll interval while waiting for
	// readiness.
	healthProbeInterval = 200 * time.Millisecond
	// healthWriteInterval mirrors lifecycle.monitor's 5s health cadence.
	healthWriteInterval = 5 * time.Second
	// lifecycleMaxBytes mirrors lifecycle.append's 2MB rotation threshold.
	lifecycleMaxBytes = 2_000_000
	// maxRestartsPerWindow / restartWindow / restartBackoff mirror the
	// PowerShell supervisor: 5 starts in 10 minutes, then 30s between tries.
	maxRestartsPerWindow = 5
	restartWindow        = 10 * time.Minute
	restartBackoff       = 30 * time.Second
	// stopTimeout mirrors the graceful shutdown grace period before SIGKILL.
	stopTimeout = 10 * time.Second
	// failedStartRetry is the pause between failed restart attempts inside
	// the monitor loop (the initial start reports its error instead).
	failedStartRetry = 5 * time.Second
)

func stateDir(profile string) string  { return filepath.Join(profile, "state", "native") }
func backupDir(profile string) string { return filepath.Join(profile, "state", "native-backup") }
func coreJSONPath(profile string) string {
	return filepath.Join(profile, "state", "core.json")
}
func corePasswordPath(profile string) string {
	return filepath.Join(profile, "state", "core-password")
}
func healthPath(profile string) string {
	return filepath.Join(profile, "state", "worker-health.json")
}
func lifecyclePath(profile string) string {
	return filepath.Join(profile, "state", "lifecycle.jsonl")
}
func stopFilePath(profile string) string {
	return filepath.Join(profile, "state", "supervisor-stop")
}
func lockPath(profile string) string {
	return filepath.Join(profile, "state", "desktop.lock")
}
func coreLogPath(profile string) string {
	return filepath.Join(profile, "state", "core.log")
}

// rendezvous is the on-disk core.json record.
type rendezvous struct {
	URL           string `json:"url"`
	PID           int    `json:"pid"`
	StartedAtUnix int64  `json:"started_at_unix"`
}

// healthRecord is the worker-health.json record, written every 5 seconds.
type healthRecord struct {
	PID       int    `json:"pid"`
	URL       string `json:"url"`
	At        int64  `json:"at"`
	Restarts  int    `json:"restarts"`
	LastError string `json:"last_error,omitempty"`
}

// ensureCoreDirs creates the Go state/backup directories (0700).
func ensureCoreDirs(profile string) error {
	if err := privateDirectory(stateDir(profile)); err != nil {
		return err
	}
	return privateDirectory(backupDir(profile))
}

// ResolveCoreBinary resolves the core executable, in order: an explicit
// --core-binary value, $MUSICOPS_CORE_BINARY, musicops[.exe] next to the
// desktop executable, and musicops on PATH.
func ResolveCoreBinary(explicit string) (string, error) {
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	}
	if env := os.Getenv("MUSICOPS_CORE_BINARY"); env != "" {
		candidates = append(candidates, env)
	}
	if exe, err := os.Executable(); err == nil {
		name := "musicops"
		if runtime.GOOS == "windows" {
			name = "musicops.exe"
		}
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), name))
	}
	for _, c := range candidates {
		if resolved, err := exec.LookPath(c); err == nil {
			return resolved, nil
		}
	}
	if resolved, err := exec.LookPath("musicops"); err == nil {
		return resolved, nil
	}
	return "", errors.New("cannot locate the musicops core binary: pass --core-binary or set MUSICOPS_CORE_BINARY")
}

// generatePassword returns n random alphanumeric characters (crypto/rand).
// The core requires a password of at least 16 characters.
func generatePassword(n int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(buf), nil
}

// probeHealth GETs url/healthz with the core's Basic credentials and
// reports whether it answers 200.
func probeHealth(url, password string) bool {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(url, "/")+"/healthz", nil)
	if err != nil {
		return false
	}
	req.SetBasicAuth("musicops", password)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// readRendezvous reads core.json and the vault-protected core-password.
// It returns nil, nil when no rendezvous exists.
func readRendezvous(profile string) (*rendezvous, string, error) {
	raw, err := os.ReadFile(coreJSONPath(profile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	var rec rendezvous
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, "", fmt.Errorf("core.json is corrupt: %w", err)
	}
	blob, err := os.ReadFile(corePasswordPath(profile))
	if err != nil {
		return nil, "", fmt.Errorf("core password file is missing: %w", err)
	}
	plain, err := vault.Unprotect(blob)
	if err != nil {
		return nil, "", fmt.Errorf("cannot decrypt the core password: %w", err)
	}
	return &rec, string(plain), nil
}

// writeRendezvous atomically writes core.json and the vault-protected
// core-password (both 0600).
func writeRendezvous(profile string, handle *CoreHandle, startedAt int64) error {
	encoded, err := json.Marshal(rendezvous{URL: handle.URL, PID: handle.PID, StartedAtUnix: startedAt})
	if err != nil {
		return err
	}
	if err := atomicWriteFile(coreJSONPath(profile), encoded, 0600); err != nil {
		return err
	}
	blob, err := vault.Protect([]byte(handle.Password))
	if err != nil {
		return err
	}
	return atomicWriteFile(corePasswordPath(profile), blob, 0600)
}

// clearRendezvous removes core.json and core-password, ignoring missing
// files. The health file is deliberately kept (last known state).
func clearRendezvous(profile string) {
	_ = os.Remove(coreJSONPath(profile))
	_ = os.Remove(corePasswordPath(profile))
}

// appendLifecycle appends one JSON line to lifecycle.jsonl, rotating to
// lifecycle.previous.jsonl past 2MB, mirroring worker/lifecycle.py. It
// never fails the caller: lifecycle logging must not break supervision.
func appendLifecycle(profile, event string, fields map[string]any) {
	path := lifecyclePath(profile)
	if info, err := os.Stat(path); err == nil && info.Size() > lifecycleMaxBytes {
		previous := strings.TrimSuffix(path, ".jsonl") + ".previous.jsonl"
		_ = os.Remove(previous)
		_ = os.Rename(path, previous)
	}
	record := map[string]any{"at": float64(time.Now().UnixNano()) / 1e9, "pid": os.Getpid(), "event": event}
	for k, v := range fields {
		record[k] = v
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(encoded, '\n'))
	_ = f.Close()
}

// runningCore is a core process owned by this supervisor.
type runningCore struct {
	handle *CoreHandle
	cmd    *exec.Cmd  // nil when attached to an already-running core
	stdinW *os.File   // nil when attached; closing it asks the core to stop
	exited chan error // fed once by the reaper goroutine; nil when attached
	stderr bytes.Buffer
}

// startCore launches a new core and waits up to 90s for its ready JSON and
// a 200 from /healthz. On success it writes the rendezvous files.
//
// When detached is true the core is started without --parent-pipe, in a
// new session/process group, with stdout/stderr redirected to core.log:
// a short-lived parent (the CLI) must not own the core's lifetime, and a
// pipe would break with SIGPIPE once that parent exits. The supervisor and
// the desktop always use detached=false, so their core still dies with them.
func startCore(profile, coreBinary string, detached bool) (*runningCore, error) {
	if err := ensureCoreDirs(profile); err != nil {
		return nil, err
	}
	binary, err := ResolveCoreBinary(coreBinary)
	if err != nil {
		return nil, err
	}
	password, err := generatePassword(32)
	if err != nil {
		return nil, err
	}
	args := []string{
		"--state-dir", stateDir(profile),
		"--backup-dir", backupDir(profile),
		"--listen", "127.0.0.1:0",
	}
	rc := &runningCore{exited: make(chan error, 1)}
	var stdinR, stdinW *os.File
	var stdoutR io.ReadCloser
	var logFile *os.File
	if detached {
		logFile, err = os.OpenFile(coreLogPath(profile), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
	} else {
		args = append(args, "--parent-pipe")
		stdinR, stdinW, err = os.Pipe()
		if err != nil {
			return nil, err
		}
	}
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), "MUSICOPS_APP_PASSWORD="+password)
	if detached {
		cmd.SysProcAttr = detachProcAttr()
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	} else {
		cmd.Stdin = stdinR
		stdoutR, err = cmd.StdoutPipe()
		if err != nil {
			_ = stdinR.Close()
			_ = stdinW.Close()
			return nil, err
		}
		rc.stdinW = stdinW
		// Stderr is captured for diagnostics through a writer the command owns.
		cmd.Stderr = &teeWriter{buf: &rc.stderr}
	}
	closeStdin := func() {
		if stdinW != nil {
			_ = stdinW.Close()
		}
	}
	if err := cmd.Start(); err != nil {
		if stdinR != nil {
			_ = stdinR.Close()
		}
		closeStdin()
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, fmt.Errorf("cannot start the core %q: %w", binary, err)
	}
	if stdinR != nil {
		_ = stdinR.Close() // the child owns the read end now
	}
	if logFile != nil {
		_ = logFile.Close() // the child owns its copy now
	}

	// Reap exactly once, in the background, so a started core never
	// becomes a zombie even if nobody waits on it yet.
	go func() { rc.exited <- cmd.Wait() }()

	deadline := time.Now().Add(readyTimeout)
	var url string
	if detached {
		url, err = waitReadyFromLog(profile, deadline, rc.exited, cmd)
		if err != nil {
			return nil, err
		}
	} else {
		readyCh := make(chan string, 1)
		go func() {
			scanner := bufio.NewScanner(stdoutR)
			scanner.Buffer(make([]byte, 64*1024), 1024*1024)
			for scanner.Scan() {
				var ready struct {
					Ready bool   `json:"ready"`
					URL   string `json:"url"`
				}
				if err := json.Unmarshal(scanner.Bytes(), &ready); err == nil && ready.Ready && ready.URL != "" {
					readyCh <- ready.URL
					return
				}
			}
		}()
		select {
		case url = <-readyCh:
		case err := <-rc.exited:
			closeStdin()
			return nil, fmt.Errorf("core exited before becoming ready: %v (stderr: %s)", err, strings.TrimSpace(rc.stderr.String()))
		case <-time.After(time.Until(deadline)):
			_ = cmd.Process.Kill()
			<-rc.exited
			closeStdin()
			return nil, fmt.Errorf("core did not become ready within %s", readyTimeout)
		}
	}

	// Poll /healthz every 200ms until the core answers or the deadline
	// (shared with the ready wait) expires.
	for time.Now().Before(deadline) {
		if probeHealth(url, password) {
			break
		}
		select {
		case err := <-rc.exited:
			closeStdin()
			return nil, fmt.Errorf("core exited while waiting for /healthz: %v (diagnostics: %s)", err, strings.TrimSpace(rc.diagText(profile)))
		case <-time.After(healthProbeInterval):
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			<-rc.exited
			closeStdin()
			return nil, fmt.Errorf("core did not answer /healthz within %s", readyTimeout)
		}
	}
	if !probeHealth(url, password) {
		_ = cmd.Process.Kill()
		<-rc.exited
		closeStdin()
		return nil, fmt.Errorf("core did not answer /healthz within %s", readyTimeout)
	}

	handle := &CoreHandle{URL: url, PID: cmd.Process.Pid, Password: password, StartedByUs: true}
	rc.handle = handle
	rc.cmd = cmd
	startedAt := time.Now().Unix()
	if err := writeRendezvous(profile, handle, startedAt); err != nil {
		_ = cmd.Process.Kill()
		<-rc.exited
		closeStdin()
		return nil, fmt.Errorf("cannot write the core rendezvous: %w", err)
	}
	appendLifecycle(profile, "core_started", map[string]any{"url": url, "pid": handle.PID, "started_by_us": true, "detached": detached})
	return rc, nil
}

// diagText returns the best available diagnostics for a failed start: the
// captured stderr for a supervised core, the tail of core.log for a
// detached one.
func (rc *runningCore) diagText(profile string) string {
	if rc.stderr.Len() > 0 {
		return rc.stderr.String()
	}
	return logTail(profile)
}

// waitReadyFromLog polls <profile>/state/core.log for the core's ready
// JSON line, for detached starts where stdout is a file, not a pipe.
func waitReadyFromLog(profile string, deadline time.Time, exited chan error, cmd *exec.Cmd) (string, error) {
	for time.Now().Before(deadline) {
		if url := readyURLFromLog(profile); url != "" {
			return url, nil
		}
		select {
		case err := <-exited:
			return "", fmt.Errorf("core exited before becoming ready: %v (log tail: %s)", err, strings.TrimSpace(logTail(profile)))
		case <-time.After(100 * time.Millisecond):
		}
	}
	_ = cmd.Process.Kill()
	<-exited
	return "", fmt.Errorf("core did not become ready within %s", readyTimeout)
}

// readyURLFromLog scans core.log for a {"ready":true,"url":...} line.
func readyURLFromLog(profile string) string {
	raw, err := os.ReadFile(coreLogPath(profile))
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var ready struct {
			Ready bool   `json:"ready"`
			URL   string `json:"url"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &ready); err == nil && ready.Ready && ready.URL != "" {
			return ready.URL
		}
	}
	return ""
}

// logTail returns the last 4KB of core.log for diagnostics.
func logTail(profile string) string {
	raw, err := os.ReadFile(coreLogPath(profile))
	if err != nil {
		return ""
	}
	if len(raw) > 4096 {
		raw = raw[len(raw)-4096:]
	}
	return string(raw)
}

// attachCore tries to attach to the core described by the rendezvous files.
// It returns (nil, nil) when no rendezvous exists or the recorded process
// is gone. A live process that cannot be authenticated is an error: starting
// a second core on the same state directory would be worse than refusing.
func attachCore(profile string) (*runningCore, error) {
	rec, password, err := readRendezvous(profile)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, nil
	}
	if !pidAlive(rec.PID) {
		return nil, nil
	}
	if password == "" {
		return nil, fmt.Errorf("core process %d is running but its password cannot be read", rec.PID)
	}
	if !probeHealth(rec.URL, password) {
		return nil, fmt.Errorf("core process %d is running but does not answer %s/healthz", rec.PID, rec.URL)
	}
	return &runningCore{
		handle: &CoreHandle{URL: rec.URL, PID: rec.PID, Password: password, StartedByUs: false},
	}, nil
}

// EnsureCore attaches to the live core for profile when the rendezvous
// files describe one, otherwise starts a new core. It performs a single
// attach-or-start; continuous supervision is Supervisor's job.
func EnsureCore(profile, coreBinary string) (*CoreHandle, error) {
	if err := ensureCoreDirs(profile); err != nil {
		return nil, err
	}
	if rc, err := attachCore(profile); err != nil {
		return nil, err
	} else if rc != nil {
		return rc.handle, nil
	}
	rc, err := startCore(profile, coreBinary, false)
	if err != nil {
		return nil, err
	}
	return rc.handle, nil
}

// EnsureCoreDetached attaches to the live core for profile when the
// rendezvous files describe one, otherwise starts a new core that survives
// the calling process: no --parent-pipe, a new session/process group, and
// stdout/stderr in core.log. This is the CLI's start path: a short-lived
// CLI invocation must not kill the core it just started. The desktop
// supervisor uses EnsureCore/Supervisor instead, so its core still dies
// with it.
func EnsureCoreDetached(profile, coreBinary string) (*CoreHandle, error) {
	if err := ensureCoreDirs(profile); err != nil {
		return nil, err
	}
	if rc, err := attachCore(profile); err != nil {
		return nil, err
	} else if rc != nil {
		return rc.handle, nil
	}
	rc, err := startCore(profile, coreBinary, true)
	if err != nil {
		return nil, err
	}
	return rc.handle, nil
}

// CoreStatus reads core.json and probes liveness, returning the rendezvous
// core's state. A missing or stale rendezvous reports Running=false.
func CoreStatus(profile string) (CoreStatusInfo, error) {
	raw, err := os.ReadFile(coreJSONPath(profile))
	if err != nil {
		if os.IsNotExist(err) {
			return CoreStatusInfo{}, nil
		}
		return CoreStatusInfo{}, err
	}
	var rec rendezvous
	if err := json.Unmarshal(raw, &rec); err != nil {
		return CoreStatusInfo{}, fmt.Errorf("core.json is corrupt: %w", err)
	}
	if !pidAlive(rec.PID) {
		return CoreStatusInfo{PID: rec.PID, URL: rec.URL, StartedAtUnix: rec.StartedAtUnix}, nil
	}
	// /healthz needs no credentials; a live core answers 200.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(strings.TrimSuffix(rec.URL, "/") + "/healthz")
	if err != nil {
		return CoreStatusInfo{PID: rec.PID, URL: rec.URL, StartedAtUnix: rec.StartedAtUnix}, nil
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return CoreStatusInfo{PID: rec.PID, URL: rec.URL, StartedAtUnix: rec.StartedAtUnix}, nil
	}
	return CoreStatusInfo{Running: true, URL: rec.URL, PID: rec.PID, StartedAtUnix: rec.StartedAtUnix}, nil
}

// StopCore terminates the rendezvous core: SIGTERM, up to 10s grace, then
// SIGKILL, then rendezvous cleanup. It is idempotent: a missing rendezvous
// or an already-dead core returns nil.
func StopCore(profile string) error {
	raw, err := os.ReadFile(coreJSONPath(profile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var rec rendezvous
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("core.json is corrupt: %w", err)
	}
	if pidAlive(rec.PID) {
		_ = signalPID(rec.PID, sigTerm)
		deadline := time.Now().Add(stopTimeout)
		for pidAlive(rec.PID) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if pidAlive(rec.PID) {
			_ = signalPID(rec.PID, sigKill)
			for i := 0; i < 50 && pidAlive(rec.PID); i++ {
				time.Sleep(100 * time.Millisecond)
			}
		}
	}
	clearRendezvous(profile)
	return nil
}

// teeWriter captures a child process's stderr into a buffer for diagnostics.
type teeWriter struct{ buf *bytes.Buffer }

func (w *teeWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }

// Supervisor owns one core for a profile: it starts (or attaches to) the
// core, restarts it when it exits unexpectedly, writes the health file
// every 5s, and stops it cleanly on Stop.
type Supervisor struct {
	profile    string
	coreBinary string
	logger     *log.Logger

	mu       sync.Mutex
	lockFile *os.File
	stopCh   chan struct{}
	stopOnce sync.Once
	doneCh   chan struct{}
	current  *runningCore
	restarts []time.Time // start times in the last restartWindow
	total    int         // restarts since Start
	lastErr  string
	wg       sync.WaitGroup
	started  bool
}

// New returns a Supervisor for profile. coreBinary is passed through to
// ResolveCoreBinary ("" means auto-detect).
func New(profile, coreBinary string) *Supervisor {
	return &Supervisor{
		profile:    profile,
		coreBinary: coreBinary,
		logger:     log.New(os.Stderr, "musicops supervisor: ", log.LstdFlags),
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
}

// Handle returns the current core handle, or nil when no core is running.
func (s *Supervisor) Handle() *CoreHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil || s.current.handle == nil {
		return nil
	}
	h := *s.current.handle
	return &h
}

// Start acquires the desktop lock, removes any stale supervisor-stop file,
// attaches to or starts the core, and launches the monitor and health
// loops. A core that cannot be started (missing binary, 90s without
// readiness) is an explicit error, never a silent background state.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("supervisor is already started")
	}
	s.mu.Unlock()

	if err := ensureCoreDirs(s.profile); err != nil {
		return err
	}
	lockFile, err := acquireDesktopLock(lockPath(s.profile))
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.lockFile = lockFile
	s.mu.Unlock()
	cleanupLock := func() {
		s.mu.Lock()
		if s.lockFile != nil {
			releaseDesktopLock(s.lockFile)
			s.lockFile = nil
		}
		s.mu.Unlock()
	}

	_ = os.Remove(stopFilePath(s.profile))

	rc, err := attachCore(s.profile)
	if err != nil {
		cleanupLock()
		return err
	}
	if rc == nil {
		rc, err = startCore(s.profile, s.coreBinary, false)
		if err != nil {
			cleanupLock()
			return err
		}
	}
	s.mu.Lock()
	s.current = rc
	s.started = true
	s.mu.Unlock()

	appendLifecycle(s.profile, "supervisor_started", map[string]any{"pid": rc.handle.PID, "started_by_us": rc.handle.StartedByUs})

	s.wg.Add(2)
	go s.healthLoop()
	go s.monitorLoop(ctx)
	return nil
}

// Wait blocks until the supervisor has fully stopped (after Stop).
func (s *Supervisor) Wait() { <-s.doneCh }

// Stop writes the supervisor-stop file (intentional shutdown), terminates
// the core (SIGTERM, 10s grace, SIGKILL), clears the rendezvous files
// (core.json and core-password; the health file keeps its last record),
// releases the desktop lock, and waits for the monitor loops. It is
// idempotent.
func (s *Supervisor) Stop() error {
	var err error
	s.stopOnce.Do(func() {
		close(s.stopCh)
		_ = os.WriteFile(stopFilePath(s.profile), []byte("intentional shutdown"), 0600)
		s.mu.Lock()
		rc := s.current
		s.mu.Unlock()
		if rc != nil {
			err = s.terminateCore(rc)
		}
		clearRendezvous(s.profile)
		s.mu.Lock()
		if s.lockFile != nil {
			releaseDesktopLock(s.lockFile)
			s.lockFile = nil
		}
		s.mu.Unlock()
		appendLifecycle(s.profile, "supervisor_stopped", nil)
		s.wg.Wait()
		close(s.doneCh)
	})
	return err
}

// terminateCore stops one core process gracefully: close the parent pipe
// (the core watches stdin and shuts down when it closes), SIGTERM, up to
// 10s, then SIGKILL.
func (s *Supervisor) terminateCore(rc *runningCore) error {
	pid := rc.handle.PID
	if rc.stdinW != nil {
		_ = rc.stdinW.Close()
	}
	if !pidAlive(pid) {
		return nil
	}
	_ = signalPID(pid, sigTerm)
	deadline := time.Now().Add(stopTimeout)
	for pidAlive(pid) && time.Now().Before(deadline) {
		if rc.exited != nil {
			select {
			case <-rc.exited:
				return nil
			case <-time.After(100 * time.Millisecond):
			}
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !pidAlive(pid) {
		return nil
	}
	_ = signalPID(pid, sigKill)
	if rc.exited != nil {
		select {
		case <-rc.exited:
			return nil
		case <-time.After(5 * time.Second):
			return fmt.Errorf("core process %d did not exit after SIGKILL", pid)
		}
	}
	return nil
}

// monitorLoop restarts the core whenever it exits unexpectedly, applying
// the PowerShell supervisor's backoff: more than 5 starts in 10 minutes
// means waiting 30s before each further try.
func (s *Supervisor) monitorLoop(ctx context.Context) {
	defer s.wg.Done()
	for {
		rc := s.currentHandle()
		if rc == nil {
			return
		}
		exited := s.waitForExit(ctx, rc)
		if exited == exitStopped {
			return
		}
		if exited == exitContextDone {
			return
		}
		// Unexpected exit.
		appendLifecycle(s.profile, "core_exited", map[string]any{
			"pid": rc.handle.PID, "url": rc.handle.URL, "started_by_us": rc.handle.StartedByUs,
		})
		s.logger.Printf("core %d exited unexpectedly; restarting", rc.handle.PID)
		for {
			if s.isStopping() {
				return
			}
			s.mu.Lock()
			now := time.Now()
			recent := s.restarts[:0]
			for _, t := range s.restarts {
				if now.Sub(t) < restartWindow {
					recent = append(recent, t)
				}
			}
			s.restarts = recent
			backoff := time.Duration(0)
			if len(s.restarts) >= maxRestartsPerWindow {
				backoff = restartBackoff
			}
			s.mu.Unlock()
			if backoff > 0 {
				s.logger.Printf("core restarted %d times in %s; waiting %s", len(recent), restartWindow, backoff)
				if !s.sleepOrStop(backoff) {
					return
				}
			}
			next, err := startCore(s.profile, s.coreBinary, false)
			if err != nil {
				s.mu.Lock()
				s.lastErr = err.Error()
				s.mu.Unlock()
				appendLifecycle(s.profile, "core_restart_failed", map[string]any{"error": err.Error()})
				s.logger.Printf("core restart failed: %v", err)
				if !s.sleepOrStop(failedStartRetry) {
					return
				}
				continue
			}
			if s.isStopping() {
				// Stop raced a successful restart: terminate the core we
				// just started instead of leaking it.
				_ = s.terminateCore(next)
				return
			}
			s.mu.Lock()
			s.current = next
			s.restarts = append(s.restarts, time.Now())
			s.total++
			s.mu.Unlock()
			appendLifecycle(s.profile, "core_restarted", map[string]any{"pid": next.handle.PID, "url": next.handle.URL})
			break
		}
	}
}

type exitReason int

const (
	exitStopped exitReason = iota
	exitContextDone
	exitDied
)

func (s *Supervisor) currentHandle() *runningCore {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

func (s *Supervisor) isStopping() bool {
	select {
	case <-s.stopCh:
		return true
	default:
		return false
	}
}

func (s *Supervisor) sleepOrStop(d time.Duration) bool {
	select {
	case <-s.stopCh:
		return false
	case <-time.After(d):
		return true
	}
}

// waitForExit waits for the current core to exit. Attached cores (no child
// handle) are polled by PID.
func (s *Supervisor) waitForExit(ctx context.Context, rc *runningCore) exitReason {
	if rc.exited != nil {
		select {
		case <-s.stopCh:
			return exitStopped
		case <-ctx.Done():
			return exitContextDone
		case <-rc.exited:
			return exitDied
		}
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return exitStopped
		case <-ctx.Done():
			return exitContextDone
		case <-ticker.C:
			if !pidAlive(rc.handle.PID) {
				return exitDied
			}
		}
	}
}

// healthLoop writes worker-health.json every 5s until Stop.
func (s *Supervisor) healthLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(healthWriteInterval)
	defer ticker.Stop()
	for {
		s.writeHealth()
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
		}
	}
}

func (s *Supervisor) writeHealth() {
	s.mu.Lock()
	rc := s.current
	restarts := s.total
	lastErr := s.lastErr
	s.mu.Unlock()
	if rc == nil {
		return
	}
	record := healthRecord{
		PID: rc.handle.PID, URL: rc.handle.URL,
		At: time.Now().Unix(), Restarts: restarts, LastError: lastErr,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	_ = atomicWriteFile(healthPath(s.profile), encoded, 0600)
}
