// Command docker-smoke drives the Docker CLI to run the container
// acceptance checks for a built MusicOps image. It is the Go replacement
// for docker/smoke.py: the same real behaviour (a built image starts,
// serves the WebUI behind Basic auth with the generated
// /config/web-password, reports an empty library, survives a restart
// with its password intact), exercised through the container and the
// core's own HTTP API, with no Python, no real downloads and no writes
// to an existing library.
//
// The bundle checks reuse internal/smoke's exported helpers against a
// copy of /opt/musicops extracted from the image (docker create +
// docker cp), so the no-Python / Go-built / version / manifest / engines
// verdicts are the same code the release gate runs.
//
// Usage:
//
//	docker-smoke --image <image> [--tag <version>] [--report <path>] [--keep]
//
// --tag is the expected version (from the release process); when empty,
// any version the core reports is accepted. The JSON report
// {"passed": ..., "checks": {...}} goes to stdout, and to --report when
// given. Exit 0 when every check passed, 1 otherwise, 2 on flag misuse.
//
// This program needs the docker CLI and a Linux amd64 host to run the
// extracted amd64 core binary for the host-side version check; the
// in-container `docker exec musicops version` check is the portable one
// and also runs on other hosts.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/smoke"
	"github.com/wsdbb72/MusicOps/native/core/internal/version"
)

type options struct {
	image      string
	tag        string
	reportPath string
	keep       bool
}

type report struct {
	Passed       bool            `json:"passed"`
	Checks       map[string]bool `json:"checks"`
	Skipped      []string        `json:"skipped,omitempty"`
	Image        string          `json:"image"`
	Version      string          `json:"version,omitempty"`
	SmokeVersion string          `json:"smoke_version,omitempty"`
	Error        string          `json:"error,omitempty"`
}

type runner struct {
	opts      options
	checks    map[string]bool
	skipped   []string
	err       string
	password  string // generated /config/web-password, never logged
	tmpDir    string
	configDir string
	mediaDir  string
	bundleDir string
	name      string
	client    *http.Client
	baseURL   string
	version   string
}

func main() {
	os.Exit(run())
}

func run() int {
	opts := options{}
	flag.StringVar(&opts.image, "image", "", "built image to test (required)")
	flag.StringVar(&opts.tag, "tag", "", "expected version reported by the image's core")
	flag.StringVar(&opts.reportPath, "report", "", "also write the JSON report to this file")
	flag.BoolVar(&opts.keep, "keep", false, "keep the container and temp dirs for debugging")
	flag.Parse()
	if opts.image == "" {
		fmt.Fprintln(os.Stderr, "docker-smoke: --image is required")
		flag.Usage()
		return 2
	}
	r := &runner{
		opts:   opts,
		checks: map[string]bool{},
		client: &http.Client{Timeout: 15 * time.Second},
	}
	return r.run()
}

func (r *runner) run() int {
	tmp, err := os.MkdirTemp("", "musicops-docker-smoke-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "docker-smoke: cannot create temp dir: %v\n", err)
		return 1
	}
	r.tmpDir = tmp
	if !r.opts.keep {
		defer os.RemoveAll(tmp)
	}
	r.configDir = filepath.Join(tmp, "config")
	r.mediaDir = filepath.Join(tmp, "media")
	r.bundleDir = filepath.Join(tmp, "bundle")
	for _, dir := range []string{r.configDir, r.mediaDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "docker-smoke: cannot create %s: %v\n", dir, err)
			return 1
		}
	}
	r.name = containerNameFor(os.Getpid())
	if !r.opts.keep {
		defer func() {
			_, _ = r.docker("rm", "-f", r.name)
			_, _ = r.docker("rm", "-f", r.name+"-extract")
		}()
	}
	if err := r.extractBundle(); err != nil {
		r.recordErr("extract_bundle", err)
	} else {
		r.checkBundle()
		r.runContainerChecks()
	}
	rep := r.finishReport()
	raw, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(raw))
	if r.opts.reportPath != "" {
		if err := os.WriteFile(r.opts.reportPath, append(raw, '\n'), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "docker-smoke: cannot write report: %v\n", err)
			return 1
		}
	}
	if !rep.Passed {
		r.printLogs()
		return 1
	}
	return 0
}

