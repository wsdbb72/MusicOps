// Package smoke runs black-box acceptance checks against a built MusicOps
// bundle and a live Go core. It is the Go replacement for the Python
// packaging/smoke_*.py scripts: the checks exercise the same real behaviour
// (a packaged core starts, serves the WebUI behind Basic auth, runs a
// synthetic search into review, transcodes real audio, never persists the
// search cookie) through the core's own HTTP API, with no Python worker,
// no real downloads and no writes to an existing library.
//
// Run is driven by the platform build scripts (and, at release time, by
// the desktop-release workflow) against the bundle they just produced. The
// gated test in smoke_test.go runs the same checks against a locally built
// bundle when MUSICOPS_SMOKE_BUNDLE points at one.
package smoke

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Report is the JSON smoke report the build scripts and the releasegate
// consume. Checks maps each check name to its outcome; Passed is true
// only when every recorded check passed. Skipped lists checks that
// passed vacuously because the thing they verify is absent and not
// required (a core-only bundle has no engines or manifest); the release
// workflow sets RequireEngines/RequireManifest so those checks verify
// for real there.
type Report struct {
	Version  string          `json:"version"`
	Evidence string          `json:"evidence"`
	Passed   bool            `json:"passed"`
	Checks   map[string]bool `json:"checks"`
	Skipped  []string        `json:"skipped,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// Config drives one Run.
type Config struct {
	// BundleDir is the bundle root: dist/MusicOps-Windows, a .app, or the
	// Docker image's /opt/musicops.
	BundleDir string
	// WorkDir is a scratch directory for the run's profile/state. Run
	// creates it when empty.
	WorkDir string
	// Version is the expected version; "" accepts whatever the core
	// binary reports.
	Version string
	// CoreName is the core binary name; "" defaults to musicops[.exe].
	CoreName string
	// DesktopName is the desktop shell binary name; "" means the bundle
	// has no desktop shell (Docker).
	DesktopName string
	// ToolsDir holds the bundled ffmpeg/ffprobe; "" defaults to
	// <BundleDir>/tools.
	ToolsDir string
	// ServicesDir holds the managed engines; "" defaults to
	// <BundleDir>/tools/services.
	ServicesDir string
	// RequireManifest requires manifest.json to exist and verify.
	// RequireEngines requires the managed engines to be present.
	RequireManifest bool
	RequireEngines  bool
	// ManifestRel is the manifest path relative to BundleDir;
	// "" defaults to manifest.json (macOS uses
	// Contents/Resources/manifest.json).
	ManifestRel string
	// Timeout bounds the whole run; 0 means 5 minutes.
	Timeout time.Duration
}

func (c *Config) defaults() {
	if c.CoreName == "" {
		c.CoreName = "musicops"
		if runtime.GOOS == "windows" {
			c.CoreName += ".exe"
		}
	}
	// A macOS .app keeps its binaries under Contents/MacOS, its tools
	// under Contents/Resources/tools and its manifest under
	// Contents/Resources — the same layout the releasegate's "macos"
	// layout uses. Callers may still spell out explicit relative
	// paths (the darwin installer test does); those are used as given.
	isApp := strings.HasSuffix(c.BundleDir, ".app")
	if c.ToolsDir == "" {
		if isApp {
			c.ToolsDir = filepath.Join(c.BundleDir, "Contents", "Resources", "tools")
		} else {
			c.ToolsDir = filepath.Join(c.BundleDir, "tools")
		}
	}
	if c.ServicesDir == "" {
		c.ServicesDir = filepath.Join(c.ToolsDir, "services")
	}
	if c.Timeout <= 0 {
		c.Timeout = 5 * time.Minute
	}
	if c.ManifestRel == "" {
		if isApp {
			c.ManifestRel = filepath.Join("Contents", "Resources", "manifest.json")
		} else {
			c.ManifestRel = "manifest.json"
		}
	}
}

// binRel resolves a binary name inside the bundle: a plain name maps
// into Contents/MacOS for a .app, while a name that already carries a
// relative path (the darwin installer test's "Contents/MacOS/...") is
// used as given.
func (c *Config) binRel(name string) string {
	if filepath.Base(name) != name {
		return name
	}
	if strings.HasSuffix(c.BundleDir, ".app") {
		return filepath.Join("Contents", "MacOS", name)
	}
	return name
}

type runner struct {
	cfg      Config
	ctx      context.Context
	client   *http.Client
	baseURL  string
	password string
	report   Report
}

// Run executes the checks and returns a Report. The first failing check
// does not stop the run: every check is attempted so the report shows the
// full picture, except that the live checks are skipped when the core
// cannot be started at all.
func Run(ctx context.Context, cfg Config) Report {
	cfg.defaults()
	if cfg.BundleDir == "" {
		return Report{Evidence: "go_bundle_smoke", Passed: false, Checks: map[string]bool{}, Error: "BundleDir is required"}
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	r := &runner{cfg: cfg, ctx: ctx, client: &http.Client{Timeout: 15 * time.Second}}
	r.report = Report{Evidence: "go_bundle_smoke", Checks: map[string]bool{}}
	r.check("bundle_no_python", func() error { return CheckNoPython(cfg.BundleDir, cfg.ServicesDir) })
	corePath := filepath.Join(cfg.BundleDir, cfg.binRel(cfg.CoreName))
	r.check("core_present_and_executable", func() error { return checkExecutable(corePath) })
	if cfg.DesktopName != "" {
		desktopPath := filepath.Join(cfg.BundleDir, cfg.binRel(cfg.DesktopName))
		r.check("desktop_present_and_executable", func() error { return checkExecutable(desktopPath) })
	}
	version := ""
	r.check("core_is_go_built", func() error { return CheckGoBuilt(corePath) })
	r.check("core_version", func() error {
		v, err := BinaryVersion(corePath, "version")
		version = v
		return err
	})
	r.report.Version = version
	if cfg.DesktopName != "" {
		desktopPath := filepath.Join(cfg.BundleDir, cfg.binRel(cfg.DesktopName))
		r.check("desktop_is_go_built", func() error { return CheckGoBuilt(desktopPath) })
		r.check("desktop_version", func() error {
			v, err := BinaryVersion(desktopPath, "--version")
			if err != nil {
				return err
			}
			if version != "" && v != version {
				return fmt.Errorf("desktop version %q does not match core version %q", v, version)
			}
			return nil
		})
	}
	if cfg.Version != "" {
		r.report.Checks["core_version_matches"] = version == cfg.Version
	}
	r.check("manifest_hashes_match", func() error { return CheckManifest(cfg.BundleDir, cfg.ManifestRel, cfg.RequireManifest) })
	r.check("engines_present", func() error { return CheckEngines(cfg.ServicesDir, cfg.RequireEngines) })
	r.check("ffmpeg_present", func() error { return checkFFmpeg(cfg.ToolsDir) })
	if !r.report.Checks["core_present_and_executable"] || !r.report.Checks["core_is_go_built"] {
		r.finish()
		return r.report
	}
	if err := r.runLive(); err != nil {
		// runLive records the failing check itself; a setup error that
		// prevented the live phase is reported here.
		if r.report.Error == "" {
			r.report.Error = err.Error()
		}
	}
	r.finish()
	return r.report
}

func (r *runner) finish() {
	r.report.Passed = len(r.report.Checks) > 0
	for _, ok := range r.report.Checks {
		if !ok {
			r.report.Passed = false
		}
	}
}

func (r *runner) check(name string, fn func() error) {
	if err := fn(); err != nil {
		if errors.Is(err, ErrNotApplicable) {
			r.report.Checks[name] = true
			r.report.Skipped = append(r.report.Skipped, name)
			return
		}
		r.report.Checks[name] = false
		if r.report.Error == "" {
			r.report.Error = name + ": " + err.Error()
		}
		return
	}
	r.report.Checks[name] = true
}

// ErrNotApplicable marks a check that passed vacuously: the thing it
// verifies is absent and not required for this bundle.
var ErrNotApplicable = errors.New("not applicable: absent and not required")

// CheckNoPython verifies the MusicOps runtime ships no Python: no .py
// source, no bytecode, no _internal directory, no python DLLs, no
// MusicOps-worker.exe at the bundle root. The managed engines under
// servicesDir are third-party frozen binaries (FlareSolverr is itself
// a PyInstaller-frozen Python app) and are excluded here; they are
// verified separately by CheckEngines.
func CheckNoPython(bundleDir, servicesDir string) error {
	var offenders []string
	err := filepath.WalkDir(bundleDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if servicesDir != "" && d.IsDir() && samePath(path, servicesDir) {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(bundleDir, path)
		if err != nil {
			return err
		}
		name := strings.ToLower(d.Name())
		if d.IsDir() {
			if name == "_internal" || name == "__pycache__" {
				offenders = append(offenders, rel)
			}
			return nil
		}
		switch {
		case strings.HasSuffix(name, ".py") || strings.HasSuffix(name, ".pyc") || strings.HasSuffix(name, ".pyo"):
			offenders = append(offenders, rel)
		case strings.HasPrefix(name, "python") && (strings.HasSuffix(name, ".dll") || strings.HasSuffix(name, ".so") || strings.HasSuffix(name, ".dylib")):
			offenders = append(offenders, rel)
		case name == "musicops-worker.exe" || name == "musicops-worker":
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(offenders) > 0 {
		return fmt.Errorf("python files in bundle: %s", strings.Join(offenders, ", "))
	}
	return nil
}

// samePath reports whether two paths name the same directory, after
// cleaning and best-effort symlink resolution.
func samePath(a, b string) bool {
	ac, bc := filepath.Clean(a), filepath.Clean(b)
	if ac == bc {
		return true
	}
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		ac = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		bc = rb
	}
	return ac == bc
}

func checkExecutable(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	if runtime.GOOS != "windows" && st.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}
	return nil
}

// CheckGoBuilt requires path to be a Go-built binary from this module.
// It reads the build info embedded in the binary itself (debug/buildinfo),
// so it needs no Go toolchain on PATH — the release runners and the gate
// CLI can verify a Windows or macOS binary from any host.
func CheckGoBuilt(path string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("not a Go-built binary: %w", err)
	}
	if info.GoVersion == "" {
		return fmt.Errorf("not a Go-built binary: no Go version in build info")
	}
	if !strings.HasPrefix(info.Path, "github.com/wsdbb72/MusicOps/native/core/") {
		return fmt.Errorf("binary is from module %q, want this module", info.Path)
	}
	return nil
}

// BinaryVersion runs a packaged binary's version command and returns the
// version it reports. The core uses the `version` subcommand, the desktop
// shell uses the --version flag; both print {"version": "..."}.
func BinaryVersion(path string, args ...string) (string, error) {
	out, err := exec.Command(path, args...).Output()
	if err != nil {
		return "", fmt.Errorf("cannot run %s %s: %w", path, strings.Join(args, " "), err)
	}
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &doc); err != nil {
		return "", fmt.Errorf("version output unreadable: %s: %w", out, err)
	}
	if doc.Version == "" {
		return "", fmt.Errorf("version output has no version: %s", out)
	}
	return doc.Version, nil
}

// CheckManifest verifies the manifest at manifestRel (relative to
// bundleDir) when present: a JSON array of {path, size, sha256} (the
// shape build_windows.ps1 writes), each entry hashed against the
// bundle. Entry paths are relative to bundleDir. When no manifest
// exists the check returns ErrNotApplicable unless required is set.
func CheckManifest(bundleDir, manifestRel string, required bool) error {
	raw, err := os.ReadFile(filepath.Join(bundleDir, filepath.FromSlash(manifestRel)))
	if err != nil {
		if os.IsNotExist(err) && !required {
			return ErrNotApplicable
		}
		return err
	}
	var entries []struct {
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("manifest.json is not the expected array: %w", err)
	}
	if len(entries) == 0 {
		return errors.New("manifest.json is empty")
	}
	for _, e := range entries {
		full := filepath.Join(bundleDir, filepath.FromSlash(e.Path))
		data, err := os.ReadFile(full)
		if err != nil {
			return fmt.Errorf("manifest entry %s unreadable: %w", e.Path, err)
		}
		if int64(len(data)) != e.Size {
			return fmt.Errorf("manifest entry %s size %d, want %d", e.Path, len(data), e.Size)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != strings.ToLower(e.SHA256) {
			return fmt.Errorf("manifest entry %s hash mismatch", e.Path)
		}
	}
	return nil
}

// CheckEngines verifies the managed engines layout produced by
// packaging/prepare_engines.py: engines.json plus the go-music-dl and
// flaresolverr binaries, executable, with the browser/driver alongside
// flaresolverr. When the services directory is absent the check passes
// unless RequireEngines is set (a minimal core-only bundle).
func CheckEngines(servicesDir string, required bool) error {
	if _, err := os.Stat(servicesDir); err != nil {
		if os.IsNotExist(err) && !required {
			return ErrNotApplicable
		}
		return err
	}
	raw, err := os.ReadFile(filepath.Join(servicesDir, "engines.json"))
	if err != nil {
		return fmt.Errorf("engines.json missing: %w", err)
	}
	var doc struct {
		Sources       map[string]any `json:"sources"`
		ChromeVersion string         `json:"chrome_version"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("engines.json unreadable: %w", err)
	}
	if len(doc.Sources) == 0 {
		return errors.New("engines.json has no sources")
	}
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	for _, p := range []string{
		filepath.Join(servicesDir, "go-music-dl", "go-music-dl"+exe),
		filepath.Join(servicesDir, "flaresolverr", "flaresolverr"+exe),
	} {
		if err := checkExecutable(p); err != nil {
			return err
		}
	}
	return nil
}

