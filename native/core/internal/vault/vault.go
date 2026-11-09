// Package vault is the encrypted credential store, format-compatible with
// the Python clients.vault.Vault: the same vault file names, the same record
// JSON, the same platform protection (Windows DPAPI, macOS Keychain master
// key + AES-GCM, Linux AES-GCM with a separate key file). A vault written by
// the Python desktop opens here, and a vault written here opens in Python;
// the master keys never leave their platform store.
package vault

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Ref is one stored credential reference.
type Ref struct {
	Ref      string   `json:"ref"`
	Hosts    []string `json:"hosts"`
	Revision int      `json:"revision"`
}

type record struct {
	Value    string   `json:"value"`
	Hosts    []string `json:"hosts"`
	Revision int      `json:"revision"`
	// Host is the legacy single-host field the Python store used before the
	// hosts list. It is only read, never written.
	Host string `json:"host,omitempty"`
}

var (
	refPattern  = regexp.MustCompile(`^vault://[A-Za-z0-9_.\-/]+$`)
	hostPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)
)

const maxValueBytes = 1_000_000

// ErrHostMismatch is returned when a credential is asked for a host it is
// not bound to. The value is never returned in that case.
var ErrHostMismatch = errors.New("CREDENTIAL_HOST_MISMATCH")

// Vault is one profile's encrypted credential store.
type Vault struct {
	base string
	path string
}

var (
	locksMu sync.Mutex
	locks   = map[string]*sync.Mutex{}
)

func lockFor(path string) *sync.Mutex {
	locksMu.Lock()
	defer locksMu.Unlock()
	if m, ok := locks[path]; ok {
		return m
	}
	m := &sync.Mutex{}
	locks[path] = m
	return m
}

// Open returns the vault for a profile directory. It does not create any
// file; the store file appears on the first Set.
func Open(base string) (*Vault, error) {
	if base == "" {
		return nil, errors.New("profile directory is required")
	}
	abs, err := filepath.Abs(base)
	if err != nil {
		return nil, err
	}
	name := vaultFileName()
	return &Vault{base: abs, path: filepath.Join(abs, "vault", name)}, nil
}

// Path is the vault store file.
func (v *Vault) Path() string { return v.path }

// Set stores value under ref, bound to host (a single host or a
// comma-separated list). The revision increments on every overwrite.
func (v *Vault) Set(ref, value, host string) (int, error) {
	if !refPattern.MatchString(ref) || host == "" {
		return 0, errors.New("credential reference and bound host are required")
	}
	if value == "" || len([]byte(value)) > maxValueBytes {
		return 0, errors.New("credential must be non-empty text, at most 1MB")
	}
	hosts, err := splitHosts(host)
	if err != nil {
		return 0, err
	}
	if err := privateDir(filepath.Dir(v.path)); err != nil {
		return 0, err
	}
	release, err := lockFile(v.path + ".lock")
	if err != nil {
		return 0, err
	}
	defer release()
	mu := lockFor(v.path)
	mu.Lock()
	defer mu.Unlock()

	data, err := v.load()
	if err != nil {
		return 0, err
	}
	old := data[ref]
	data[ref] = record{Value: value, Hosts: hosts, Revision: old.Revision + 1}
	if err := v.store(data); err != nil {
		return 0, err
	}
	return data[ref].Revision, nil
}

// Get returns the stored value for ref, or found=false when the reference
// does not exist. A host the credential is not bound to yields
// ErrHostMismatch and no value.
func (v *Vault) Get(ref, host string) (string, bool, error) {
	data, err := v.load()
	if err != nil {
		return "", false, err
	}
	rec, ok := data[ref]
	if !ok {
		return "", false, nil
	}
	hosts := rec.Hosts
	if len(hosts) == 0 && rec.Host != "" {
		hosts = []string{rec.Host}
	}
	for _, h := range hosts {
		if h == lowerHost(host) {
			return rec.Value, true, nil
		}
	}
	return "", false, ErrHostMismatch
}

// Revision returns the stored revision of ref, or 0 when it does not exist.
func (v *Vault) Revision(ref string) int {
	data, err := v.load()
	if err != nil || data == nil {
		return 0
	}
	return data[ref].Revision
}

// ListRefs returns every stored reference with its bound hosts and revision.
func (v *Vault) ListRefs() ([]Ref, error) {
	data, err := v.load()
	if err != nil {
		return nil, err
	}
	out := make([]Ref, 0, len(data))
	for ref, rec := range data {
		hosts := rec.Hosts
		if len(hosts) == 0 && rec.Host != "" {
			hosts = []string{rec.Host}
		}
		out = append(out, Ref{Ref: ref, Hosts: hosts, Revision: rec.Revision})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

func (v *Vault) load() (map[string]record, error) {
	raw, err := os.ReadFile(v.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]record{}, nil
	}
	if err != nil {
		return nil, err
	}
	plain, err := Unprotect(raw)
	if err != nil {
		return nil, err
	}
	var data map[string]record
	if err := json.Unmarshal(plain, &data); err != nil {
		return nil, fmt.Errorf("credential store is corrupt: %w", err)
	}
	if data == nil {
		data = map[string]record{}
	}
	return data, nil
}

func (v *Vault) store(data map[string]record) error {
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	blob, err := Protect(encoded)
	if err != nil {
		return err
	}
	dir := filepath.Dir(v.path)
	tmp, err := os.CreateTemp(dir, "secrets-*.new")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Windows readers may briefly hold the old file; keep the complete
	// replacement until that handle closes.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := os.Rename(tmpName, v.path); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func splitHosts(host string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, h := range splitComma(host) {
		h = strings.TrimSpace(lowerHost(h))
		if h == "" {
			continue
		}
		if !hostPattern.MatchString(h) || containsDotDot(h) {
			return nil, errors.New("binding only accepts explicit host names or IPs, no scheme, port or path")
		}
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("binding only accepts explicit host names or IPs, no scheme, port or path")
	}
	sort.Strings(out)
	return out, nil
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func lowerHost(h string) string {
	b := []byte(h)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func containsDotDot(h string) bool {
	for i := 0; i+1 < len(h); i++ {
		if h[i] == '.' && h[i+1] == '.' {
			return true
		}
	}
	return false
}
