// Package updater ports src/musicops/updates.py: official-release checks,
// bounded downloads, SHA-256 verification and the installer handoff. It
// only ever talks to the trusted GitHub hosts, only over HTTPS, and it never
// follows a redirect off that list. Linux has no auto-update installer:
// Docker images update instead, so Launch refuses there.
package updater

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultReleasesURL is the GitHub API endpoint for the latest release.
	DefaultReleasesURL = "https://api.github.com/repos/wsdbb72/MusicOps/releases/latest"
	// MaxInstaller is the largest installer the updater will accept, matching
	// Python's MAX_INSTALLER.
	MaxInstaller = 512 * 1024 * 1024
	// MaxReleaseInfo is the largest release metadata body accepted.
	MaxReleaseInfo = 2 * 1024 * 1024
	// DownloadTimeout bounds one staged download, matching Python's 600s.
	DownloadTimeout = 600 * time.Second
	// CheckTimeout bounds the release metadata request, matching Python's
	// request timeout of 20s.
	CheckTimeout = 20 * time.Second
)

var defaultTrustedHosts = []string{
	"api.github.com",
	"github.com",
	"release-assets.githubusercontent.com",
	"objects.githubusercontent.com",
}

var (
	versionPattern = regexp.MustCompile(`^\d{1,6}\.\d{1,6}\.\d{1,6}$`)
	digestPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	hex64Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Updater checks GitHub Releases and stages installers. A nil HTTP client
// means http.DefaultClient; TrustedHosts defaults to the GitHub hosts.
type Updater struct {
	HTTP         *http.Client
	ReleasesURL  string
	Directory    string // staging directory; empty means <UserDataRoot>/MusicOps/updates
	Current      string // current version, e.g. "0.4.32"
	System       string // "win32" | "darwin"; empty resolves from runtime
	Machine      string // "amd64" | "arm64"; empty resolves from runtime
	TrustedHosts []string
}

// Release is a verified newer release with its installer asset.
type Release struct {
	Version string
	Tag     string
	Name    string
	URL     string
	SHA256  string
	Size    int64
}

// Staged is a downloaded, verified installer on disk.
type Staged struct {
	Version string
	Path    string
	SHA256  string
	Size    int64
}

func (u *Updater) system() string {
	if u.System != "" {
		return u.System
	}
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	default:
		return runtime.GOOS
	}
}

func (u *Updater) machine() string {
	if u.Machine != "" {
		return strings.ToLower(u.Machine)
	}
	return runtime.GOARCH
}

func (u *Updater) trustedHosts() []string {
	if len(u.TrustedHosts) > 0 {
		return u.TrustedHosts
	}
	return defaultTrustedHosts
}

func (u *Updater) releasesURL() string {
	if u.ReleasesURL != "" {
		return u.ReleasesURL
	}
	return DefaultReleasesURL
}

func (u *Updater) directory() (string, error) {
	if u.Directory != "" {
		return u.Directory, nil
	}
	root, err := defaultUserDataRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "MusicOps", "updates"), nil
}

// defaultUserDataRoot mirrors musicops.platforms.user_data_root: macOS
// ~/Library/Application Support, Windows %LOCALAPPDATA%, elsewhere
// $XDG_DATA_HOME or ~/.local/share. internal/desktop.UserDataRoot is the
// canonical source once that package lands; this keeps the same rules so
// the default staging directory agrees with it.
func defaultUserDataRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support"), nil
	case "windows":
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return local, nil
		}
		return filepath.Join(home, "AppData", "Local"), nil
	default:
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return xdg, nil
		}
		return filepath.Join(home, ".local", "share"), nil
	}
}

// InstallerName returns the installer file name for a version on a
// platform, matching Python's installer_name. Only win32/amd64 and
// darwin/arm64 have auto-update installers.
func InstallerName(version, system, machine string) (string, error) {
	if !versionPattern.MatchString(version) {
		return "", errors.New("更新版本格式无效")
	}
	machine = strings.ToLower(machine)
	switch {
	case system == "win32" && (machine == "amd64" || machine == "x86_64"):
		return "MusicOps-" + version + "-Setup.exe", nil
	case system == "darwin" && (machine == "arm64" || machine == "aarch64"):
		return "MusicOps-" + version + "-macOS-arm64.pkg", nil
	default:
		return "", errors.New("当前系统没有自动更新安装包")
	}
}

// parseVersion parses a strict x.y.z version into its numeric parts.
func parseVersion(value string) ([3]int, error) {
	var out [3]int
	if !versionPattern.MatchString(value) {
		return out, errors.New("更新版本格式无效")
	}
	parts := strings.Split(value, ".")
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, errors.New("更新版本格式无效")
		}
		out[i] = n
	}
	return out, nil
}

// parseCurrentVersion parses the running version for comparison. Release
// versions stay strict (parseVersion): a release tag is never a
// prerelease. But a dev build reports "0.0.0-dev", whose numeric triple
// still orders below every release, so the prerelease suffix is stripped
// here and only here — a dev build must still be able to check for
// updates instead of failing with "当前版本格式无效".
func parseCurrentVersion(value string) ([3]int, error) {
	if i := strings.Index(value, "-"); i >= 0 {
		value = value[:i]
	}
	return parseVersion(value)
}

