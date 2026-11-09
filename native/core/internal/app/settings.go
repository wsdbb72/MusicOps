// Settings storage: one Pebble JSON document per section under
// app/settings/<section>. Secrets never live here: keys, cookies and
// tokens go to the Vault (see vault.go) and only their refs or saved-state
// live in these documents. Every save is a compare-and-swap loop, the same
// shape the site and download settings already use.
package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

// loadJSONSetting reads one settings document. found=false when the key was
// never saved; a corrupt document is an error, never a silent default.
func (a *App) loadJSONSetting(key []byte, out any) (bool, error) {
	var raw []byte
	var found bool
	err := a.engine.View(func(v *dataengine.View) error {
		var err error
		raw, found, err = v.Get(key)
		return err
	})
	if err != nil || !found {
		return false, err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return false, err
	}
	return true, nil
}

// casSaveJSON stores one marshalled settings document with the same
// compare-and-swap retry the older settings writers use: up to 8 attempts
// against a value that changed under the write.
func (a *App) casSaveJSON(key, encoded []byte) error {
	for attempt := 0; attempt < 8; attempt++ {
		var current []byte
		var present bool
		if err := a.engine.View(func(v *dataengine.View) error {
			var err error
			current, present, err = v.Get(key)
			return err
		}); err != nil {
			return err
		}
		conditions := []dataengine.Condition{{Key: key, Absent: true}}
		if present {
			conditions = []dataengine.Condition{{Key: key, Value: current}}
		}
		ok, err := a.engine.CompareAndSwap(conditions, []dataengine.Mutation{{Key: key, Value: encoded}})
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return errors.New("settings changed under the write")
}

// cleanURL parses a service URL the way the Python config validators do: a
// bare origin (or origin + path for the AI/lyrics services), never with
// embedded credentials, a query string or a fragment. It returns the
// cleaned URL and its hostname. httpsOnly rejects plain HTTP, which is
// what the Python AI client and the lyrics primary service require.
func cleanURL(raw string, httpsOnly bool) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", "", errors.New("service url is required")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", errors.New("service url must not carry credentials, a query or a fragment")
	}
	if httpsOnly && parsed.Scheme != "https" {
		return "", "", errors.New("service url must use https")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", "", errors.New("service url must use http or https")
	}
	host := parsed.Hostname()
	if host == "" {
		return "", "", errors.New("service url host is required")
	}
	return strings.TrimSuffix(parsed.String(), "/"), host, nil
}

// cleanOrigin is cleanURL for services that take an origin only: the path
// must be empty or "/".
func cleanOrigin(raw string) (string, string, error) {
	cleaned, host, err := cleanURL(raw, false)
	if err != nil {
		return "", "", err
	}
	parsed, err := url.Parse(cleaned)
	if err != nil {
		return "", "", err
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", "", errors.New("service url must be an origin without a path")
	}
	parsed.Path = ""
	return parsed.String(), host, nil
}

// isLoopbackHost reports whether host is a loopback name or address, the
// only hosts the Python FlareSolverr validator accepts.
func isLoopbackHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// vaultUserRef builds the Python setup.py secret() reference for a
// host-bound user secret: vault://user/<kind>/<sha256(host)[:12]>.
func vaultUserRef(kind, host string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(host)))
	return "vault://user/" + kind + "/" + hex.EncodeToString(sum[:])[:12]
}