// checkFFmpeg requires ffmpeg and ffprobe to run (from the bundle's
// tools directory or PATH) — the transcode executor resolves them via
// exec.LookPath.
func checkFFmpeg(toolsDir string) error {
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		full, err := exec.LookPath(name)
		if err != nil {
			// LookPath uses the process PATH; retry the bundle's tools
			// directory directly.
			candidate := filepath.Join(toolsDir, name)
			if runtime.GOOS == "windows" {
				candidate += ".exe"
			}
			if _, err := os.Stat(candidate); err != nil {
				return fmt.Errorf("%s not runnable: %w", name, err)
			}
			full = candidate
		}
		cmd := exec.Command(full, "-version")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s -version failed: %w", name, err)
		}
	}
	return nil
}

// runLive starts the packaged core against a scratch profile and runs
// the HTTP checks.
func (r *runner) runLive() error {
	work := r.cfg.WorkDir
	if work == "" {
		var err error
		work, err = os.MkdirTemp("", "musicops-smoke-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(work)
	}
	stateDir := filepath.Join(work, "state")
	backupDir := filepath.Join(work, "backup")
	password, err := randomPassword(32)
	if err != nil {
		return err
	}
	r.password = password
	corePath := filepath.Join(r.cfg.BundleDir, r.cfg.binRel(r.cfg.CoreName))
	cmd := exec.CommandContext(r.ctx, corePath,
		"--state-dir", stateDir, "--backup-dir", backupDir, "--listen", "127.0.0.1:0")
	// The bundled tools directory goes first on PATH so the core finds
	// the bundled ffmpeg/ffprobe, exactly as the desktop supervisor does.
	cmd.Env = append(os.Environ(), "MUSICOPS_APP_PASSWORD="+password,
		"PATH="+r.cfg.ToolsDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cannot start the packaged core: %w", err)
	}
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	// The core prints one ready JSON line, then serves.
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("core printed no ready line: %w (stderr: %s)", err, stderr.String())
	}
	var ready struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(line), &ready); err != nil || ready.URL == "" {
		return fmt.Errorf("core ready line unreadable: %s", line)
	}
	r.baseURL = ready.URL

	r.check("core_starts_and_serves_healthz", func() error { return r.checkHealthz() })
	r.check("webui_requires_auth", func() error { return r.checkAuthRequired() })
	r.check("webui_loads_with_auth", func() error { return r.checkWebUI() })
	r.check("jobs_api_empty", func() error { return r.checkJobsEmpty() })
	r.check("settings_api", func() error { return r.checkSettings() })
	r.check("library_api_empty", func() error { return r.checkLibraryEmpty() })
	r.check("duplicate_scan_empty", func() error { return r.checkDuplicatesEmpty() })
	r.check("parallel_api_calls", func() error { return r.checkParallel() })
	r.check("search_synthetic_end_to_end", func() error { return r.checkSearch(stateDir) })
	r.check("review_skip", func() error { return r.checkReviewSkip() })
	r.check("cookie_never_persisted", func() error { return checkCookieAbsent(stateDir, searchCookie) })
	r.check("transcode_roundtrip", func() error { return r.checkTranscode(stateDir) })
	return nil
}