// compareVersion returns -1, 0 or 1 for a against b.
func compareVersion(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// trustedURL rejects anything that is not plain HTTPS on a whitelisted host
// with no embedded credentials and no port other than 443 (or none).
func (u *Updater) trustedURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("更新下载地址不可信: %w", err)
	}
	if parsed.Scheme != "https" {
		return errors.New("更新下载地址不可信")
	}
	if parsed.User != nil {
		return errors.New("更新下载地址不可信")
	}
	host := parsed.Hostname()
	trusted := false
	for _, h := range u.trustedHosts() {
		if h == host {
			trusted = true
			break
		}
	}
	if !trusted {
		return errors.New("更新下载地址不可信")
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return errors.New("更新下载地址不可信")
	}
	return nil
}

// client returns an HTTP client that validates every redirect target with
// the trusted-host rules, mirroring Python's SafeRedirect handler.
func (u *Updater) client() *http.Client {
	var base *http.Client
	if u.HTTP != nil {
		cp := *u.HTTP
		base = &cp
	} else {
		base = &http.Client{}
	}
	check := func(req *http.Request, via []*http.Request) error {
		if err := u.trustedURL(req.URL.String()); err != nil {
			return err
		}
		if len(via) >= 10 {
			return errors.New("更新重定向过多")
		}
		return nil
	}
	base.CheckRedirect = check
	return base
}

// request builds a trusted GET with the MusicOps user agent.
func (u *Updater) request(ctx context.Context, raw, accept string) (*http.Response, error) {
	if err := u.trustedURL(raw); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "MusicOps/"+u.Current)
	req.Header.Set("Accept", accept)
	return u.client().Do(req)
}

// Check asks GitHub for the latest formal release. It returns nil, nil
// when there is no newer version: a malformed version or one no newer than
// Current is not an update. A draft, a prerelease, a tag without the v
// prefix, or a release whose installer asset fails any check is an error,
// matching Python's stage_latest, which refuses to continue on those.
func (u *Updater) Check(ctx context.Context) (*Release, error) {
	ctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	resp, err := u.request(ctx, u.releasesURL(), "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("检查更新失败: HTTP %d", resp.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, MaxReleaseInfo+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > MaxReleaseInfo {
		return nil, errors.New("更新信息超出大小限制")
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var release struct {
		TagName    string `json:"tag_name"`
		Draft      *bool  `json:"draft"`
		Prerelease *bool  `json:"prerelease"`
		Assets     []struct {
			Name               string      `json:"name"`
			BrowserDownloadURL string      `json:"browser_download_url"`
			Size               json.Number `json:"size"`
			Digest             string      `json:"digest"`
		} `json:"assets"`
	}
	if err := dec.Decode(&release); err != nil {
		return nil, fmt.Errorf("更新信息解析失败: %w", err)
	}
	tag := release.TagName
	if !strings.HasPrefix(tag, "v") || release.Draft == nil || *release.Draft ||
		release.Prerelease == nil || *release.Prerelease {
		return nil, errors.New("更新信息不是正式版本")
	}
	version := strings.TrimPrefix(tag, "v")
	current, err := parseCurrentVersion(u.Current)
	if err != nil {
		return nil, fmt.Errorf("当前版本格式无效: %w", err)
	}
	latest, err := parseVersion(version)
	if err != nil || compareVersion(latest, current) <= 0 {
		return nil, nil
	}
	name, err := InstallerName(version, u.system(), u.machine())
	if err != nil {
		return nil, err
	}
	expectedURL := "https://github.com/wsdbb72/MusicOps/releases/download/" + tag + "/" + name
	var asset *struct {
		Name               string      `json:"name"`
		BrowserDownloadURL string      `json:"browser_download_url"`
		Size               json.Number `json:"size"`
		Digest             string      `json:"digest"`
	}
	matches := 0
	for i := range release.Assets {
		if release.Assets[i].Name == name {
			matches++
			asset = &release.Assets[i]
		}
	}
	if matches != 1 {
		return nil, errors.New("新版尚未提供当前系统的安装包")
	}
	if asset.BrowserDownloadURL != expectedURL {
		return nil, errors.New("新版安装包缺少有效的来源或校验信息")
	}
	size, err := asset.Size.Int64()
	if err != nil || size <= 0 || size > MaxInstaller {
		return nil, errors.New("新版安装包缺少有效的来源或校验信息")
	}
	if !digestPattern.MatchString(asset.Digest) {
		return nil, errors.New("新版安装包缺少有效的来源或校验信息")
	}
	return &Release{
		Version: version,
		Tag:     tag,
		Name:    name,
		URL:     expectedURL,
		SHA256:  asset.Digest[7:],
		Size:    size,
	}, nil
}

// rejectReparseChain refuses a path whose own entry or any ancestor up to
// and including root is a symlink or reparse point. Ancestors above root
// are outside the updater's tree and are not policed: on macOS the user
// data root can legitimately sit under a symlinked parent such as
// /var -> /private/var. Missing components are skipped: they cannot be a
// symlink yet and are created by the caller.
func rejectReparseChain(path, root string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	for cur := abs; ; cur = filepath.Dir(cur) {
		if info, err := os.Lstat(cur); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("路径含junction/symlink/reparse point，须人工确认范围: %s", cur)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if cur == rootAbs || cur == filepath.Dir(cur) {
			return nil
		}
	}
}

// ensurePrivateDir creates dir with 0700 permissions, refusing to traverse
// a symlinked component inside the updater's own tree.
func ensurePrivateDir(dir, root string) error {
	if err := rejectReparseChain(dir, root); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(dir, 0700)
	}
	return rejectReparseChain(dir, root)
}

