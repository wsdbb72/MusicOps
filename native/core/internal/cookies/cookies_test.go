package cookies

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func fptr(v float64) *float64 { return &v }

func TestParseEditorBasicAndDomainFiltering(t *testing.T) {
	raw := `[
		{"name":"sid","value":"abc","domain":".Example.COM","path":"/","secure":true,"httpOnly":true,"sameSite":"lax","hostOnly":false,"storeId":"0","id":7},
		{"name":"sub","value":"s","domain":"example.com","path":"/api"},
		{"name":"other","value":"x","domain":"other.com","path":"/"},
		{"name":"bad name","value":"x","domain":"other.com","path":"/"},
		{"name":"bad;value","value":"a;b","domain":"other.com","path":"/"}
	]`
	cookies, err := ParseEditor(raw, "sub.example.com")
	if err != nil {
		t.Fatalf("ParseEditor: %v", err)
	}
	if len(cookies) != 2 {
		t.Fatalf("got %d cookies, want 2: %+v", len(cookies), cookies)
	}
	// Domain is stored normalized: leading dot stripped, lower-cased.
	if cookies[0].Domain != "example.com" {
		t.Errorf("domain = %q, want %q", cookies[0].Domain, "example.com")
	}
	if !cookies[0].Secure || !cookies[0].HTTPOnly || cookies[0].SameSite != "lax" {
		t.Errorf("flags not kept: %+v", cookies[0])
	}
	if cookies[0].Path != "/" || cookies[1].Name != "sub" || cookies[1].Path != "/api" {
		t.Errorf("unexpected cookies: %+v", cookies)
	}
	// Unknown fields (storeId, id) are dropped by Marshal.
	out, err := Marshal(cookies)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(out, "storeId") || strings.Contains(out, `"id"`) {
		t.Errorf("Marshal kept dropped fields: %s", out)
	}
	// Malformed entries for other domains are skipped, not errors.
}

func TestParseEditorSiteLabel(t *testing.T) {
	raw := "music.example.com:\n  " + `[{"name":"sid","value":"abc","domain":"example.com","path":"/"}]`
	cookies, err := ParseEditor(raw, "example.com")
	if err != nil {
		t.Fatalf("ParseEditor with site label: %v", err)
	}
	if len(cookies) != 1 || cookies[0].Name != "sid" {
		t.Fatalf("unexpected cookies: %+v", cookies)
	}
}

func TestParseEditorHostOnly(t *testing.T) {
	raw := `[
		{"name":"exact","value":"e","domain":"example.com","path":"/","hostOnly":true},
		{"name":"sub","value":"s","domain":"example.com","path":"/","hostOnly":true},
		{"name":"plain","value":"p","domain":"example.com","path":"/","hostOnly":false}
	]`
	// Exact host: hostOnly entries are kept.
	cookies, err := ParseEditor(raw, "example.com")
	if err != nil {
		t.Fatalf("ParseEditor: %v", err)
	}
	if len(cookies) != 3 {
		t.Fatalf("exact host: got %d cookies, want 3", len(cookies))
	}
	// Subdomain host: hostOnly entries are skipped.
	cookies, err = ParseEditor(raw, "sub.example.com")
	if err != nil {
		t.Fatalf("ParseEditor: %v", err)
	}
	if len(cookies) != 1 || cookies[0].Name != "plain" {
		t.Fatalf("subdomain host: got %+v, want only plain", cookies)
	}
}

func TestParseEditorNoMatch(t *testing.T) {
	raw := `[{"name":"a","value":"b","domain":"other.com","path":"/"}]`
	if _, err := ParseEditor(raw, "example.com"); err == nil {
		t.Error("expected error when no cookie belongs to the host")
	}
	if _, err := ParseEditor(`[]`, "example.com"); err == nil {
		t.Error("expected error for an empty export")
	}
}

