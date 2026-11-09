package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Protect encrypts data with the platform master key, creating it when
// needed. The encoding is the same as the Python clients.vault.protect on
// the same platform, so a blob written here decrypts there and vice versa.
func Protect(data []byte) ([]byte, error) {
	return platformProtect(data, false)
}

// Unprotect decrypts data produced by Protect on this platform.
func Unprotect(data []byte) ([]byte, error) {
	return platformProtect(data, true)
}

var (
	macMagic   = []byte("MusicOpsKeychain1\x00")
	linuxMagic = []byte("MusicOpsLinux1\x00")
)

// seal encrypts with AES-GCM: magic || nonce(12) || ciphertext, AAD is the magic.
func seal(magic, key, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(magic)+len(nonce)+len(data)+gcm.Overhead())
	out = append(out, magic...)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, data, magic), nil
}

// open decrypts a seal envelope, rejecting anything that is not one.
func open(magic, key, data []byte) ([]byte, error) {
	if len(data) < len(magic)+28 || !equalPrefix(data, magic) {
		return nil, errors.New("credential file does not belong to this profile; re-enter the credentials")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := data[len(magic) : len(magic)+gcm.NonceSize()]
	plain, err := gcm.Open(nil, nonce, data[len(magic)+gcm.NonceSize():], magic)
	if err != nil {
		return nil, errors.New("credential decryption failed; the key does not match or the file is corrupt")
	}
	return plain, nil
}

func equalPrefix(data, prefix []byte) bool {
	if len(data) < len(prefix) {
		return false
	}
	for i := range prefix {
		if data[i] != prefix[i] {
			return false
		}
	}
	return true
}

// vaultFileName is the platform store file name, matching Python.
func vaultFileName() string {
	switch runtime.GOOS {
	case "windows":
		return "secrets.dpapi"
	case "darwin":
		return "secrets.keychain"
	default:
		return "secrets.aesgcm"
	}
}

// privateDir creates dir as a private directory (0700, not a symlink, owned
// by the current user on Unix), matching Python platforms.private_directory.
func privateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("private directory must not be a symlink")
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err = os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if err := checkDirOwnership(dir, info); err != nil {
		return err
	}
	return os.Chmod(dir, 0700)
}

// lockFile takes an exclusive non-blocking lock on path (creating it) and
// returns a release function. It retries for up to 5 seconds, matching the
// Python Vault lock.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0600); err != nil {
			f.Close()
			return nil, err
		}
	}
	if err := tryLock(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("credential store is being updated by another process, try again shortly: %w", err)
	}
	return func() {
		unlock(f)
		f.Close()
	}, nil
}

// userDataRoot mirrors Python platforms.user_data_root.
func userDataRoot() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support")
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return v
		}
		return filepath.Join(home, "AppData", "Local")
	default:
		if v := os.Getenv("XDG_DATA_HOME"); v != "" {
			return v
		}
		return filepath.Join(home, ".local", "share")
	}
}