func randomPassword(n int) (string, error) {
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

func (r *runner) do(method, path string, body any, auth bool) (int, map[string]any, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(r.ctx, method, r.baseURL+path, reader)
	if err != nil {
		return 0, nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.SetBasicAuth("musicops", r.password)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, nil, err
	}
	var decoded map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return resp.StatusCode, decoded, raw, nil
}

func (r *runner) checkHealthz() error {
	status, body, _, err := r.do(http.MethodGet, "/healthz", nil, false)
	if err != nil {
		return err
	}
	if status != http.StatusOK || body["ok"] != true || body["application"] != "musicops" || body["python"] != false {
		return fmt.Errorf("healthz status %d body %v", status, body)
	}
	return nil
}

func (r *runner) checkAuthRequired() error {
	status, _, _, err := r.do(http.MethodGet, "/", nil, false)
	if err != nil {
		return err
	}
	if status != http.StatusUnauthorized {
		return fmt.Errorf("GET / without auth returned %d, want 401", status)
	}
	return nil
}

func (r *runner) checkWebUI() error {
	status, _, raw, err := r.do(http.MethodGet, "/", nil, true)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("GET / with auth returned %d", status)
	}
	if !bytes.Contains(raw, []byte("/app.js")) {
		return errors.New("WebUI does not reference app.js")
	}
	return nil
}

