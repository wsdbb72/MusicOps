package vault

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testKeyFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "master.key")
	t.Setenv("MUSICOPS_VAULT_KEY_FILE", key)
	return key
}

func TestProtectRoundTrip(t *testing.T) {
	testKeyFile(t)
	plain := []byte(`{"vault://pt/dicmusic/cookies":"secret-value"}`)
	blob, err := Protect(plain)
	if err != nil {
		t.Fatalf("Protect: %v", err)
	}
	if bytes.Equal(blob, plain) {
		t.Fatal("Protect returned plaintext")
	}
	// The Linux envelope is magic || nonce(12) || ciphertext.
	if !bytes.HasPrefix(blob, linuxMagic) {
		t.Fatalf("blob lacks the Linux magic, got %q", blob[:16])
	}
	back, err := Unprotect(blob)
	if err != nil {
		t.Fatalf("Unprotect: %v", err)
	}
	if !bytes.Equal(back, plain) {
		t.Fatalf("round-trip mismatch: got %q", back)
	}
	// Tampered ciphertext must fail closed.
	tampered := append([]byte{}, blob...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := Unprotect(tampered); err == nil {
		t.Fatal("tampered blob decrypted without error")
	}
	// Wrong magic must fail closed.
	if _, err := Unprotect([]byte("not-a-vault-blob-at-all-0123456789")); err == nil {
		t.Fatal("non-envelope blob decrypted without error")
	}
	// Empty plaintext round-trips.
	empty, err := Protect([]byte{})
	if err != nil {
		t.Fatalf("Protect empty: %v", err)
	}
	if got, err := Unprotect(empty); err != nil || len(got) != 0 {
		t.Fatalf("empty round-trip: got %q err %v", got, err)
	}
}

func TestProtectRequiresKeyFile(t *testing.T) {
	t.Setenv("MUSICOPS_VAULT_KEY_FILE", "")
	if _, err := Protect([]byte("x")); err == nil {
		t.Fatal("Protect without a key file must fail")
	}
}

func TestVaultSetGet(t *testing.T) {
	testKeyFile(t)
	v, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rev, err := v.Set("vault://pt/dicmusic/cookies", "cookie-value", "dicmusic.com")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if rev != 1 {
		t.Fatalf("first revision = %d, want 1", rev)
	}
	got, found, err := v.Get("vault://pt/dicmusic/cookies", "dicmusic.com")
	if err != nil || !found || got != "cookie-value" {
		t.Fatalf("Get: got %q found %v err %v", got, found, err)
	}
	// Host binding is enforced and case-insensitive.
	if _, _, err := v.Get("vault://pt/dicmusic/cookies", "other.example"); err != ErrHostMismatch {
		t.Fatalf("other host: err = %v, want ErrHostMismatch", err)
	}
	if _, found, err := v.Get("vault://pt/dicmusic/cookies", "DICMUSIC.COM"); err != nil || !found {
		t.Fatalf("upper-case host: found %v err %v", found, err)
	}
	// Missing reference reports not-found, no error.
	if _, found, err := v.Get("vault://pt/open.cd/cookies", "open.cd"); err != nil || found {
		t.Fatalf("missing ref: found %v err %v", found, err)
	}
	// Overwrite bumps the revision.
	if rev, err := v.Set("vault://pt/dicmusic/cookies", "cookie-value-2", "dicmusic.com"); err != nil || rev != 2 {
		t.Fatalf("overwrite: rev %d err %v", rev, err)
	}
	if v.Revision("vault://pt/dicmusic/cookies") != 2 {
		t.Fatal("Revision did not track the overwrite")
	}
	refs, err := v.ListRefs()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Ref != "vault://pt/dicmusic/cookies" || refs[0].Revision != 2 {
		t.Fatalf("ListRefs: %+v", refs)
	}
	// Multi-host binding, sorted and deduplicated.
	if _, err := v.Set("vault://engine/qq", "engine-cookies", "music.163.com, qq.com, music.163.com"); err != nil {
		t.Fatalf("multi-host Set: %v", err)
	}
	refs, _ = v.ListRefs()
	for _, r := range refs {
		if r.Ref == "vault://engine/qq" && (len(r.Hosts) != 2 || r.Hosts[0] != "music.163.com" || r.Hosts[1] != "qq.com") {
			t.Fatalf("hosts not sorted/deduped: %v", r.Hosts)
		}
	}
}

func TestVaultValidation(t *testing.T) {
	testKeyFile(t)
	v, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		ref   string
		value string
		host  string
	}{
		{"bad ref scheme", "https://example.com/x", "v", "example.com"},
		{"bad ref chars", "vault://a b", "v", "example.com"},
		{"empty value", "vault://a/b", "", "example.com"},
		{"empty host", "vault://a/b", "v", ""},
		{"host with scheme", "vault://a/b", "v", "https://example.com"},
		{"host with port", "vault://a/b", "v", "example.com:8080"},
		{"host with dotdot", "vault://a/b", "v", "exa..mple.com"},
		{"oversized value", "vault://a/b", strings.Repeat("x", maxValueBytes+1), "example.com"},
	}
	for _, tc := range cases {
		if _, err := v.Set(tc.ref, tc.value, tc.host); err == nil {
			t.Fatalf("%s: Set accepted invalid input", tc.name)
		}
	}
}

