package app

// Bundled engines: the installer ships go-music-dl and FlareSolverr under
// tools/services, but a binary on disk does nothing until the app starts it
// with the environment those builds expect. This supervisor is that starter:
// it picks a loopback port, exports the contract from packaging/flare_entry.py
// (MUSICOPS_ENGINE_DATA, MUSICOPS_SERVICE_TOKEN, HEADLESS, HOST, PORT) and the
// go-music-dl one (web --desktop --no-browser --port, /music base), waits for
// the health endpoint, and stops what it started when the app closes.
//
// Requests to a managed engine must carry X-MusicOps-Service: <token>; both
// wrappers answer 403 without it. A user-supplied external engine has no
// token, so the header is only sent for a process this app started.

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/netproxy"
)

// engineName identifies one bundled service.
type engineName string

const (
	engineStreaming engineName = "go-music-dl"
	engineSolver    engineName = "flaresolverr"
	// engineTokenHeader is the header both wrappers require.
	engineTokenHeader = "X-MusicOps-Service"
)

// engineProcess is one running managed engine.
type engineProcess struct {
	cmd    *exec.Cmd
	base   string
	token  string
	state  string
	note   string
	proxy  string
	active int
}

// servicesRoot is the directory the installer put the engines in. A test or a
// portable run can point MUSICOPS_SERVICES_DIR somewhere else.
func (a *App) servicesRoot() string {
	if override := strings.TrimSpace(os.Getenv("MUSICOPS_SERVICES_DIR")); override != "" {
		return override
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "tools", "services")
}

// engineBinary returns the bundled executable for one engine.
func (a *App) engineBinary(name engineName) (string, bool) {
	root := a.servicesRoot()
	if root == "" {
		return "", false
	}
	suffix := ""
	if filepath.Separator == '\\' {
		suffix = ".exe"
	}
	path := filepath.Join(root, string(name), string(name)+suffix)
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "", false
	}
	return path, true
}

