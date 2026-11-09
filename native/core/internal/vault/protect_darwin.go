//go:build darwin

package vault

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const (
	macKeychainService = "com.musicops.desktop.vault"
	macKeychainAccount = "master-key-v1"
)

var (
	macKeyMu    sync.Mutex
	macKeyCache = map[string][]byte{}
)

// platformProtect encrypts/decrypts with the macOS Keychain master key.
func platformProtect(data []byte, decrypt bool) ([]byte, error) {
	key, err := macMasterKey(!decrypt)
	if err != nil {
		return nil, err
	}
	if decrypt {
		return open(macMagic, key, data)
	}
	return seal(macMagic, key, data)
}

// macMasterKey returns the 32-byte master key from the macOS Keychain,
// creating it when create is true. It shells out to /usr/bin/security, the
// same mechanism the Python vault uses, so both sides share one key.
func macMasterKey(create bool) ([]byte, error) {
	selected := os.Getenv("MUSICOPS_KEYCHAIN_PATH")
	if selected != "" {
		abs, err := filepath.Abs(selected)
		if err != nil {
			return nil, errors.New("the specified macOS keychain does not exist")
		}
		if info, err := os.Stat(abs); err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("the specified macOS keychain does not exist")
		}
		selected = abs
	}
	macKeyMu.Lock()
	defer macKeyMu.Unlock()
	if key, ok := macKeyCache[selected]; ok {
		return key, nil
	}
	query := []string{"find-generic-password", "-s", macKeychainService, "-a", macKeychainAccount, "-w"}
	out, code := runSecurity(query, selected)
	if code == 44 && create {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		encoded := base64.StdEncoding.EncodeToString(raw)
		_, _ = runSecurity([]string{"add-generic-password", "-s", macKeychainService, "-a", macKeychainAccount, "-w", encoded}, selected)
		out, code = runSecurity(query, selected)
	}
	if code != 0 {
		return nil, errors.New("macOS keychain master key is unavailable; unlock the keychain or re-enter the credentials")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil || len(key) != 32 {
		return nil, errors.New("macOS keychain master key has an invalid format")
	}
	macKeyCache[selected] = key
	return key, nil
}

// runSecurity runs /usr/bin/security and returns its stdout and exit code.
// The keychain path, when set, is appended as the final argument, matching
// the Python call.
func runSecurity(args []string, keychain string) (string, int) {
	full := args
	if keychain != "" {
		full = append(append([]string{}, args...), keychain)
	}
	cmd := exec.Command("/usr/bin/security", full...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		return "", -1
	}
	return string(out), 0
}
