package trackersearch

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The browser solver is a fallback, not a transport. A site that answers
// directly is never sent through a browser; only a read-only GET that came
// back as a Cloudflare interstitial is retried, once, through the loopback
// solver. That keeps ordinary reads fast, keeps the solver's browser out of
// authenticated flows it cannot complete, and makes "why was the browser
// used" answerable from the observation.
//
// Cookies are handed to the solver scoped to the site host: the interstitial
// appears before the page for a logged-in user too, so solving without the
// session just produces another interstitial. They are passed in the request
// body only, never part of a returned observation.

// engineTokenHeader is the header the bundled wrapper requires.
const engineTokenHeader = "X-MusicOps-Service"

const (
	// solverMinTimeoutMS and solverMaxTimeoutMS bound one browser solve. The
	// settings allow 10-120 s, and the solver adds its own page wait.
	solverMinTimeoutMS = 10_000
	solverMaxTimeoutMS = 120_000
)

// challengeMarkers are the phrases a Cloudflare interstitial carries. They
// only count on 403/503: a normal tracker error page also answers 403, and
// handing that to a browser would hide an expired session behind two minutes
// of waiting.
var challengeMarkers = [][]byte{
	[]byte("just a moment"),
	[]byte("cf-browser-verification"),
	[]byte("cf_chl_opt"),
	[]byte("__cf_chl"),
	[]byte("cf-challenge"),
	[]byte("cf_chl_"),
	[]byte("attention required! | cloudflare"),
	[]byte("enable javascript and cookies to continue"),
	[]byte("checking your browser before accessing"),
}

// looksChallenged reports whether a read answered with a browser challenge
// instead of the document that was asked for.
func looksChallenged(status int, body []byte) bool {
	if status != http.StatusForbidden && status != http.StatusServiceUnavailable {
		return false
	}
	if len(body) == 0 || (!bytes.Contains(body, []byte("<")) && !bytes.Contains(body, []byte("{"))) {
		return false
	}
	lower := bytes.ToLower(body)
	for _, marker := range challengeMarkers {
		if bytes.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// solverBase validates a solver address. Only loopback is accepted: the
// browser that solves the challenge has to run next to this process, because
// the site cookie goes into it.
func solverBase(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	switch parsed.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		return ""
	}
	return strings.TrimRight(raw, "/")
}

// solverCookies turns the site cookie header into the solver's cookie list,
// scoped to that site's host. An unparseable pair is skipped rather than sent
// with a guessed domain.
func solverCookies(site, cookie string) []map[string]any {
	host := Hosts[site]
	if host == "" || strings.TrimSpace(cookie) == "" {
		return nil
	}
	out := []map[string]any{}
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, found := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		if !found || name == "" {
			continue
		}
		out = append(out, map[string]any{"name": name, "value": strings.TrimSpace(value), "domain": host, "path": "/"})
	}
	return out
}

// solverTimeoutMS converts the configured request timeout into the solver's
// millisecond budget, clamped to the range the settings allow.
func solverTimeoutMS(q Query) int {
	ms := int(q.Timeout / time.Millisecond)
	if ms <= 0 {
		ms = solverMaxTimeoutMS / 2
	}
	if ms < solverMinTimeoutMS {
		ms = solverMinTimeoutMS
	}
	if ms > solverMaxTimeoutMS {
		ms = solverMaxTimeoutMS
	}
	return ms
}

// solverPage is one decoded FlareSolverr reply.
type solverPage struct {
	Status   string `json:"status"`
	Message  string `json:"message"`
	Solution struct {
		Status   int    `json:"status"`
		Response string `json:"response"`
		URL      string `json:"url"`
	} `json:"solution"`
}

// solverGet asks the loopback solver for one page and returns it only when
// the answer is a usable document. A solved page that is itself still a
// challenge is refused: the caller then reports the failure instead of
// parsing an interstitial as if it were search results.
func (c *Client) solverGet(ctx context.Context, q Query, pageURL string) ([]byte, bool) {
	base := solverBase(q.SolverURL)
	if base == "" {
		return nil, false
	}
	payload := map[string]any{
		"cmd":        "request.get",
		"url":        pageURL,
		"maxTimeout": solverTimeoutMS(q),
	}
	if cookies := solverCookies(q.Site, q.Cookie); len(cookies) > 0 {
		payload["cookies"] = cookies
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1", bytes.NewReader(encoded))
	if err != nil {
		return nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	if q.SolverToken != "" {
		req.Header.Set(engineTokenHeader, q.SolverToken)
	}
	response, err := c.doer(q.Timeout).Do(req)
	if err != nil {
		return nil, false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, false
	}
	var decoded solverPage
	if json.NewDecoder(io.LimitReader(response.Body, maxBody)).Decode(&decoded) != nil {
		return nil, false
	}
	if decoded.Status != "ok" || decoded.Solution.Response == "" {
		return nil, false
	}
	if decoded.Solution.Status != 0 && decoded.Solution.Status != http.StatusOK {
		return nil, false
	}
	page := []byte(decoded.Solution.Response)
	if looksChallenged(http.StatusForbidden, page) {
		return nil, false
	}
	// The solved page must still be the site that was asked for: a solver that
	// followed a login or cross-site redirect returns a document this client
	// must not treat as the tracker's page.
	if decoded.Solution.URL != "" {
		want, wantErr := url.Parse(pageURL)
		got, gotErr := url.Parse(decoded.Solution.URL)
		if wantErr == nil && gotErr == nil && !strings.EqualFold(want.Hostname(), got.Hostname()) {
			return nil, false
		}
	}
	return page, true
}