// engineProfileDir is the writable directory an engine gets: the chromedriver
// copy and the settings database live there, never in the install directory.
func (a *App) engineProfileDir(name engineName) string {
	dir := filepath.Join(a.Dir, "services", string(name))
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// freeLoopbackPort picks a port and releases it for the child to bind.
func freeLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// ensureEngine starts one bundled engine (or reuses the running one) and
// returns its base URL and token.
func (a *App) ensureEngine(name engineName) (string, string, error) {
	a.engineMu.Lock()
	defer a.engineMu.Unlock()
	if a.engines == nil {
		a.engines = map[engineName]*engineProcess{}
	}
	if running := a.engines[name]; running != nil && running.cmd != nil && running.cmd.Process != nil {
		if running.cmd.ProcessState == nil {
			current, _ := netproxy.Current()
			if name != engineStreaming || running.proxy == current {
				return running.base, running.token, nil
			}
			if running.active > 0 {
				// Keep the current download alive on its old transport. The
				// request completion hook retires it as soon as the child is idle.
				return running.base, running.token, nil
			}
			delete(a.engines, name)
			_ = running.cmd.Process.Kill()
		} else {
			delete(a.engines, name)
		}
	}
	binary, ok := a.engineBinary(name)
	if !ok {
		return "", "", fmt.Errorf("这个安装包缺少内置服务：%s", name)
	}
	port, err := freeLoopbackPort()
	if err != nil {
		return "", "", err
	}
	token, err := randomToken()
	if err != nil {
		return "", "", err
	}
	dir := a.engineProfileDir(name)
	temp := filepath.Join(dir, "temp")
	_ = os.MkdirAll(temp, 0o700)

	args := []string{}
	if name == engineStreaming {
		args = []string{"web", "--desktop", "--no-browser", "--port", fmt.Sprint(port)}
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	baseEnv := netproxy.ChildEnvironment(os.Environ())
	if name == engineSolver {
		// FlareSolverr receives the proxy on each request. A process-level
		// PROXY_URL would survive a later settings change and silently override
		// a request that intentionally disables the proxy.
		baseEnv = netproxy.SanitizedEnvironment(os.Environ())
	}
	cmd.Env = append(baseEnv,
		"HOST=127.0.0.1",
		"PORT="+fmt.Sprint(port),
		"MUSICOPS_SERVICE_TOKEN="+token,
		"MUSICOPS_ENGINE_DATA="+dir,
		"LOG_LEVEL=error",
		"LOG_HTML=false",
		"HEADLESS=true",
		"MUSIC_DL_CONFIG_DB="+filepath.Join(dir, "settings.db"),
		"MUSIC_DL_COOKIE_FILE="+filepath.Join(dir, "cookies.json"),
		"TEMP="+temp,
		"TMP="+temp,
		"TMPDIR="+temp,
	)
	// The bundled ffmpeg is what go-music-dl calls for its own conversions.
	if tools, ok := a.bundledTools(); ok {
		cmd.Env = append(cmd.Env, "MUSIC_DL_FFMPEG="+filepath.Join(tools, "ffmpeg.exe"), "MUSIC_DL_FFPROBE="+filepath.Join(tools, "ffprobe.exe"))
	}
	prepareSolverCommand(cmd)
	if err := cmd.Start(); err != nil {
		return "", "", err
	}
	base := "http://127.0.0.1:" + fmt.Sprint(port)
	if name == engineStreaming {
		base += "/music"
	}
	proxy, _ := netproxy.Current()
	entry := &engineProcess{cmd: cmd, base: base, token: token, state: "starting", proxy: proxy}
	a.engines[name] = entry
	// Reap the child so a stopped engine does not linger as a zombie.
	go func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(120 * time.Second)
	if name == engineStreaming {
		deadline = time.Now().Add(60 * time.Second)
	}
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil {
			entry.state, entry.note = "failed", "启动后立即退出"
			return "", "", errors.New("内置服务启动失败：" + string(name))
		}
		if engineHealthy(base, token) {
			entry.state = "ready"
			return base, token, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	entry.state, entry.note = "failed", "等待健康检查超时"
	return "", "", errors.New("内置服务没有在预期时间内就绪：" + string(name))
}

// engineHealthy probes the health endpoint with the service token.
func engineHealthy(base, token string) bool {
	path := "/health"
	if strings.HasSuffix(base, "/music") {
		path = "/healthz"
	}
	request, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return false
	}
	request.Header.Set(engineTokenHeader, token)
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}

// retireStreamingEngineForProxyChange stops an idle downloader so its next
// launch inherits the new outbound environment. An active request is left
// alone; finishEngineRequest performs the same retirement when it completes.
func (a *App) retireStreamingEngineForProxyChange() {
	current, _ := netproxy.Current()
	a.engineMu.Lock()
	entry := a.engines[engineStreaming]
	if entry == nil || entry.proxy == current || entry.active > 0 {
		a.engineMu.Unlock()
		return
	}
	delete(a.engines, engineStreaming)
	a.engineMu.Unlock()
	killEngineProcess(entry)
}

// beginEngineRequest counts one managed-engine call. The returned function
// retires the process after both the request and the old proxy generation have
// finished. External engines are not in the map and no tracking is performed.
func (a *App) beginEngineRequest(base string) func() {
	a.engineMu.Lock()
	var entry *engineProcess
	for _, candidate := range a.engines {
		if candidate != nil && candidate.base == base {
			entry = candidate
			entry.active++
			break
		}
	}
	a.engineMu.Unlock()
	if entry == nil {
		return func() {}
	}
	return func() {
		current, _ := netproxy.Current()
		a.engineMu.Lock()
		entry.active--
		retire := entry.active == 0 && entry.proxy != current
		if retire {
			for name, candidate := range a.engines {
				if candidate == entry {
					delete(a.engines, name)
					break
				}
			}
		}
		a.engineMu.Unlock()
		if retire {
			killEngineProcess(entry)
		}
	}
}

func killEngineProcess(entry *engineProcess) {
	if entry != nil && entry.cmd != nil && entry.cmd.Process != nil {
		_ = entry.cmd.Process.Kill()
	}
}

// stopEngines terminates only the processes this app started.
func (a *App) stopEngines() {
	a.engineMu.Lock()
	entries := make([]*engineProcess, 0, len(a.engines))
	for name, entry := range a.engines {
		entries = append(entries, entry)
		delete(a.engines, name)
	}
	a.engineMu.Unlock()
	for _, entry := range entries {
		if entry != nil && entry.cmd != nil && entry.cmd.Process != nil {
			_ = entry.cmd.Process.Kill()
		}
	}
}

// bundledTools is the tools directory next to the engines (ffmpeg, ffprobe).
func (a *App) bundledTools() (string, bool) {
	root := a.servicesRoot()
	if root == "" {
		return "", false
	}
	tools := filepath.Dir(root)
	if info, err := os.Stat(filepath.Join(tools, "ffmpeg.exe")); err != nil || info.IsDir() {
		return "", false
	}
	return tools, true
}

// engineStatus reports the managed engines for the settings view.
func (a *App) engineStatus() []map[string]any {
	a.engineMu.Lock()
	defer a.engineMu.Unlock()
	out := []map[string]any{}
	for _, name := range []engineName{engineStreaming, engineSolver} {
		_, bundled := a.engineBinary(name)
		state := ""
		note := ""
		if entry := a.engines[name]; entry != nil {
			state, note = entry.state, entry.note
		}
		out = append(out, map[string]any{"name": string(name), "bundled": bundled, "state": state, "note": note})
	}
	return out
}

// randomToken is the per-process service token.
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(buf)*2)
	for i, b := range buf {
		out[i*2] = hexdigits[b>>4]
		out[i*2+1] = hexdigits[b&0x0f]
	}
	return string(out), nil
}

