// Package cookies replicates the Cookie-Editor import and send-time
// filtering logic from the Python implementation in
// src/musicops/clients/vault.py (import_cookies and cookie_header).
//
// Only the ten fields the Python version keeps are stored; everything else
// in an export is dropped. Cookies are never sent from here directly:
// ParseEditor validates and filters an export for one host, Marshal and
// Unmarshal round-trip the stored form, and Header selects the cookies
// that apply to one request path at a given moment.
package cookies

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Cookie is one Cookie-Editor entry. JSON tags match the Python field
// names. ExpirationDate and Expires are pointers so that a missing value
// (a session cookie) is distinguishable from an explicit zero.
type Cookie struct {
	Name           string   `json:"name"`
	Value          string   `json:"value"`
	Domain         string   `json:"domain"`
	Path           string   `json:"path"`
	Secure         bool     `json:"secure"`
	ExpirationDate *float64 `json:"expirationDate"`
	Expires        *float64 `json:"expires"`
	HTTPOnly       bool     `json:"httpOnly"`
	SameSite       string   `json:"sameSite"`
	HostOnly       bool     `json:"hostOnly"`
}

var (
	// tokenPattern is the RFC 7230 token rule the Python version applies
	// to cookie names.
	tokenPattern = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)
	// siteLabelPattern matches the human-readable site label some
	// Cookie-Editor exports prepend, e.g. "example.com:\n[...]".
	siteLabelPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+:\s*\n\s*\[`)
)

// ParseEditor parses a Cookie-Editor JSON export for host, keeping only
// the cookies that belong to that host. It mirrors Python import_cookies:
//
//   - a leading "site-name:\n[" label is stripped;
//   - the JSON must be an array of objects;
//   - domain has leading dots stripped and is lower-cased; an entry is
//     skipped unless host equals domain or ends with "."+domain;
//   - a hostOnly entry is skipped unless host equals domain;
//   - name must be a token, value a string without \r, \n, ; or NUL,
//     path defaults to "/" and must start with "/", and expirationDate /
//     expires must be finite numbers when present;
//   - only the ten known fields are kept.
//
// Entries skipped by the domain/hostOnly rules are never validated, so a
// malformed cookie for another site does not fail the import. An error is
// returned when no cookie belongs to host.
func ParseEditor(text, host string) ([]Cookie, error) {
	host = strings.ToLower(host)
	text = strings.TrimSpace(text)
	if siteLabelPattern.MatchString(text) {
		text = strings.TrimSpace(strings.SplitN(text, ":", 2)[1])
	}
	items, err := decodeArray(text)
	if err != nil {
		return nil, err
	}
	result := make([]Cookie, 0, len(items))
	for _, item := range items {
		domain := cookieDomain(item)
		if domain == "" || (host != domain && !strings.HasSuffix(host, "."+domain)) {
			continue
		}
		if v, ok := item["hostOnly"]; ok && v != nil {
			hostOnly, ok := v.(bool)
			if !ok {
				return nil, errors.New("Cookie字段类型无效")
			}
			if hostOnly && host != domain {
				continue
			}
		}
		cookie, err := decodeCookie(item)
		if err != nil {
			return nil, err
		}
		result = append(result, cookie)
	}
	if len(result) == 0 {
		return nil, errors.New("没有属于指定域名的Cookie")
	}
	return result, nil
}

// Marshal renders cookies as canonical JSON for Vault storage: compact,
// all ten fields in a fixed order, no HTML escaping. The output of
// Marshal always parses back to the same cookies.
func Marshal(cookies []Cookie) (string, error) {
	if cookies == nil {
		cookies = []Cookie{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(cookies); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// Unmarshal parses JSON previously produced by Marshal. It applies the
// same per-cookie field validation as ParseEditor but no host filtering,
// so a stored jar loads exactly as it was saved.
func Unmarshal(raw string) ([]Cookie, error) {
	items, err := decodeArray(raw)
	if err != nil {
		return nil, err
	}
	cookies := make([]Cookie, 0, len(items))
	for _, item := range items {
		cookie, err := decodeCookie(item)
		if err != nil {
			return nil, err
		}
		cookies = append(cookies, cookie)
	}
	return cookies, nil
}

// Header selects the cookies that apply to one request, mirroring Python
// cookie_header: expired cookies are skipped, the request path must match
// the cookie path on a boundary (equal, or the cookie path ends with "/",
// or the next character is "/"), and survivors are ordered by descending
// path length and joined as "name=value; ...". An error is returned when
// no cookie is usable.
func Header(cookies []Cookie, path string, now time.Time) (string, error) {
	nowUnix := float64(now.UnixNano()) / 1e9
	type selected struct {
		pathLen int
		pair    string
	}
	selectedCookies := make([]selected, 0, len(cookies))
	for _, cookie := range cookies {
		if expiry := cookieExpiry(cookie); expiry > 0 && expiry <= nowUnix {
			continue
		}
		cookiePath := cookie.Path
		if cookiePath == "" {
			cookiePath = "/"
		}
		if path != cookiePath && !(strings.HasPrefix(path, cookiePath) &&
			(strings.HasSuffix(cookiePath, "/") || strings.HasPrefix(path[len(cookiePath):], "/"))) {
			continue
		}
		selectedCookies = append(selectedCookies, selected{len(cookiePath), cookie.Name + "=" + cookie.Value})
	}
	sort.SliceStable(selectedCookies, func(i, j int) bool {
		return selectedCookies[i].pathLen > selectedCookies[j].pathLen
	})
	if len(selectedCookies) == 0 {
		return "", errors.New("没有可用Cookie；请重新导出登录态")
	}
	pairs := make([]string, len(selectedCookies))
	for i, item := range selectedCookies {
		pairs[i] = item.pair
	}
	return strings.Join(pairs, "; "), nil
}

// decodeArray parses text as a JSON array of objects, with numbers kept
// as json.Number so expiry validation can tell integers, floats and
// out-of-range values apart.
func decodeArray(text string) ([]map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var top any
	if err := dec.Decode(&top); err != nil {
		return nil, fmt.Errorf("Cookie JSON解析失败: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("Cookie JSON解析失败: 存在多余内容")
		}
		return nil, fmt.Errorf("Cookie JSON解析失败: %w", err)
	}
	items, ok := top.([]any)
	if !ok {
		return nil, errors.New("Cookie JSON必须是数组")
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("Cookie项必须为对象")
		}
		result = append(result, obj)
	}
	return result, nil
}

// cookieDomain returns the normalized domain of one export entry: leading
// dots stripped, lower-cased. A missing or non-string domain yields "".
func cookieDomain(item map[string]any) string {
	domain, _ := item["domain"].(string)
	return strings.ToLower(strings.TrimLeft(domain, "."))
}

// decodeCookie validates one export object and returns the Cookie with
// only the ten kept fields. Domain is stored normalized (leading dots
// stripped, lower-cased); path defaults to "/".
func decodeCookie(item map[string]any) (Cookie, error) {
	var cookie Cookie
	name, ok := item["name"].(string)
	if !ok || !tokenPattern.MatchString(name) {
		return Cookie{}, errors.New("Cookie字段不完整")
	}
	value, ok := item["value"].(string)
	if !ok {
		return Cookie{}, errors.New("Cookie字段不完整")
	}
	if strings.ContainsAny(value, "\r\n;\x00") {
		return Cookie{}, errors.New("Cookie值包含不允许的字符")
	}
	cookie.Name = name
	cookie.Value = value
	cookie.Domain = cookieDomain(item)
	switch raw := item["path"]; {
	case raw == nil:
		cookie.Path = "/"
	case isEmptyString(raw):
		cookie.Path = "/"
	default:
		path, ok := raw.(string)
		if !ok || !strings.HasPrefix(path, "/") {
			return Cookie{}, errors.New("Cookie路径无效")
		}
		cookie.Path = path
	}
	for _, key := range []string{"expirationDate", "expires"} {
		expiry, err := decodeExpiry(item[key])
		if err != nil {
			return Cookie{}, err
		}
		if key == "expirationDate" {
			cookie.ExpirationDate = expiry
		} else {
			cookie.Expires = expiry
		}
	}
	for _, field := range []struct {
		key string
		dst *bool
	}{
		{"secure", &cookie.Secure},
		{"httpOnly", &cookie.HTTPOnly},
		{"hostOnly", &cookie.HostOnly},
	} {
		if raw, ok := item[field.key]; ok && raw != nil {
			flag, ok := raw.(bool)
			if !ok {
				return Cookie{}, errors.New("Cookie字段类型无效")
			}
			*field.dst = flag
		}
	}
	if raw, ok := item["sameSite"]; ok && raw != nil {
		sameSite, ok := raw.(string)
		if !ok {
			return Cookie{}, errors.New("Cookie字段类型无效")
		}
		cookie.SameSite = sameSite
	}
	return cookie, nil
}

func isEmptyString(v any) bool {
	s, ok := v.(string)
	return ok && s == ""
}

// decodeExpiry validates one expiry field: absent or null yields nil, a
// JSON number must parse to a finite float, anything else is an error.
func decodeExpiry(raw any) (*float64, error) {
	if raw == nil {
		return nil, nil
	}
	num, ok := raw.(json.Number)
	if !ok {
		return nil, errors.New("Cookie过期时间无效")
	}
	value, err := num.Float64()
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return nil, errors.New("Cookie过期时间无效")
	}
	return &value, nil
}

// cookieExpiry mirrors Python's `expirationDate or expires or 0`: the
// first non-zero expiry wins, zero means a session cookie.
func cookieExpiry(cookie Cookie) float64 {
	if cookie.ExpirationDate != nil && *cookie.ExpirationDate != 0 {
		return *cookie.ExpirationDate
	}
	if cookie.Expires != nil && *cookie.Expires != 0 {
		return *cookie.Expires
	}
	return 0
}
