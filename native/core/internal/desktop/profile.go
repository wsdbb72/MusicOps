// Package desktop is the Windows/macOS desktop shell for MusicOps: profile
// selection, core supervision, autostart, the reverse proxy in front of the
// Go core, and the thin Wails window wiring (windows/darwin only).
//
// It ports src/musicops/startup.py, src/musicops/platforms.py,
// src/musicops/worker/supervision.py, src/musicops/worker/lifecycle.py and
// the ensure_running half of src/musicops/worker/client.py. The desktop
// window renders the same WebUI the Go core serves to Docker, through a
// reverse proxy that injects the core's Basic credentials.
package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// activeProfileFile is the remembered-profile pointer under the user data
	// root. Mirrors startup.py's active-profile.json (4096 byte cap).
	activeProfileFile     = "active-profile.json"
	maxActiveProfileBytes = 4096
)

// UserDataRoot is the per-user application data root, mirroring
// platforms.user_data_root: macOS ~/Library/Application Support, Windows
// %LOCALAPPDATA% (falling back to ~/AppData/Local), Linux $XDG_DATA_HOME or
// ~/.local/share.
func UserDataRoot() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support")
	case "windows":
		if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
			return dir
		}
		return filepath.Join(home, "AppData", "Local")
	default:
		if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
			return dir
		}
		return filepath.Join(home, ".local", "share")
	}
}

// ResolveProfile picks the profile directory for the desktop executable,
// mirroring startup.default_profile:
//
// (a) portable mode: a profile directory next to the executable (not macOS);
// (b) a source checkout: walking up at most 3 levels from the executable
// directory, a directory holding pyproject.toml whose
// profile/state/musicops.sqlite exists (not macOS);
// (c) the remembered profile in <user-data>/MusicOps/active-profile.json
// (at most 4096 bytes, JSON {"profile": ...}, absolute, on macOS no path
// segment may end in .app, and profile/state/musicops.sqlite must exist);
// (d) the default <user-data>/MusicOps/profile.
//
// The returned path is resolved (symlinks expanded). A candidate profile
// directory that is itself a symbolic link is refused, mirroring
// platforms.private_directory: private runtime directories must not be
// symlinks.
func ResolveProfile(executable string) (string, error) {
	if executable == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("cannot locate the desktop executable: %w", err)
		}
		executable = exe
	}
	exeAbs, err := filepath.Abs(executable)
	if err != nil {
		return "", err
	}
	root := filepath.Dir(exeAbs)

	if runtime.GOOS != "darwin" {
		if dir := filepath.Join(root, "profile"); isDir(dir) {
			return finalizeProfile(dir)
		}
		// root plus up to three parents, mirroring
		// (root, *list(root.parents)[:3]).
		dir := root
		for i := 0; i < 4; i++ {
			if isFile(filepath.Join(dir, "pyproject.toml")) &&
				isFile(filepath.Join(dir, "profile", "state", "musicops.sqlite")) {
				return finalizeProfile(filepath.Join(dir, "profile"))
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}

	if p := rememberedProfile(); p != "" {
		return finalizeProfile(p)
	}
	return finalizeProfile(filepath.Join(UserDataRoot(), "MusicOps", "profile"))
}

// finalizeProfile refuses a profile directory that is itself a symlink and
// returns the symlink-resolved path. Nonexistent paths resolve through their
// nearest existing ancestor (filepath.EvalSymlinks fails on missing paths,
// while Python's Path.resolve() does not).
func finalizeProfile(candidate string) (string, error) {
	if info, err := os.Lstat(candidate); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("profile directory %q must not be a symbolic link", candidate)
	}
	resolved, err := resolvePath(candidate)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// resolvePath expands symlinks in path, tolerating a nonexistent tail.
func resolvePath(path string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, nil
	}
	// Walk up to the nearest existing ancestor, resolve it, re-append the
	// remainder verbatim.
	rest := []string{}
	cur := path
	for {
		if _, err := os.Lstat(cur); err == nil {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			for i := len(rest) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, rest[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Clean(path), nil
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// rememberedProfile reads the active-profile.json pointer. It returns ""
// when there is no usable remembered profile, mirroring startup.py's
// swallow-and-fall-through behaviour on any read/parse/validation failure.
func rememberedProfile() string {
	location := filepath.Join(UserDataRoot(), "MusicOps", activeProfileFile)
	info, err := os.Stat(location)
	if err != nil || info.Size() > maxActiveProfileBytes {
		return ""
	}
	raw, err := os.ReadFile(location)
	if err != nil {
		return ""
	}
	var value struct {
		Profile string `json:"profile"`
	}
	if err := json.Unmarshal(raw, &value); err != nil || value.Profile == "" {
		return ""
	}
	profile := value.Profile
	if !filepath.IsAbs(profile) {
		return ""
	}
	if runtime.GOOS == "darwin" {
		for _, part := range strings.Split(profile, string(filepath.Separator)) {
			if strings.HasSuffix(part, ".app") {
				return ""
			}
		}
	}
	if !isFile(filepath.Join(profile, "state", "musicops.sqlite")) {
		return ""
	}
	return profile
}

// RememberProfile atomically writes <user-data>/MusicOps/active-profile.json
// pointing at base (temp file + fsync + rename, directory 0700), mirroring
// startup.remember_profile. The stored path is the resolved one.
func RememberProfile(base string) error {
	resolved, err := resolvePath(base)
	if err != nil {
		return err
	}
	location := filepath.Join(UserDataRoot(), "MusicOps")
	if err := privateDirectory(location); err != nil {
		return err
	}
	encoded, err := json.Marshal(map[string]string{"profile": resolved})
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(location, activeProfileFile), encoded, 0600)
}

// LinuxDefaultKeyFile implements the Linux vault key default: when
// MUSICOPS_VAULT_KEY_FILE is unset, it defaults to
// <user-data>/MusicOps/vault.key and records that in the process
// environment so internal/vault can use it. An explicitly set value is
// never overwritten. On non-Linux platforms it is a no-op returning "".
// The return value is the effective key file path ("" off Linux).
func LinuxDefaultKeyFile() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	if existing := os.Getenv("MUSICOPS_VAULT_KEY_FILE"); existing != "" {
		return existing
	}
	path := filepath.Join(UserDataRoot(), "MusicOps", "vault.key")
	_ = os.Setenv("MUSICOPS_VAULT_KEY_FILE", path)
	return path
}

// privateDirectory creates dir with mode 0700, refusing a symlink, mirroring
// platforms.private_directory (including the same-owner check on Unix).
func privateDirectory(path string) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("private runtime directory must not be a symbolic link")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0700); err != nil {
			return err
		}
		if info, err := os.Stat(path); err == nil {
			if st, ok := statUID(info); ok && st != os.Getuid() {
				return errors.New("private runtime directory does not belong to the current user")
			}
		}
	}
	return nil
}

// atomicWriteFile writes data to path via a temp file in the same directory
// (fsync + rename), with the given permissions on the final file.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// Best-effort directory fsync so the rename itself is durable.
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