func TestParseEditorInvalid(t *testing.T) {
	cases := map[string]string{
		"not json":         `not json`,
		"not array":        `{"name":"a"}`,
		"trailing data":    `[] []`,
		"item not object":  `[1]`,
		"bad name space":   `[{"name":"a b","value":"v","domain":"example.com","path":"/"}]`,
		"empty name":       `[{"name":"","value":"v","domain":"example.com","path":"/"}]`,
		"name not string":  `[{"name":1,"value":"v","domain":"example.com","path":"/"}]`,
		"value semicolon":  `[{"name":"a","value":"x;y","domain":"example.com","path":"/"}]`,
		"value newline":    `[{"name":"a","value":"x\ny","domain":"example.com","path":"/"}]`,
		"value NUL":        `[{"name":"a","value":"x\u0000y","domain":"example.com","path":"/"}]`,
		"relative path":    `[{"name":"a","value":"v","domain":"example.com","path":"api"}]`,
		"expiry string":    `[{"name":"a","value":"v","domain":"example.com","path":"/","expirationDate":"soon"}]`,
		"expiry bool":      `[{"name":"a","value":"v","domain":"example.com","path":"/","expires":true}]`,
		"expiry overflow":  `[{"name":"a","value":"v","domain":"example.com","path":"/","expirationDate":1e999}]`,
		"secure not bool":  `[{"name":"a","value":"v","domain":"example.com","path":"/","secure":"yes"}]`,
		"sameSite not str": `[{"name":"a","value":"v","domain":"example.com","path":"/","sameSite":1}]`,
	}
	for name, raw := range cases {
		if _, err := ParseEditor(raw, "example.com"); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestParseEditorDefaults(t *testing.T) {
	raw := `[{"name":"a","value":"v","domain":"example.com"}]`
	cookies, err := ParseEditor(raw, "example.com")
	if err != nil {
		t.Fatalf("ParseEditor: %v", err)
	}
	if cookies[0].Path != "/" {
		t.Errorf("path = %q, want %q", cookies[0].Path, "/")
	}
	if cookies[0].ExpirationDate != nil || cookies[0].Expires != nil {
		t.Errorf("expiry should be nil for a session cookie: %+v", cookies[0])
	}
	if cookies[0].Secure || cookies[0].HTTPOnly || cookies[0].HostOnly || cookies[0].SameSite != "" {
		t.Errorf("zero flags expected: %+v", cookies[0])
	}
}

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	cookies := []Cookie{
		{Name: "sid", Value: "abc", Domain: "example.com", Path: "/", Secure: true, ExpirationDate: fptr(1893456000), HTTPOnly: true, SameSite: "lax"},
		{Name: "pref", Value: "dark", Domain: "example.com", Path: "/api", Expires: fptr(1893456000.5), HostOnly: true},
		{Name: "sess", Value: "s", Domain: "example.com", Path: "/"},
	}
	raw, err := Marshal(cookies)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	back, err := Unmarshal(raw)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(back, cookies) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v\nraw: %s", back, cookies, raw)
	}
	// Marshal(Unmarshal(x)) reproduces canonical JSON text exactly.
	again, err := Marshal(back)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if again != raw {
		t.Errorf("Marshal(Unmarshal(x)) = %s, want %s", again, raw)
	}
	// Field order follows the Python keep-list.
	if !strings.HasPrefix(raw, `[{"name":"sid","value":"abc","domain":"example.com","path":"/","secure":true,"expirationDate":1893456000,"expires":null,"httpOnly":true,"sameSite":"lax","hostOnly":false}`) {
		t.Errorf("unexpected Marshal output: %s", raw)
	}
}