// record stores one check outcome; the first failure also becomes the
// report's error so a failing run says which check failed first.
func (r *runner) record(name string, ok bool) {
	r.checks[name] = ok
	if !ok && r.err == "" {
		r.err = name + ": check failed"
	}
}

func (r *runner) recordErr(name string, err error) {
	if err != nil {
		r.record(name, false)
		if r.err == "" {
			r.err = name + ": " + err.Error()
		}
		return
	}
	r.record(name, true)
}

func (r *runner) finishReport() report {
	rep := report{
		Passed:       len(r.checks) > 0,
		Checks:       r.checks,
		Skipped:      r.skipped,
		Image:        r.opts.image,
		Version:      r.version,
		SmokeVersion: version.Version,
		Error:        r.err,
	}
	for _, ok := range r.checks {
		if !ok {
			rep.Passed = false
		}
	}
	return rep
}

// docker runs the docker CLI and returns its combined output, trimmed.
// Combined output keeps the failure reason in the returned error.
func (r *runner) docker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		return trimmed, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, trimmed)
	}
	return trimmed, nil
}

// extractBundle copies /opt/musicops out of a created (never started)
// container so the smoke helpers can verify the bundle on the host.
func (r *runner) extractBundle() error {
	extract := r.name + "-extract"
	if _, err := r.docker("create", "--name", extract, "--platform", "linux/amd64", r.opts.image); err != nil {
		return fmt.Errorf("docker create: %w", err)
	}
	if _, err := r.docker("cp", extract+":/opt/musicops", r.bundleDir); err != nil {
		return fmt.Errorf("docker cp /opt/musicops: %w", err)
	}
	if _, err := r.docker("rm", extract); err != nil {
		return fmt.Errorf("docker rm extract container: %w", err)
	}
	return nil
}

// checkBundle runs the shared smoke helpers over the extracted bundle.
func (r *runner) checkBundle() {
	services := filepath.Join(r.bundleDir, "tools", "services")
	r.recordErr("bundle_no_python", smoke.CheckNoPython(r.bundleDir, services))
	corePath := filepath.Join(r.bundleDir, "musicops")
	r.recordErr("core_is_go_built", smoke.CheckGoBuilt(corePath))
	if runtime.GOARCH == "amd64" {
		// The extracted core is an amd64 binary; it only runs on an
		// amd64 host. The in-container version check below is the
		// portable one.
		v, err := smoke.BinaryVersion(corePath, "version")
		r.version = v
		r.recordErr("core_version", err)
	} else {
		r.record("core_version", true)
		r.skipped = append(r.skipped, "core_version")
	}
	if r.opts.tag != "" {
		r.record("core_version_matches_tag", r.version == r.opts.tag)
	}
	if err := smoke.CheckManifest(r.bundleDir, "manifest.json", false); err != nil {
		if errors.Is(err, smoke.ErrNotApplicable) {
			// The Docker bundle carries no manifest; that is allowed.
			r.record("manifest_hashes_match", true)
			r.skipped = append(r.skipped, "manifest_hashes_match")
		} else {
			r.recordErr("manifest_hashes_match", err)
		}
	} else {
		r.record("manifest_hashes_match", true)
	}
	// The Docker image must carry the managed engines.
	r.recordErr("engines_present", smoke.CheckEngines(services, true))
}

