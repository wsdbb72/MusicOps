package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/desktop"
	"github.com/wsdbb72/MusicOps/native/core/internal/vault"
)

// resolveProfile picks the profile directory: an explicit --base wins, then
// a ./profile next to the working directory, then the desktop's remembered
// profile resolution for this executable.
func resolveProfile(baseFlag string) (string, error) {
	if baseFlag != "" {
		return filepath.Abs(baseFlag)
	}
	if info, err := os.Stat("./profile"); err == nil && info.IsDir() {
		return filepath.Abs("./profile")
	}
	profile, err := desktop.ResolveProfile(os.Args[0])
	if err != nil {
		return "", err
	}
	return filepath.Abs(profile)
}

// readCorePassword reads the core password from
// <profile>/state/core-password, decrypting the vault-protected blob. The
// password itself never goes to stdout or logs.
func readCorePassword(profile string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(profile, "state", "core-password"))
	if err != nil {
		return "", err
	}
	plain, err := vault.Unprotect(raw)
	if err != nil {
		return "", fmt.Errorf("核心口令无法解密: %w", err)
	}
	password := string(plain)
	if len(password) < 16 {
		return "", errors.New("核心口令无效")
	}
	return password, nil
}

// attachCore returns a client for the running core, starting it first when
// it is not running. A running core is attached to; a missing password file
// or a stopped core falls through to EnsureCoreDetached, which starts (or
// re-attaches to) the core and returns its URL and password. The start is
// detached: a short-lived CLI invocation must not kill the core it just
// started, so cross-invocation flows (status, then job, then control) work
// against the same core; stop with `musicops stop`.
func attachCore(profile, coreBinaryFlag string) (*coreClient, error) {
	if status, err := desktop.CoreStatus(profile); err == nil && status.Running && status.URL != "" {
		if password, err := readCorePassword(profile); err == nil {
			return &coreClient{base: status.URL, password: password}, nil
		}
	}
	binary, err := resolveCoreBinary(coreBinaryFlag)
	if err != nil {
		return nil, err
	}
	handle, err := desktop.EnsureCoreDetached(profile, binary)
	if err != nil {
		return nil, err
	}
	if handle.URL == "" || handle.Password == "" {
		return nil, errors.New("核心未返回访问信息")
	}
	return &coreClient{base: handle.URL, password: handle.Password}, nil
}

// resolveCoreBinary finds the core executable: the --core-binary flag,
// then MUSICOPS_CORE_BINARY, then musicops on PATH.
func resolveCoreBinary(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env := strings.TrimSpace(os.Getenv("MUSICOPS_CORE_BINARY")); env != "" {
		return env, nil
	}
	if path, err := exec.LookPath("musicops"); err == nil {
		return path, nil
	}
	return "", errors.New("找不到核心程序：请用 --core-binary 指定或把 musicops 加入 PATH")
}

// stopCore stops the core for a profile.
func stopCore(profile string) error {
	return desktop.StopCore(profile)
}