// streamingEngineOverride returns an external engine address only when
// MUSICOPS_ENGINE_URL names one. The settings page no longer asks for it: the
// installer ships go-music-dl, and a field that most people leave empty (or
// fill with an address that is not running) only produces confusing failures.
// The variable is the escape hatch for running the UI against a remote engine.
func streamingEngineOverride() string {
	raw := strings.TrimSpace(os.Getenv("MUSICOPS_ENGINE_URL"))
	if raw == "" {
		return ""
	}
	base, ok := allowedEngineURL(raw)
	if !ok {
		return ""
	}
	return base
}

// testStreamingEngine starts the bundled go-music-dl (or uses the configured
// external engine) and performs one search, so the settings page can prove the
// engine answers instead of asking the user to trust it.
func (a *App) testStreamingEngine(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query    string `json:"query"`
		Sources  string `json:"sources"`
		Download bool   `json:"download"`
	}
	_ = decode(r, &body)
	query := strings.TrimSpace(body.Query)
	if query == "" {
		query = "周杰伦"
	}
	service := strings.TrimSpace(body.Sources)
	if service == "" {
		service = "netease"
	}
	// The address field is retired (see streamingEngineOverride), but the
	// settings read stays: it fails when the profile itself is unavailable,
	// and that is still an error worth reporting instead of starting an engine.
	if _, err := a.loadDownloadSettings(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	base := streamingEngineOverride()
	token, external := "", base != ""
	if base == "" {
		external = false
		managedBase, managedToken, startErr := a.ensureEngine(engineStreaming)
		if startErr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "ENGINE_UNAVAILABLE", "error": startErr.Error()})
			return
		}
		base, token = managedBase, managedToken
	}
	song, class, err := a.streamSearch(r.Context(), base, service, SearchPlan{Query: query}, token)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "ENGINE_UNAVAILABLE", "error": err.Error(), "class": class})
		return
	}
	if song == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ready": true, "found": false, "external": external, "base_url": base, "note": "引擎可用，但这次查询没有结果"})
		return
	}
	out := map[string]any{
		"ok": true, "ready": true, "found": true, "external": external, "base_url": base,
		"title": song.Name, "artist": song.Artist, "album": song.Album, "source": song.Source,
		"note": "引擎可用",
	}
	if body.Download {
		audio, _, dlErr := a.streamDownload(r.Context(), base, song, token)
		if dlErr != nil {
			out["download"] = false
			out["download_error"] = dlErr.Error()
			writeJSON(w, http.StatusOK, out)
			return
		}
		format := "unknown"
		if len(audio) > 4 {
			format = string(audio[:4])
		}
		out["download"] = true
		out["bytes"] = len(audio)
		out["format"] = format
		if format != "fLaC" {
			if flac, convErr := a.transcodeEngineAudio(audio, song.Source, SearchPlan{}); convErr != nil {
				out["transcode"] = false
				out["transcode_error"] = convErr.Error()
			} else {
				out["transcode"] = true
				out["flac_bytes"] = len(flac)
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}