// runContainerChecks starts the image, exercises it live, stops it
// cleanly, and restarts it against the same /config.
func (r *runner) runContainerChecks() {
	if _, err := r.docker("run", "-d", "--name", r.name, "--platform", "linux/amd64", "--shm-size", "1g",
		"-p", "127.0.0.1::8080",
		"-v", r.configDir+":/config",
		"-v", r.mediaDir+":/media",
		r.opts.image); err != nil {
		r.recordErr("container_starts", err)
		return
	}
	r.record("container_starts", true)
	if err := r.waitHealthy(120 * time.Second); err != nil {
		r.recordErr("container_healthy", err)
		return
	}
	r.record("container_healthy", true)
	info, err := r.inspect()
	if err != nil {
		r.recordErr("container_inspect", err)
		return
	}
	if info.HostPort == "" {
		r.record("container_port_mapped", false)
		return
	}
	r.record("container_port_mapped", true)
	r.baseURL = "http://127.0.0.1:" + info.HostPort

	out, err := r.docker("exec", r.name, "id", "-u")
	r.record("container_uid_1000", err == nil && strings.TrimSpace(out) == "1000")
	out, err = r.docker("image", "inspect", "--format", "{{.Architecture}}", r.opts.image)
	r.record("image_amd64", err == nil && strings.TrimSpace(out) == "amd64")

	// The generated password is read into memory only, never logged.
	pw, err := r.docker("exec", r.name, "cat", "/config/web-password")
	pw = strings.TrimSpace(pw)
	if err == nil && pw != "" {
		r.password = pw
	}
	r.record("web_password_present", err == nil && pw != "")
	mode, err := r.docker("exec", r.name, "stat", "-c", "%a", "/config/web-password")
	r.record("web_password_0600", err == nil && modeIs600(mode))

	status, _, _, err := r.get("/", false)
	r.record("auth_required", err == nil && status == http.StatusUnauthorized)
	status, _, body, err := r.get("/", true)
	r.record("webui_loads", err == nil && status == http.StatusOK && bytes.Contains(body, []byte("app.js")))
	status, _, healthBody, err := r.get("/healthz", false)
	r.record("healthz_ok", err == nil && status == http.StatusOK && healthzOK(healthBody))

	// Version from inside the running container; must agree with the
	// extracted binary and, when given, with --tag.
	vout, err := r.docker("exec", r.name, "musicops", "version")
	ver, verr := parseVersionOutput([]byte(vout))
	versionOK := err == nil && verr == nil && ver != ""
	if r.opts.tag != "" {
		versionOK = versionOK && ver == r.opts.tag
	}
	if r.version != "" {
		versionOK = versionOK && ver == r.version
	}
	r.record("version_matches", versionOK)

	status, doc, _, err := r.get("/v1/jobs", true)
	jobsOK := err == nil && status == http.StatusOK && doc["ok"] == true
	if jobs, ok := doc["jobs"].([]any); ok && len(jobs) != 0 {
		jobsOK = false
	}
	r.record("jobs_empty", jobsOK)
	status, doc, _, err = r.get("/v1/library", true)
	libraryOK := err == nil && status == http.StatusOK && doc["ok"] == true
	if count, ok := doc["count"].(float64); ok && count != 0 {
		libraryOK = false
	}
	r.record("library_empty", libraryOK)
	status, doc, _, err = r.get("/v1/settings/download", true)
	r.record("settings_download_ok", err == nil && status == http.StatusOK && doc["ok"] == true && doc["streaming_fallback_wired"] == true)

	out, err = r.docker("exec", r.name, "sh", "-c", "grep -l -e websockify -e x11vnc /proc/[0-9]*/cmdline 2>/dev/null || true")
	r.record("no_vnc_processes", err == nil && strings.TrimSpace(out) == "")
	_, errFFmpeg := r.docker("exec", r.name, "ffmpeg", "-version")
	_, errFFprobe := r.docker("exec", r.name, "ffprobe", "-version")
	r.record("ffmpeg_ffprobe", errFFmpeg == nil && errFFprobe == nil)

	// Clean shutdown, then restart against the same /config.
	if _, err := r.docker("stop", "--time", "120", r.name); err != nil {
		r.recordErr("clean_shutdown", err)
		return
	}
	stopped, err := r.inspect()
	if err != nil {
		r.recordErr("clean_shutdown", err)
		return
	}
	r.record("clean_shutdown", stopped.ExitCode == 0)
	if _, err := r.docker("start", r.name); err != nil {
		r.recordErr("container_restarts", err)
		return
	}
	r.record("container_restarts", true)
	if err := r.waitHealthy(120 * time.Second); err != nil {
		r.recordErr("restart_healthy", err)
		return
	}
	r.record("restart_healthy", true)
	pw2, err := r.docker("exec", r.name, "cat", "/config/web-password")
	r.record("web_password_preserved", err == nil && r.password != "" && strings.TrimSpace(pw2) == r.password)
	status, _, healthBody, err = r.get("/healthz", false)
	r.record("restart_healthz", err == nil && status == http.StatusOK && healthzOK(healthBody))
	status, _, _, err = r.get("/", true)
	r.record("restart_auth_works", err == nil && status == http.StatusOK)
}