func (r *runner) checkJobsEmpty() error {
	status, body, _, err := r.do(http.MethodGet, "/v1/jobs", nil, true)
	if err != nil {
		return err
	}
	if status != http.StatusOK || body["ok"] != true || body["python"] != false {
		return fmt.Errorf("jobs status %d body %v", status, body)
	}
	jobs, _ := body["jobs"].([]any)
	if len(jobs) != 0 {
		return fmt.Errorf("fresh core lists %d jobs", len(jobs))
	}
	return nil
}

func (r *runner) checkSettings() error {
	status, body, _, err := r.do(http.MethodGet, "/v1/settings/download", nil, true)
	if err != nil {
		return err
	}
	if status != http.StatusOK || body["ok"] != true || body["streaming_fallback_wired"] != true {
		return fmt.Errorf("settings status %d body %v", status, body)
	}
	return nil
}

func (r *runner) checkLibraryEmpty() error {
	status, body, _, err := r.do(http.MethodGet, "/v1/library", nil, true)
	if err != nil {
		return err
	}
	if status != http.StatusOK || body["ok"] != true {
		return fmt.Errorf("library status %d body %v", status, body)
	}
	if count, _ := body["count"].(float64); count != 0 {
		return fmt.Errorf("fresh core library count %v", count)
	}
	return nil
}