func TestVaultPythonFixture(t *testing.T) {
	// Fixture written by the Python vault (clients.vault.Vault) with the
	// venv interpreter: one ref, vault://pt/dicmusic/cookies, bound to
	// dicmusic.com. The Go store must read it byte-for-byte.
	keyBytes, err := os.ReadFile("testdata/python-linux-master.key")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile("testdata/python-secrets.aesgcm")
	if err != nil {
		t.Fatal(err)
	}
	profile := t.TempDir()
	keyPath := filepath.Join(profile, "master.key")
	if err := os.WriteFile(keyPath, keyBytes, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSICOPS_VAULT_KEY_FILE", keyPath)
	vaultDir := filepath.Join(profile, "vault")
	if err := os.MkdirAll(vaultDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vaultDir, "secrets.aesgcm"), blob, 0600); err != nil {
		t.Fatal(err)
	}
	v, err := Open(profile)
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := v.Get("vault://pt/dicmusic/cookies", "dicmusic.com")
	if err != nil || !found {
		t.Fatalf("Get python fixture: found %v err %v", found, err)
	}
	want := `[{"name":"session","value":"py-fixture-value","domain":"dicmusic.com","path":"/"}]`
	if got != want {
		t.Fatalf("fixture value mismatch:\n got %s\nwant %s", got, want)
	}
	if v.Revision("vault://pt/dicmusic/cookies") != 1 {
		t.Fatal("fixture revision is not 1")
	}
	// A Go write into the same file must not disturb the Python record.
	if _, err := v.Set("vault://pt/open.cd/cookies", "go-value", "open.cd"); err != nil {
		t.Fatalf("Go Set over python file: %v", err)
	}
	if got, found, err := v.Get("vault://pt/dicmusic/cookies", "dicmusic.com"); err != nil || !found || got != want {
		t.Fatalf("python record after Go write: got %q found %v err %v", got, found, err)
	}
	if got, found, err := v.Get("vault://pt/open.cd/cookies", "open.cd"); err != nil || !found || got != "go-value" {
		t.Fatalf("go record: got %q found %v err %v", got, found, err)
	}
}

func TestVaultConcurrentSet(t *testing.T) {
	testKeyFile(t)
	v, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ref := "vault://test/concurrent/" + string(rune('a'+i))
			if _, err := v.Set(ref, "value", "example.com"); err != nil {
				t.Errorf("Set %s: %v", ref, err)
			}
		}(i)
	}
	wg.Wait()
	refs, err := v.ListRefs()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 16 {
		t.Fatalf("after concurrent sets: %d refs, want 16", len(refs))
	}
}

func TestPrivateDirRejectsSymlink(t *testing.T) {
	testKeyFile(t)
	base := t.TempDir()
	target := filepath.Join(base, "real")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	// The vault directory itself must not be a symlink. (A symlinked
	// profile base is rejected one level up, by profile resolution, the
	// same place Python rejects it.)
	link := filepath.Join(base, "vault")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	v, err := Open(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Set("vault://a/b", "v", "example.com"); err == nil {
		t.Fatal("Set into a symlinked vault directory must fail")
	}
}
