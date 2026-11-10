//go:build darwin

package vault

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestKeychainDecryptsInAFreshProcess is the Go replacement for the
// Python macOS keychain regression. It refuses to run unless
// MUSICOPS_KEYCHAIN_PATH names an explicit temporary keychain, so a
// developer machine's login keychain is never created, read or deleted.
// Encryption in this process must decrypt in a second process, which
// cannot see the in-memory key cache.
func TestKeychainDecryptsInAFreshProcess(t *testing.T) {
	selected := os.Getenv("MUSICOPS_KEYCHAIN_PATH")
	if selected == "" {
		t.Skip("MUSICOPS_KEYCHAIN_PATH is required; this test never touches the login keychain")
	}
	if os.Getenv("MUSICOPS_KEYCHAIN_CHILD") == "1" {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			t.Fatal(err)
		}
		blob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		plain, err := Unprotect(blob)
		if err != nil {
			t.Fatal(err)
		}
		if string(plain) != "musicops-fixture-only-keychain-check" {
			t.Fatalf("fresh process decrypted %q", plain)
		}
		fmt.Println("verified")
		return
	}
	info, err := os.Stat(selected)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("temporary keychain %s is not a file", selected)
	}
	const secret = "musicops-fixture-only-keychain-check"
	blob, err := Protect([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte(secret)) {
		t.Fatal("ciphertext contains the plaintext")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestKeychainDecryptsInAFreshProcess$")
	cmd.Env = append(os.Environ(), "MUSICOPS_KEYCHAIN_CHILD=1")
	cmd.Stdin = strings.NewReader(base64.StdEncoding.EncodeToString(blob))
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("verified")) {
		t.Fatalf("fresh process: %v\n%s", err, out)
	}

	profile := t.TempDir()
	store, err := Open(profile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Set("vault://fixture/mac", "fixture value", "official.invalid"); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(store.Path()) != "secrets.keychain" {
		t.Fatalf("vault file %s", store.Path())
	}
	body, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("fixture value")) {
		t.Fatal("vault file contains plaintext")
	}
	if info, err := os.Stat(store.Path()); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("vault mode %v err %v", info, err)
	}
	got, found, err := store.Get("vault://fixture/mac", "official.invalid")
	if err != nil || !found || got != "fixture value" {
		t.Fatalf("Get: %q found %v err %v", got, found, err)
	}
	if _, _, err := store.Get("vault://fixture/mac", "third-party.invalid"); err != ErrHostMismatch {
		t.Fatalf("other host: %v", err)
	}
}