// Stage downloads rel's installer (or reuses a staged copy that already
// verifies), reporting download progress as deduplicated percentages.
// The download is bounded by size and by 600 seconds, fsynced, verified,
// and only then atomically renamed into place.
func (u *Updater) Stage(ctx context.Context, rel *Release, progress func(percent int)) (*Staged, error) {
	if rel == nil {
		return nil, errors.New("缺少更新信息")
	}
	if !versionPattern.MatchString(rel.Version) {
		return nil, errors.New("更新版本格式无效")
	}
	if rel.Size <= 0 || rel.Size > MaxInstaller {
		return nil, errors.New("新版安装包缺少有效的来源或校验信息")
	}
	if !hex64Pattern.MatchString(rel.SHA256) {
		return nil, errors.New("新版安装包缺少有效的来源或校验信息")
	}
	name, err := InstallerName(rel.Version, u.system(), u.machine())
	if err != nil {
		return nil, err
	}
	if rel.Name != name {
		return nil, errors.New("新版安装包名称与当前系统不符")
	}
	dir, err := u.directory()
	if err != nil {
		return nil, err
	}
	versionDir := filepath.Join(dir, rel.Version)
	if err := ensurePrivateDir(versionDir, dir); err != nil {
		return nil, err
	}
	final := filepath.Join(versionDir, name)
	staged := &Staged{Version: rel.Version, Path: final, SHA256: rel.SHA256, Size: rel.Size}
	if _, err := os.Stat(final); err == nil {
		if err := Verify(staged); err == nil {
			return staged, nil
		}
	}
	// A fresh partial name per attempt; crypto randomness, not a timestamp.
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	partial := filepath.Join(versionDir, name+"."+hex.EncodeToString(nonce)+".part")
	out, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	downloadErr := func() error {
		defer os.Remove(partial)
		defer out.Close()
		dctx, cancel := context.WithTimeout(ctx, DownloadTimeout)
		defer cancel()
		resp, err := u.request(dctx, rel.URL, "application/octet-stream")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("下载更新失败: HTTP %d", resp.StatusCode)
		}
		received := int64(0)
		lastPercent := -1
		report := func(p int) {
			if progress != nil && p != lastPercent {
				lastPercent = p
				progress(p)
			}
		}
		buf := make([]byte, 256*1024)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				received += int64(n)
				if received > rel.Size {
					return errors.New("更新下载大小或时间超出限制")
				}
				if _, err := out.Write(buf[:n]); err != nil {
					return err
				}
				report(int(received * 100 / rel.Size))
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					break
				}
				return readErr
			}
		}
		if err := out.Sync(); err != nil {
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		pending := &Staged{Version: rel.Version, Path: partial, SHA256: rel.SHA256, Size: rel.Size}
		if err := Verify(pending); err != nil {
			return err
		}
		// The destination itself must not have become a symlink while the
		// download ran; renaming over one would hand the installer to it.
		if err := rejectReparseChain(final, dir); err != nil {
			return err
		}
		if err := os.Rename(partial, final); err != nil {
			return err
		}
		return nil
	}()
	if downloadErr != nil {
		return nil, downloadErr
	}
	return staged, nil
}

// Verify rejects a staged installer whose path is a symlink, that is
// missing, whose size differs, or whose SHA-256 differs.
func Verify(s *Staged) error {
	if s == nil || s.Path == "" {
		return errors.New("更新安装包不完整，请重新检查更新")
	}
	if !hex64Pattern.MatchString(s.SHA256) {
		return errors.New("更新安装包校验失败，请重新下载")
	}
	// The updater owns <updates>/<version>/<name>; police that tree only.
	root := filepath.Dir(filepath.Dir(s.Path))
	if err := rejectReparseChain(s.Path, root); err != nil {
		return err
	}
	info, err := os.Stat(s.Path)
	if err != nil {
		return errors.New("更新安装包不完整，请重新检查更新")
	}
	if info.Size() != s.Size {
		return errors.New("更新安装包不完整，请重新检查更新")
	}
	file, err := os.Open(s.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return err
	}
	if hex.EncodeToString(sum.Sum(nil)) != s.SHA256 {
		return errors.New("更新安装包校验失败，请重新下载")
	}
	return nil
}
