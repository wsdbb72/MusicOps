// Reverse proxy from the desktop window to the Go core.
//
// The Wails window serves no frontend of its own: every request goes to the
// core's WebUI/API on 127.0.0.1, with the core's Basic credentials injected
// (the same credentials Docker deployments type into the browser). The
// proxy never exposes the password or the core's internal address to the
// caller: an unreachable core is a 502 with a fixed JSON body.
package desktop

import (
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// hopHeaders are removed from proxied requests and responses (RFC 9110
// §7.6.1 hop-by-hop headers).
var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func removeHopHeaders(h http.Header) {
	// Headers listed by Connection are hop-by-hop too.
	for _, name := range h["Connection"] {
		for _, field := range splitCommaHeader(name) {
			h.Del(field)
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
}

func splitCommaHeader(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, trimSpace(s[start:i]))
			start = i + 1
		}
	}
	return append(out, trimSpace(s[start:]))
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// NewProxy returns a handler proxying every request to coreURL (e.g.
// http://127.0.0.1:54321) with `Authorization: Basic
// base64("musicops:"+password)` injected. Hop-by-hop headers are stripped
// in both directions, the core's status code and body are preserved, and an
// unreachable core yields 502 {"ok":false,"code":"CORE_UNAVAILABLE"}
// without leaking the password or the core address. Client timeout is
// 30 seconds. An unparsable coreURL yields a handler that always answers
// 502.
func NewProxy(coreURL, password string) http.Handler {
	target, err := url.Parse(coreURL)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeCoreUnavailable(w)
		})
	}
	credentials := base64.StdEncoding.EncodeToString([]byte("musicops:" + password))
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = target.Scheme
			r.URL.Host = target.Host
			r.Host = target.Host
			removeHopHeaders(r.Header)
			r.Header.Set("Authorization", "Basic "+credentials)
		},
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			ResponseHeaderTimeout: 30 * time.Second,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          16,
		},
		ModifyResponse: func(resp *http.Response) error {
			removeHopHeaders(resp.Header)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeCoreUnavailable(w)
		},
	}
	return proxy
}

func writeCoreUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write([]byte(`{"ok":false,"code":"CORE_UNAVAILABLE"}`))
}
