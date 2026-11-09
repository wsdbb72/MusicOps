package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	testVersion = "0.4.33"
	testTag     = "v0.4.33"
	testName    = "MusicOps-0.4.33-macOS-arm64.pkg"
	testURL     = "https://github.com/wsdbb72/MusicOps/releases/download/v0.4.33/MusicOps-0.4.33-macOS-arm64.pkg"
)

// fakeGitHub is one TLS server answering for every GitHub host: the test
// client's transport dials api.github.com:443 and github.com:443 to this
// listener, so the real HTTPS download, redirect and verification code
// runs without touching the real network.
type fakeGitHub struct {
	server        *httptest.Server
	releaseBody   []byte
	asset         []byte
	assetRedirect string
	assetGets     atomic.Int64
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/wsdbb72/MusicOps/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		// The product prefix is asserted, not the exact version: tests
		// drive Check with different Current values (e.g. "0.0.0-dev").
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "MusicOps/") {
			t.Errorf("Check User-Agent = %q", r.Header.Get("User-Agent"))
		}
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("Check Accept = %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(f.releaseBody)
	})
	mux.HandleFunc("/wsdbb72/MusicOps/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		f.assetGets.Add(1)
		if f.assetRedirect != "" {
			http.Redirect(w, r, f.assetRedirect, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(f.asset)
	})
	mux.HandleFunc("/redirect-target", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(f.asset)
	})
	f.server = httptest.NewTLSServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// mappedClient dials every host to the fake server, skipping TLS
// verification for the test certificate only. The updater still enforces
// its trusted-host, scheme and port rules on the URLs themselves.
func (f *fakeGitHub) mappedClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, f.server.Listener.Addr().String())
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only fake server
		},
	}
}

func (f *fakeGitHub) setRelease(t *testing.T, tag string, draft, prerelease bool, assets []map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"tag_name":   tag,
		"draft":      draft,
		"prerelease": prerelease,
		"assets":     assets,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.releaseBody = body
}

func assetEntry(name, url string, size int64, digest string) map[string]any {
	return map[string]any{
		"name": name, "browser_download_url": url, "size": size, "digest": digest,
	}
}

func testAsset(t *testing.T, size int) []byte {
	t.Helper()
	out := make([]byte, size)
	for i := range out {
		out[i] = byte(i % 251)
	}
	return out
}

func testUpdater(f *fakeGitHub, dir string) *Updater {
	return &Updater{
		HTTP: f.mappedClient(), Current: "0.4.32",
		System: "darwin", Machine: "arm64", Directory: dir,
	}
}

