package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestWebUIExposesEmbeddedBrandIconAndManifest(t *testing.T) {
	if len(appIconPNG) < 1024 || !bytes.HasPrefix(appIconPNG, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("embedded icon is not a PNG: bytes=%d", len(appIconPNG))
	}
	application := newApp(t)
	home := getAuth(t, application, "/")
	if home.Code != http.StatusOK {
		t.Fatalf("home status=%d", home.Code)
	}
	pageBody := home.Body.String()
	csp := home.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self' data:") || !strings.Contains(csp, "manifest-src 'self'") {
		t.Fatalf("CSP does not allow the brand icon or manifest: %q", csp)
	}
	for _, expected := range []string{
		`rel="icon"`, `href="/brand-icon.png"`, `rel="apple-touch-icon"`, `rel="manifest"`, `name="theme-color"`, `src="/brand-icon.png"`,
	} {
		if !strings.Contains(pageBody, expected) {
			t.Fatalf("home page missing %s", expected)
		}
	}
	if strings.Contains(pageBody, "data:image/png;base64,") {
		t.Fatal("home page still duplicates the icon as inline base64")
	}
	for _, path := range []string{"/brand-icon.png", "/apple-touch-icon.png", "/favicon.ico"} {
		response := getAuth(t, application, path)
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/png" || !bytes.Equal(response.Body.Bytes(), appIconPNG) {
			t.Fatalf("%s status=%d type=%q bytes=%d", path, response.Code, response.Header().Get("Content-Type"), response.Body.Len())
		}
	}
	manifest := getAuth(t, application, "/app.webmanifest")
	if manifest.Code != http.StatusOK || !strings.Contains(manifest.Header().Get("Content-Type"), "application/manifest+json") {
		t.Fatalf("manifest status=%d type=%q", manifest.Code, manifest.Header().Get("Content-Type"))
	}
	var decoded struct {
		Name      string `json:"name"`
		ShortName string `json:"short_name"`
		StartURL  string `json:"start_url"`
		Display   string `json:"display"`
		Icons     []struct {
			Source string `json:"src"`
			Sizes  string `json:"sizes"`
			Type   string `json:"type"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(manifest.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Name != "MusicOps" || decoded.ShortName != "MusicOps" || decoded.StartURL != "/" || decoded.Display != "standalone" || len(decoded.Icons) != 1 || decoded.Icons[0].Source != "/brand-icon.png" || decoded.Icons[0].Sizes != "1024x1024" || decoded.Icons[0].Type != "image/png" {
		t.Fatalf("manifest=%+v", decoded)
	}
}