func (r *runner) checkDuplicatesEmpty() error {
	status, body, _, err := r.do(http.MethodGet, "/v1/library/duplicates", nil, true)
	if err != nil {
		return err
	}
	if status != http.StatusOK || body["ok"] != true {
		return fmt.Errorf("duplicates status %d body %v", status, body)
	}
	if count, _ := body["count"].(float64); count != 0 {
		return fmt.Errorf("fresh core duplicate groups %v", count)
	}
	return nil
}

func (r *runner) checkParallel() error {
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _, _, err := r.do(http.MethodGet, "/v1/jobs", nil, true)
			if err != nil {
				errs <- err
				return
			}
			if status != http.StatusOK {
				errs <- fmt.Errorf("parallel GET /v1/jobs returned %d", status)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return err
	}
	if time.Since(start) >= 5*time.Second {
		return fmt.Errorf("6 parallel calls took %s", time.Since(start))
	}
	return nil
}

// searchCookie is the session cookie the synthetic search carries. It
// must never reach the disk: the core keeps unreviewed secrets in memory
// only.
const searchCookie = "session=cookie-secret-not-stored"

// sampleTorrent is a minimal valid .torrent (bencoded, one 1-byte file),
// the same fixture the app's own tests use for free downloads.
const sampleTorrent = "d8:announce12:http://a.com4:infod6:lengthi1e4:name4:test12:piece lengthi16384e6:pieces20:01234567890123456789ee"

