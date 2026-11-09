//go:build !windows && !darwin

package vault

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var linuxKeyMu sync.Mutex

// platformProtect encrypts/decrypts with the Linux key-file master key.
func platformProtect(data []byte, decrypt bool) ([]byte, error) {
	key, err := linuxMasterKey(!decrypt)
	if err != nil {
		return nil, err
	}
	if decrypt {
		return open(linuxMagic, key, data)
	}
	return seal(linuxMagic, key, data)
}

// linuxMasterKey returns the 32-byte master key from the file named by
// MUSICOPS_VAULT_KEY_FILE, creating it when create is true. This is the
// Python clients.linux_vault contract: the key lives in a separate file from
// the profile, 0600, owned by the current user, never followed through a
// symlink.
func linuxMasterKey(create bool) ([]byte, error) {
	raw := os.Getenv("MUSICOPS_VAULT_KEY_FILE")
	if raw == "" {
		return nil, errors.New("Linux requires MUSICOPS_VAULT_KEY_FILE to be set to a persistent key file")
	}
	path := raw
	if !filepath.IsAbs(path) {
		return nil, errors.New("Linux credential key path must be absolute")
	}
	linuxKeyMu.Lock()
	defer linuxKeyMu.Unlock()
	if err := privateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(filepath.Dir(path), filepath.Base(path)+".lock")
	release, err := lockFile(lockPath)
	if err != nil {
		return nil, err
	}
	defer release()
	if _, err := os.Lstat(path); os.IsNotExist(err) && create {
		fd, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			fd.Close()
			return nil, err
		}
		if _, err := fd.Write(key); err != nil {
			fd.Close()
			return nil, err
		}
		if err := fd.Sync(); err != nil {
			fd.Close()
			return nil, err
		}
		if err := fd.Close(); err != nil {
			return nil, err
		}
	}
	fd, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New("Linux credential master key is missing; restore the original key, a new key cannot decrypt old credentials")
		}
		return nil, err
	}
	defer fd.Close()
	info, err := fd.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || int(info.Sys().(*syscall.Stat_t).Uid) != os.Getuid() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("Linux credential key must belong to the current user with 0600 permissions")
	}
	buf := make([]byte, 33)
	n, err := fd.Read(buf)
	if err != nil {
		return nil, err
	}
	if n != 32 {
		return nil, errors.New("Linux credential master key is corrupt")
	}
	return buf[:32], nil
}