// waitHealthy polls the container until its Docker health status is
// healthy, or until timeout. A container with no Health block (health
// check disabled) is probed directly with `musicops healthcheck`.
func (r *runner) waitHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		info, err := r.inspect()
		if err != nil {
			return err
		}
		if !info.Running {
			return fmt.Errorf("container exited (exit code %d)", info.ExitCode)
		}
		healthy := false
		if info.Healthy != nil {
			healthy = *info.Healthy
		} else if _, err := r.docker("exec", r.name, "musicops", "healthcheck", "--addr", "127.0.0.1:8080"); err == nil {
			healthy = true
		}
		if healthy {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("container did not become healthy within %s", timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// inspect returns the container's running state, health status (nil when
// the container has no Health block) and mapped 8080 host port.
func (r *runner) inspect() (inspectData, error) {
	raw, err := r.docker("inspect", r.name)
	if err != nil {
		return inspectData{}, err
	}
	return parseInspect([]byte(raw))
}

// get performs one GET against the mapped host port.
func (r *runner) get(path string, auth bool) (int, map[string]any, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, r.baseURL+path, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	if auth {
		req.Header.Set("Authorization", authHeader("musicops", r.password))
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
	var doc map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &doc)
	}
	return resp.StatusCode, doc, raw, nil
}

// printLogs dumps the tail of the container log to stderr with the
// generated password redacted, for a failing CI run to debug.
func (r *runner) printLogs() {
	out, err := r.docker("logs", r.name)
	if err != nil {
		return
	}
	out = redact(out, r.password)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > 200 {
		lines = lines[len(lines)-200:]
	}
	fmt.Fprintln(os.Stderr, "--- container logs (last 200 lines, password redacted) ---")
	fmt.Fprintln(os.Stderr, strings.Join(lines, "\n"))
}

// inspectData is the subset of `docker inspect` this program reads.
type inspectData struct {
	Running  bool
	ExitCode int
	Healthy  *bool // nil when the container has no Health block
	HostPort string
}

func parseInspect(raw []byte) (inspectData, error) {
	var docs []struct {
		State struct {
			Running  bool `json:"Running"`
			ExitCode int  `json:"ExitCode"`
			Health   *struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
		NetworkSettings struct {
			Ports map[string][]struct {
				HostPort string `json:"HostPort"`
			} `json:"Ports"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(raw, &docs); err != nil {
		return inspectData{}, err
	}
	if len(docs) == 0 {
		return inspectData{}, errors.New("docker inspect returned no container")
	}
	info := inspectData{Running: docs[0].State.Running, ExitCode: docs[0].State.ExitCode}
	if h := docs[0].State.Health; h != nil {
		healthy := h.Status == "healthy"
		info.Healthy = &healthy
	}
	for _, binding := range docs[0].NetworkSettings.Ports["8080/tcp"] {
		if binding.HostPort != "" {
			info.HostPort = binding.HostPort
			break
		}
	}
	return info, nil
}

// parseVersionOutput parses the {"version": "..."} a `musicops version`
// call prints.
func parseVersionOutput(out []byte) (string, error) {
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &doc); err != nil {
		return "", fmt.Errorf("version output unreadable: %s: %w", strings.TrimSpace(string(out)), err)
	}
	if doc.Version == "" {
		return "", fmt.Errorf("version output has no version: %s", strings.TrimSpace(string(out)))
	}
	return doc.Version, nil
}

// healthzOK checks the /healthz body the core's healthcheck requires:
// 200 + ok:true + application:musicops + python:false. The python field
// must be present and false: a plain bool would read a missing field as
// false and accept a body that is not a Go-core healthz response at all.
func healthzOK(body []byte) bool {
	var doc struct {
		OK          bool   `json:"ok"`
		Application string `json:"application"`
		Python      *bool  `json:"python"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return false
	}
	return doc.OK && doc.Application == "musicops" && doc.Python != nil && !*doc.Python
}

// redact replaces the generated password with [MASKED] before anything
// reaches a log. An empty secret redacts nothing.
func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[MASKED]")
}

// modeIs600 reports whether a `stat -c %a` mode string is exactly 600.
func modeIs600(mode string) bool {
	return strings.TrimSpace(mode) == "600"
}

func authHeader(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

func containerNameFor(pid int) string {
	return fmt.Sprintf("musicops-docker-smoke-%d", pid)
}