func (r *runner) checkSearch(stateDir string) error {
	// The fake tracker mirrors the app's own free-download fixture:
	// open.cd reports one free torrent (Album, id 11), its detail page
	// proves global freeleech, and /download.php serves the .torrent.
	// The other three sites report no results.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-MusicOps-Site") == "open.cd" {
			switch {
			case strings.HasPrefix(req.URL.Path, "/download.php"):
				_, _ = io.WriteString(w, sampleTorrent)
				return
			case req.URL.Path == "/details.php":
				_, _ = io.WriteString(w, `<tr id="torrent11"><td><strong class="tl_free">免费</strong></td></tr>`)
				return
			default:
				_, _ = io.WriteString(w, `<html><a href="logout.php">logout</a><table><tr><td><table><tr><td><a href="details.php?id=11" title="Album">Album</a></td></tr></table></td><td>100 MiB</td></tr></table></html>`)
				return
			}
		}
		_, _ = io.WriteString(w, `{"status":"success","response":{"results":[],"pages":1}}`)
	}))
	defer fake.Close()
	body := map[string]any{
		"job_id": "smoke-search", "batch_id": "batch-1", "scene": "playlist",
		"query": "Artist Album", "expected_title": "Album",
		"source_track_ids":  []string{"track-1"},
		"enabled_sites":     []string{"open.cd", "dicmusic", "orpheus", "redacted"},
		"streaming_enabled": false, "synthetic": true, "base_url": fake.URL,
		"cookie":   searchCookie,
		"identity": map[string]any{"evidence_id": "ev-smoke", "kind": "catalog", "source": "musicbrainz", "subject_id": "release-1", "tracklist_verified": true, "identity_confirmed": true, "payload": "catalogue-row"},
	}
	status, _, _, err := r.do(http.MethodPost, "/v1/search", body, true)
	if err != nil {
		return err
	}
	if status != http.StatusAccepted && status != http.StatusOK {
		return fmt.Errorf("POST /v1/search returned %d", status)
	}
	// The posted job_id is the parent; the workflow creates
	// search:<id> and then process:search:<id> beneath it.
	if _, err := r.waitJobStage("search:smoke-search", "completed", 60*time.Second); err != nil {
		return fmt.Errorf("search job: %w", err)
	}
	child, err := r.waitJobStage("process:search:smoke-search", "needs_review", 30*time.Second)
	if err != nil {
		return fmt.Errorf("acquire child: %w", err)
	}
	if msg, _ := child["message"].(string); !strings.Contains(msg, "尚未下载") {
		return fmt.Errorf("acquire child message %q does not say the download is pending", msg)
	}
	// The free .torrent must actually be on disk in the core's data
	// directory; the review message alone is not proof.
	matches, err := filepath.Glob(filepath.Join(stateDir, "torrents", "*.torrent"))
	if err != nil || len(matches) != 1 {
		return fmt.Errorf("expected exactly one saved .torrent, got %v (%v)", matches, err)
	}
	return nil
}