func TestMarshalUnmarshalEmptyAndInvalid(t *testing.T) {
	raw, err := Marshal(nil)
	if err != nil || raw != "[]" {
		t.Errorf("Marshal(nil) = %q, %v; want %q", raw, err, "[]")
	}
	cookies, err := Unmarshal("[]")
	if err != nil || len(cookies) != 0 {
		t.Errorf("Unmarshal([]) = %+v, %v", cookies, err)
	}
	for _, bad := range []string{``, `not json`, `{}`, `[1]`, `[{"name":"a b","value":"v","domain":"example.com"}]`} {
		if _, err := Unmarshal(bad); err == nil {
			t.Errorf("Unmarshal(%q): expected error", bad)
		}
	}
}

func TestHeaderExpiry(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cookies := []Cookie{
		{Name: "live", Value: "1", Domain: "example.com", Path: "/", ExpirationDate: fptr(1700000001)},
		{Name: "dead", Value: "2", Domain: "example.com", Path: "/", ExpirationDate: fptr(1700000000)},
		{Name: "past", Value: "3", Domain: "example.com", Path: "/", Expires: fptr(1699999999)},
		{Name: "sess", Value: "4", Domain: "example.com", Path: "/"},
		// expirationDate 0 falls through to expires (Python `or` rule).
		{Name: "fallback-dead", Value: "5", Domain: "example.com", Path: "/", ExpirationDate: fptr(0), Expires: fptr(1699999999)},
		{Name: "fallback-live", Value: "6", Domain: "example.com", Path: "/", ExpirationDate: fptr(0), Expires: fptr(1700000001)},
		// expirationDate wins over expires when non-zero.
		{Name: "precedence", Value: "7", Domain: "example.com", Path: "/", ExpirationDate: fptr(1700000001), Expires: fptr(1)},
	}
	header, err := Header(cookies, "/", now)
	if err != nil {
		t.Fatalf("Header: %v", err)
	}
	want := "live=1; sess=4; fallback-live=6; precedence=7"
	if header != want {
		t.Errorf("Header = %q, want %q", header, want)
	}
}

func TestHeaderPathBoundary(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cookies := []Cookie{
		{Name: "root", Value: "r", Domain: "example.com", Path: "/"},
		{Name: "api", Value: "a", Domain: "example.com", Path: "/api"},
		{Name: "apis", Value: "s", Domain: "example.com", Path: "/api/"},
		{Name: "deep", Value: "d", Domain: "example.com", Path: "/api/users"},
	}
	header, err := Header(cookies, "/api/users/123", now)
	if err != nil {
		t.Fatalf("Header: %v", err)
	}
	// Longest path first; "/api" matches because the next character is
	// "/", while "/api/" also matches via its trailing slash.
	want := "deep=d; apis=s; api=a; root=r"
	if header != want {
		t.Errorf("Header = %q, want %q", header, want)
	}
	// "/apiary" must not match cookie path "/api" (next char is not "/").
	header, err = Header(cookies, "/apiary", now)
	if err != nil {
		t.Fatalf("Header: %v", err)
	}
	if header != "root=r" {
		t.Errorf("Header(/apiary) = %q, want %q", header, "root=r")
	}
	// Exact path match.
	header, err = Header(cookies, "/api", now)
	if err != nil {
		t.Fatalf("Header: %v", err)
	}
	if header != "api=a; root=r" {
		t.Errorf("Header(/api) = %q, want %q", header, "api=a; root=r")
	}
}

func TestHeaderNoUsable(t *testing.T) {
	now := time.Unix(1700000000, 0)
	expired := []Cookie{{Name: "a", Value: "1", Domain: "example.com", Path: "/", ExpirationDate: fptr(1)}}
	if _, err := Header(expired, "/", now); err == nil {
		t.Error("expected error when every cookie is expired")
	}
	wrongPath := []Cookie{{Name: "a", Value: "1", Domain: "example.com", Path: "/api"}}
	if _, err := Header(wrongPath, "/other", now); err == nil {
		t.Error("expected error when no cookie matches the path")
	}
	if _, err := Header(nil, "/", now); err == nil {
		t.Error("expected error for an empty jar")
	}
}