func TestInstallerName(t *testing.T) {
	cases := []struct {
		version, system, machine, want string
		wantErr                        bool
	}{
		{"0.4.33", "win32", "amd64", "MusicOps-0.4.33-Setup.exe", false},
		{"0.4.33", "win32", "x86_64", "MusicOps-0.4.33-Setup.exe", false},
		{"0.4.33", "darwin", "arm64", "MusicOps-0.4.33-macOS-arm64.pkg", false},
		{"0.4.33", "darwin", "aarch64", "MusicOps-0.4.33-macOS-arm64.pkg", false},
		{"0.4.33", "linux", "amd64", "", true},
		{"0.4.33", "win32", "arm64", "", true},
		{"0.4.33", "darwin", "amd64", "", true},
		{"1.2", "darwin", "arm64", "", true},
		{"0.4.33", "darwin", "arm64", "MusicOps-0.4.33-macOS-arm64.pkg", false},
	}
	for _, c := range cases {
		got, err := InstallerName(c.version, c.system, c.machine)
		if c.wantErr {
			if err == nil {
				t.Errorf("InstallerName(%q,%q,%q) = %q, want error", c.version, c.system, c.machine, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("InstallerName(%q,%q,%q) = %q, %v; want %q", c.version, c.system, c.machine, got, err, c.want)
		}
	}
}

func TestCheckNoUpdate(t *testing.T) {
	f := newFakeGitHub(t)
	asset := testAsset(t, 600_000)
	sum := sha256.Sum256(asset)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	updater := testUpdater(f, t.TempDir())
	for _, tag := range []string{"v0.4.32", "v0.4.31", "v1.2"} {
		f.setRelease(t, tag, false, false, []map[string]any{assetEntry(testName, testURL, int64(len(asset)), digest)})
		rel, err := updater.Check(context.Background())
		if err != nil {
			t.Fatalf("Check(%s): %v", tag, err)
		}
		if rel != nil {
			t.Fatalf("Check(%s) = %+v, want nil (no newer version)", tag, rel)
		}
	}
}

// TestCheckDevVersion: a dev build ("0.0.0-dev") must still be able to
// check for updates — its numeric triple orders below every release.
// A garbage current version still errors.
func TestCheckDevVersion(t *testing.T) {
	f := newFakeGitHub(t)
	asset := testAsset(t, 1024)
	sum := sha256.Sum256(asset)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	f.setRelease(t, testTag, false, false, []map[string]any{assetEntry(testName, testURL, int64(len(asset)), digest)})
	updater := testUpdater(f, t.TempDir())
	updater.Current = "0.0.0-dev"
	rel, err := updater.Check(context.Background())
	if err != nil {
		t.Fatalf("Check (0.0.0-dev): %v", err)
	}
	if rel == nil || rel.Version != testVersion {
		t.Fatalf("Check (0.0.0-dev) = %+v, want version %s", rel, testVersion)
	}
	updater.Current = "banana"
	if _, err := updater.Check(context.Background()); err == nil {
		t.Error("Check (banana): want error, got nil")
	}
}

func TestCheckDraftPrereleaseAndBareTag(t *testing.T) {
	f := newFakeGitHub(t)
	asset := testAsset(t, 1024)
	sum := sha256.Sum256(asset)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	updater := testUpdater(f, t.TempDir())
	cases := []struct {
		name       string
		tag        string
		draft      bool
		prerelease bool
	}{
		{"draft", testTag, true, false},
		{"prerelease", testTag, false, true},
		{"bare tag", "0.4.33", false, false},
	}
	for _, c := range cases {
		f.setRelease(t, c.tag, c.draft, c.prerelease, []map[string]any{assetEntry(testName, testURL, int64(len(asset)), digest)})
		if _, err := updater.Check(context.Background()); err == nil {
			t.Errorf("Check(%s): want error, got nil", c.name)
		}
	}
}

func TestCheckFullFlow(t *testing.T) {
	f := newFakeGitHub(t)
	asset := testAsset(t, 600_000)
	sum := sha256.Sum256(asset)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	f.asset = asset
	f.setRelease(t, testTag, false, false, []map[string]any{assetEntry(testName, testURL, int64(len(asset)), digest)})
	dir := t.TempDir()
	updater := testUpdater(f, dir)

	rel, err := updater.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rel == nil {
		t.Fatal("Check returned nil, want a release")
	}
	if rel.Version != testVersion || rel.Tag != testTag || rel.Name != testName ||
		rel.URL != testURL || rel.SHA256 != digest[7:] || rel.Size != int64(len(asset)) {
		t.Fatalf("Check = %+v, want version %s tag %s name %s size %d", rel, testVersion, testTag, testName, len(asset))
	}

	var percents []int
	staged, err := updater.Stage(context.Background(), rel, func(p int) { percents = append(percents, p) })
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if staged.Path != filepath.Join(dir, testVersion, testName) {
		t.Fatalf("Stage path = %q", staged.Path)
	}
	if len(percents) == 0 || percents[len(percents)-1] != 100 {
		t.Fatalf("progress = %v, want to end at 100", percents)
	}
	for i := 1; i < len(percents); i++ {
		if percents[i] <= percents[i-1] {
			t.Fatalf("progress not deduplicated/increasing: %v", percents)
		}
	}
	got, err := os.ReadFile(staged.Path)
	if err != nil || !bytes.Equal(got, asset) {
		t.Fatalf("staged bytes mismatch: %v", err)
	}
	if err := Verify(staged); err != nil {
		t.Fatalf("Verify staged: %v", err)
	}
	gets := f.assetGets.Load()
	info, err := os.Stat(staged.Path)
	if err != nil {
		t.Fatal(err)
	}
	// A second Stage reuses the verified copy without downloading again.
	staged2, err := updater.Stage(context.Background(), rel, nil)
	if err != nil {
		t.Fatalf("Stage reuse: %v", err)
	}
	if staged2.Path != staged.Path {
		t.Fatalf("Stage reuse path = %q, want %q", staged2.Path, staged.Path)
	}
	if f.assetGets.Load() != gets {
		t.Fatalf("Stage reuse downloaded again: %d -> %d asset GETs", gets, f.assetGets.Load())
	}
	info2, err := os.Stat(staged.Path)
	if err != nil || !info.ModTime().Equal(info2.ModTime()) {
		t.Fatalf("Stage reuse rewrote the file")
	}
}

func TestCheckAssetProblems(t *testing.T) {
	f := newFakeGitHub(t)
	asset := testAsset(t, 1024)
	sum := sha256.Sum256(asset)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	f.asset = asset
	updater := testUpdater(f, t.TempDir())
	cases := []struct {
		name   string
		assets []map[string]any
	}{
		{"no assets", nil},
		{"missing digest", []map[string]any{assetEntry(testName, testURL, int64(len(asset)), "")}},
		{"bad digest", []map[string]any{assetEntry(testName, testURL, int64(len(asset)), "sha256:zzzz")}},
		{"zero size", []map[string]any{assetEntry(testName, testURL, 0, digest)}},
		{"oversize", []map[string]any{assetEntry(testName, testURL, MaxInstaller+1, digest)}},
		{"wrong url", []map[string]any{assetEntry(testName, "https://github.com/wsdbb72/MusicOps/releases/download/v0.4.33/other.pkg", int64(len(asset)), digest)}},
		{"duplicate asset", []map[string]any{
			assetEntry(testName, testURL, int64(len(asset)), digest),
			assetEntry(testName, testURL, int64(len(asset)), digest),
		}},
	}
	for _, c := range cases {
		f.setRelease(t, testTag, false, false, c.assets)
		if rel, err := updater.Check(context.Background()); err == nil {
			t.Errorf("Check(%s): got release %+v, want error", c.name, rel)
		}
	}
	// A non-integer size must be rejected, not silently truncated.
	raw := `{"tag_name":"v0.4.33","draft":false,"prerelease":false,"assets":[{"name":"` + testName +
		`","browser_download_url":"` + testURL + `","size":1024.5,"digest":"` + digest + `"}]}`
	f.releaseBody = []byte(raw)
	if _, err := updater.Check(context.Background()); err == nil {
		t.Error("Check(float size): want error, got nil")
	}
	// An oversized release body must be rejected.
	f.releaseBody = []byte(strings.Repeat(" ", MaxReleaseInfo+1))
	if _, err := updater.Check(context.Background()); err == nil {
		t.Error("Check(oversized body): want error, got nil")
	}
}

func TestCheckOnLinuxHasNoPackage(t *testing.T) {
	f := newFakeGitHub(t)
	asset := testAsset(t, 1024)
	sum := sha256.Sum256(asset)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	f.asset = asset
	// No System/Machine override: this Linux box has no auto-update package.
	u := &Updater{HTTP: f.mappedClient(), Current: "0.4.32"}
	f.setRelease(t, testTag, false, false, []map[string]any{assetEntry(testName, testURL, int64(len(asset)), digest)})
	if _, err := u.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "当前系统没有自动更新安装包") {
		t.Fatalf("Check on linux = %v, want '当前系统没有自动更新安装包'", err)
	}
}

func TestStageRedirects(t *testing.T) {
	f := newFakeGitHub(t)
	asset := testAsset(t, 600_000)
	sum := sha256.Sum256(asset)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	f.asset = asset
	rel := &Release{Version: testVersion, Tag: testTag, Name: testName, URL: testURL, SHA256: digest[7:], Size: int64(len(asset))}

	// Redirect to a host outside the whitelist must be refused.
	f.assetRedirect = "https://evil.example.invalid/x.pkg"
	u := testUpdater(f, t.TempDir())
	if _, err := u.Stage(context.Background(), rel, nil); err == nil || !strings.Contains(err.Error(), "不可信") {
		t.Fatalf("Stage(redirect to evil host) = %v, want untrusted error", err)
	}
	// Redirect to plain HTTP must be refused even on a whitelisted host.
	f.assetRedirect = "http://github.com/wsdbb72/MusicOps/releases/download/v0.4.33/x.pkg"
	u = testUpdater(f, t.TempDir())
	if _, err := u.Stage(context.Background(), rel, nil); err == nil || !strings.Contains(err.Error(), "不可信") {
		t.Fatalf("Stage(redirect to http) = %v, want untrusted error", err)
	}
	// Redirect to a whitelisted host succeeds through the same server.
	f.assetRedirect = "https://objects.githubusercontent.com/redirect-target"
	u = testUpdater(f, t.TempDir())
	staged, err := u.Stage(context.Background(), rel, nil)
	if err != nil {
		t.Fatalf("Stage(redirect to trusted host): %v", err)
	}
	if err := Verify(staged); err != nil {
		t.Fatalf("Verify after trusted redirect: %v", err)
	}
}

func TestStageSizeLimits(t *testing.T) {
	f := newFakeGitHub(t)
	asset := testAsset(t, 1024)
	sum := sha256.Sum256(asset)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	rel := &Release{Version: testVersion, Tag: testTag, Name: testName, URL: testURL, SHA256: digest[7:], Size: int64(len(asset))}

	// Server sends more than the declared size.
	f.asset = testAsset(t, 2048)
	u := testUpdater(f, t.TempDir())
	if _, err := u.Stage(context.Background(), rel, nil); err == nil {
		t.Error("Stage(oversent body): want error, got nil")
	}
	// Server sends fewer bytes: the staged copy fails verification.
	f.asset = testAsset(t, 512)
	u = testUpdater(f, t.TempDir())
	if _, err := u.Stage(context.Background(), rel, nil); err == nil {
		t.Error("Stage(short body): want error, got nil")
	}
}

func TestVerifyRejectsTamperedAndSymlink(t *testing.T) {
	f := newFakeGitHub(t)
	asset := testAsset(t, 600_000)
	sum := sha256.Sum256(asset)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	f.asset = asset
	rel := &Release{Version: testVersion, Tag: testTag, Name: testName, URL: testURL, SHA256: digest[7:], Size: int64(len(asset))}
	dir := t.TempDir()
	u := testUpdater(f, dir)
	staged, err := u.Stage(context.Background(), rel, nil)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	// Tamper with one byte: verification must fail, and the next Stage
	// must re-download a good copy instead of reusing the tampered one.
	data, err := os.ReadFile(staged.Path)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0xff
	if err := os.WriteFile(staged.Path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(staged); err == nil {
		t.Fatal("Verify(tampered): want error, got nil")
	}
	gets := f.assetGets.Load()
	if _, err := u.Stage(context.Background(), rel, nil); err != nil {
		t.Fatalf("Stage after tamper: %v", err)
	}
	if f.assetGets.Load() == gets {
		t.Fatal("Stage after tamper did not re-download")
	}
	if err := Verify(staged); err != nil {
		t.Fatalf("Verify after re-download: %v", err)
	}
	// Replace the staged file with a symlink to a good copy: rejected.
	good := filepath.Join(dir, "good.pkg")
	if err := os.WriteFile(good, asset, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(staged.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(good, staged.Path); err != nil {
		t.Fatal(err)
	}
	if err := Verify(staged); err == nil {
		t.Fatal("Verify(symlink): want error, got nil")
	}
	// A staged file whose size changed is incomplete.
	if err := os.Remove(staged.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged.Path, asset[:100], 0600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(staged); err == nil {
		t.Fatal("Verify(short file): want error, got nil")
	}
}

func TestLaunchDarwin(t *testing.T) {
	f := newFakeGitHub(t)
	asset := testAsset(t, 1024)
	sum := sha256.Sum256(asset)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	f.asset = asset
	rel := &Release{Version: testVersion, Tag: testTag, Name: testName, URL: testURL, SHA256: digest[7:], Size: int64(len(asset))}
	dir := t.TempDir()
	u := testUpdater(f, dir)
	staged, err := u.Stage(context.Background(), rel, nil)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	old := launchExec
	defer func() { launchExec = old }()
	var gotName string
	var gotArgs []string
	var gotDir string
	launchExec = func(_ context.Context, name string, args []string, d string) error {
		gotName, gotArgs, gotDir = name, args, d
		return nil
	}
	if err := u.Launch(staged, 1234, ""); err != nil {
		t.Fatalf("Launch darwin: %v", err)
	}
	if gotName != "/usr/bin/open" || len(gotArgs) != 1 || gotArgs[0] != staged.Path || gotDir != "" {
		t.Fatalf("Launch darwin invoked (%q, %q, %q), want open of %q", gotName, gotArgs, gotDir, staged.Path)
	}
	// Launch verifies first: a tampered installer never reaches the launcher.
	data, err := os.ReadFile(staged.Path)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0xff
	if err := os.WriteFile(staged.Path, data, 0600); err != nil {
		t.Fatal(err)
	}
	launchExec = func(_ context.Context, name string, args []string, d string) error {
		t.Error("launcher ran for a tampered installer")
		return nil
	}
	if err := u.Launch(staged, 1234, ""); err == nil {
		t.Fatal("Launch(tampered): want error, got nil")
	}
}

func TestLaunchWindows(t *testing.T) {
	f := newFakeGitHub(t)
	asset := testAsset(t, 1024)
	sum := sha256.Sum256(asset)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	f.asset = asset
	// The Windows installer name differs from the darwin one.
	winName := "MusicOps-0.4.33-Setup.exe"
	winRel := &Release{Version: testVersion, Tag: testTag, Name: winName,
		URL:    "https://github.com/wsdbb72/MusicOps/releases/download/v0.4.33/" + winName,
		SHA256: digest[7:], Size: int64(len(asset))}
	dir := t.TempDir()
	u := &Updater{HTTP: f.mappedClient(), Current: "0.4.32", System: "win32", Machine: "amd64", Directory: dir}
	staged, err := u.Stage(context.Background(), winRel, nil)
	if err != nil {
		t.Fatalf("Stage windows: %v", err)
	}
	old := launchExec
	defer func() { launchExec = old }()
	var gotName string
	var gotArgs []string
	var gotDir string
	launchExec = func(_ context.Context, name string, args []string, d string) error {
		gotName, gotArgs, gotDir = name, args, d
		return nil
	}
	installDir := filepath.Join(dir, "install")
	if err := u.Launch(staged, 4242, installDir); err != nil {
		t.Fatalf("Launch windows: %v", err)
	}
	if !strings.HasSuffix(gotName, "powershell.exe") {
		t.Fatalf("Launch windows invoked %q, want powershell.exe", gotName)
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-ExecutionPolicy", "Bypass", "-File", "-Payload"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Launch windows args %q missing %q", joined, want)
		}
	}
	if gotDir != filepath.Dir(staged.Path) {
		t.Fatalf("Launch windows cwd = %q, want %q", gotDir, filepath.Dir(staged.Path))
	}
	// The helper script and payload must exist next to the installer.
	helper := filepath.Join(filepath.Dir(staged.Path), "apply-update.ps1")
	script, err := os.ReadFile(helper)
	if err != nil {
		t.Fatalf("read helper: %v", err)
	}
	for _, want := range []string{"MusicOps did not exit", "MusicOps-worker.exe", "Update checksum mismatch", `/DIR="`, "handoff-error.txt"} {
		if !strings.Contains(string(script), want) {
			t.Fatalf("helper script missing %q", want)
		}
	}
	var payloadPath string
	for i, a := range gotArgs {
		if a == "-Payload" && i+1 < len(gotArgs) {
			payloadPath = gotArgs[i+1]
		}
	}
	if payloadPath == "" {
		t.Fatal("no -Payload argument")
	}
	raw, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	var payload struct {
		PID       int    `json:"pid"`
		Installer string `json:"installer"`
		SHA256    string `json:"sha256"`
		Size      int64  `json:"size"`
		Directory string `json:"directory"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("payload JSON: %v", err)
	}
	if payload.PID != 4242 || payload.Installer != staged.Path || payload.SHA256 != digest[7:] ||
		payload.Size != int64(len(asset)) || payload.Directory != installDir {
		t.Fatalf("payload = %+v, want pid 4242 installer %q directory %q", payload, staged.Path, installDir)
	}
}

func TestLaunchLinuxRefuses(t *testing.T) {
	f := newFakeGitHub(t)
	asset := testAsset(t, 1024)
	sum := sha256.Sum256(asset)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	staged := &Staged{Version: testVersion, Path: filepath.Join(t.TempDir(), "x.pkg"), SHA256: digest[7:], Size: int64(len(asset))}
	if err := os.WriteFile(staged.Path, asset, 0600); err != nil {
		t.Fatal(err)
	}
	u := &Updater{HTTP: f.mappedClient(), Current: "0.4.32"}
	if err := u.Launch(staged, 0, ""); err == nil || !strings.Contains(err.Error(), "当前系统没有自动更新安装器") {
		t.Fatalf("Launch on linux = %v, want '当前系统没有自动更新安装器'", err)
	}
}