// waitJobStage polls GET /v1/jobs/{id} until the job reaches stage, and
// returns the job view. A failed job is an error, not a wait.
func (r *runner) waitJobStage(id, stage string, timeout time.Duration) (map[string]any, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, body, _, err := r.do(http.MethodGet, "/v1/jobs/"+id, nil, true)
		if err != nil {
			return nil, err
		}
		if status == http.StatusNotFound {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("GET /v1/jobs/%s returned %d", id, status)
		}
		inspection, _ := body["inspection"].(map[string]any)
		job, _ := inspection["job"].(map[string]any)
		if job == nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if job["stage"] == stage {
			return job, nil
		}
		if job["stage"] == "failed" {
			return nil, fmt.Errorf("job %s failed: %v", id, job)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil, fmt.Errorf("job %s did not reach %s within %s", id, stage, timeout)
}

func (r *runner) checkReviewSkip() error {
	status, body, _, err := r.do(http.MethodGet, "/v1/jobs/process:search:smoke-search/review", nil, true)
	if err != nil {
		return err
	}
	if status != http.StatusOK || body["ok"] != true {
		return fmt.Errorf("review data status %d body %v", status, body)
	}
	if body["review_kind"] != "candidate" {
		return fmt.Errorf("review kind %v, want candidate", body["review_kind"])
	}
	status, _, _, err = r.do(http.MethodPost, "/v1/jobs/process:search:smoke-search/review", map[string]any{"action": "skip"}, true)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("review skip returned %d", status)
	}
	if _, err := r.waitJobStage("process:search:smoke-search", "skipped", 15*time.Second); err != nil {
		return fmt.Errorf("skipped child: %w", err)
	}
	return nil
}

// checkCookieAbsent walks the state directory and fails when any file
// contains the search cookie value.
func checkCookieAbsent(stateDir, cookie string) error {
	var offenders []string
	err := filepath.WalkDir(stateDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if bytes.Contains(data, []byte(cookie)) {
			rel, _ := filepath.Rel(stateDir, path)
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(offenders) > 0 {
		return fmt.Errorf("cookie persisted in: %s", strings.Join(offenders, ", "))
	}
	return nil
}

func (r *runner) checkTranscode(stateDir string) error {
	// The core only accepts sources inside its own data directory, and
	// the store refuses files placed before startup, so the WAV is
	// written after the server is up.
	incoming := filepath.Join(stateDir, "incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		return err
	}
	source := filepath.Join(incoming, "smoke.wav")
	if err := writeTestWAV(source); err != nil {
		return err
	}
	status, body, _, err := r.do(http.MethodPost, "/v1/library/transcode",
		map[string]any{"request_id": "smoke-tc-1", "source": "incoming/smoke.wav", "format": "flac"}, true)
	if err != nil {
		return err
	}
	if status != http.StatusAccepted || body["ok"] != true {
		return fmt.Errorf("transcode enqueue status %d body %v", status, body)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		status, body, _, err := r.do(http.MethodGet, "/v1/library/transcode", nil, true)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("transcode list status %d", status)
		}
		receipts, _ := body["receipts"].([]any)
		for _, item := range receipts {
			receipt, _ := item.(map[string]any)
			if receipt["request_id"] != "smoke-tc-1" {
				continue
			}
			if receipt["sha256"] == "" || receipt["pcm_sha256"] == "" {
				return fmt.Errorf("transcode receipt is missing hashes: %v", receipt)
			}
			output, _ := receipt["output"].(string)
			if output == "" {
				return fmt.Errorf("transcode receipt has no output: %v", receipt)
			}
			if !filepath.IsAbs(output) {
				output = filepath.Join(stateDir, output)
			}
			if _, err := os.Stat(output); err != nil {
				return fmt.Errorf("transcode output %s missing: %w", output, err)
			}
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("transcode receipt never appeared for smoke-tc-1")
}

// writeTestWAV writes 1 second of 8kHz mono 16-bit PCM sine audio as a
// plain 44-byte-header WAV.
func writeTestWAV(path string) error {
	const sampleRate = 8000
	const samples = sampleRate
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36+samples*2))
	buf.WriteString("WAVEfmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // mono
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(samples*2))
	for i := 0; i < samples; i++ {
		v := int16(math.Sin(2*math.Pi*440*float64(i)/sampleRate) * 12000)
		_ = binary.Write(&buf, binary.LittleEndian, v)
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}
