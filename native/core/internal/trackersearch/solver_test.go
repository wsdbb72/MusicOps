package trackersearch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// scriptDoer answers from a function so a test can model a tracker and a
// solver on the same client, and records both the URL and the body.
type scriptDoer struct {
	calls  []string
	bodies []string
	answer func(req *http.Request, body string) *http.Response
}

func (d *scriptDoer) Do(req *http.Request) (*http.Response, error) {
	raw := ""
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		raw = string(body)
	}
	d.calls = append(d.calls, req.URL.String())
	d.bodies = append(d.bodies, raw)
	return d.answer(req, raw), nil
}

func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func solverReply(t *testing.T, page string) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"status":   "ok",
		"message":  "",
		"solution": map[string]any{"status": 200, "response": page},
	})
	if err != nil {
		t.Fatal(err)
	}
	return reply(200, string(encoded))
}

const gazellePage = `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"Example","artist":"Artist","torrents":[{"torrentId":11,"format":"FLAC","encoding":"Lossless","seeders":3}]}]}}`

// challenge is the interstitial Cloudflare serves before the real page.
const challenge = `<html><head><title>Just a moment...</title></head><body>Checking your browser before accessing dicmusic.com</body></html>`

func isSolverCall(rawURL string) bool {
	return strings.HasSuffix(rawURL, "/v1")
}

// A direct answer is never sent through a browser: the solver only sees a read
// that came back as a challenge.
func TestSolverIsUsedOnlyAfterAChallenge(t *testing.T) {
	doer := &scriptDoer{answer: func(req *http.Request, body string) *http.Response {
		if isSolverCall(req.URL.String()) {
			return solverReply(t, gazellePage)
		}
		return reply(403, challenge)
	}}
	out, err := (&Client{Doer: doer}).Search(context.Background(), Query{
		Site: "dicmusic", Text: "Example", Cookie: "session=virtual", SolverURL: "http://127.0.0.1:8191",
	})
	if err != nil || out.Status != "OK" || !out.SolverUsed || len(out.Candidates) != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	if len(doer.calls) != 2 || !isSolverCall(doer.calls[1]) {
		t.Fatalf("calls = %v", doer.calls)
	}
}

// An expired session answers 403 without any challenge marker. Replaying that
// through a browser would only produce the login form, so it stays an auth
// failure and the solver is never called.
func TestSolverIsNotUsedForAnOrdinaryAuthFailure(t *testing.T) {
	doer := &scriptDoer{answer: func(req *http.Request, body string) *http.Response {
		if isSolverCall(req.URL.String()) {
			t.Error("the solver was called for a plain 403")
			return solverReply(t, gazellePage)
		}
		return reply(403, `<html><body>Login required</body></html>`)
	}}
	out, err := (&Client{Doer: doer}).Search(context.Background(), Query{
		Site: "dicmusic", Text: "Example", Cookie: "session=expired", SolverURL: "http://127.0.0.1:8191",
	})
	if err != nil || out.Status != "AUTH_FAILED" || out.SolverUsed || len(doer.calls) != 1 {
		t.Fatalf("%+v %v %v", out, err, doer.calls)
	}
}

// Rate limiting is not a challenge: the solver would make it worse.
func TestSolverIsNotUsedForRateLimit(t *testing.T) {
	doer := &scriptDoer{answer: func(req *http.Request, body string) *http.Response {
		if isSolverCall(req.URL.String()) {
			t.Error("the solver was called for a 429")
			return solverReply(t, gazellePage)
		}
		response := reply(429, "")
		response.Header.Set("Retry-After", "30")
		return response
	}}
	out, err := (&Client{Doer: doer}).Search(context.Background(), Query{
		Site: "dicmusic", Text: "Example", Cookie: "session=virtual", SolverURL: "http://127.0.0.1:8191",
	})
	if err != nil || out.Status != "RATE_LIMITED" || out.SolverUsed || len(doer.calls) != 1 {
		t.Fatalf("%+v %v %v", out, err, doer.calls)
	}
}

// The browser must look like the signed-in client: the site cookie goes to the
// solver, scoped to that site's host and nowhere else.
func TestSolverGetsTheSiteCookieScopedToItsHost(t *testing.T) {
	doer := &scriptDoer{answer: func(req *http.Request, body string) *http.Response {
		if isSolverCall(req.URL.String()) {
			var payload struct {
				Cmd     string `json:"cmd"`
				URL     string `json:"url"`
				Cookies []struct {
					Name   string `json:"name"`
					Value  string `json:"value"`
					Domain string `json:"domain"`
				} `json:"cookies"`
			}
			if err := json.Unmarshal([]byte(body), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Cmd != "request.get" || len(payload.Cookies) != 2 {
				t.Fatalf("payload = %s", body)
			}
			if payload.Cookies[0].Name != "session" || payload.Cookies[0].Value != "virtual" || payload.Cookies[0].Domain != "dicmusic.com" {
				t.Fatalf("cookies = %+v", payload.Cookies)
			}
			// The tracker cookie must never be sent to a different site.
			if strings.Contains(body, "orpheus") {
				t.Fatalf("payload leaked another site: %s", body)
			}
			return solverReply(t, gazellePage)
		}
		return reply(403, challenge)
	}}
	out, err := (&Client{Doer: doer}).Search(context.Background(), Query{
		Site: "dicmusic", Text: "Example", Cookie: "session=virtual; other=1", SolverURL: "http://127.0.0.1:8191",
	})
	if err != nil || out.Status != "OK" || !out.SolverUsed {
		t.Fatalf("%+v %v", out, err)
	}
}

// A solved page that is still an interstitial is refused: parsing it would
// report an empty result set as if the site had no matches.
func TestSolvedPageThatIsStillAChallengeIsRefused(t *testing.T) {
	doer := &scriptDoer{answer: func(req *http.Request, body string) *http.Response {
		if isSolverCall(req.URL.String()) {
			return solverReply(t, challenge)
		}
		return reply(503, challenge)
	}}
	out, err := (&Client{Doer: doer}).Search(context.Background(), Query{
		Site: "dicmusic", Text: "Example", Cookie: "session=virtual", SolverURL: "http://127.0.0.1:8191",
	})
	if err != nil || out.Status != "BROWSER_CHALLENGE" || out.SolverUsed {
		t.Fatalf("%+v %v", out, err)
	}
}

// A solver address that is not loopback is ignored, because the site cookie
// would leave the machine.
func TestNonLoopbackSolverIsIgnored(t *testing.T) {
	doer := &scriptDoer{answer: func(req *http.Request, body string) *http.Response {
		if isSolverCall(req.URL.String()) {
			t.Error("a remote solver address was used")
			return solverReply(t, gazellePage)
		}
		return reply(403, challenge)
	}}
	out, err := (&Client{Doer: doer}).Search(context.Background(), Query{
		Site: "dicmusic", Text: "Example", Cookie: "session=virtual", SolverURL: "https://solver.example",
	})
	if err != nil || out.Status != "AUTH_FAILED" || out.SolverUsed || len(doer.calls) != 1 {
		t.Fatalf("%+v %v %v", out, err, doer.calls)
	}
}

// The torrent detail page is where the token button lives; it is a read-only
// GET, so a challenge there is recoverable too.
func TestDetailChallengeGoesThroughTheSolver(t *testing.T) {
	doer := &scriptDoer{answer: func(req *http.Request, body string) *http.Response {
		if isSolverCall(req.URL.String()) {
			return solverReply(t, `<html><a href="torrents.php?action=download&amp;id=11">DL</a></html>`)
		}
		return reply(403, challenge)
	}}
	page, err := (&Client{Doer: doer}).ReadDetail(context.Background(), Query{
		Site: "dicmusic", Cookie: "session=virtual", SolverURL: "http://127.0.0.1:8191",
	}, "/ajax.php", url.Values{"action": {"torrent"}, "id": {"11"}})
	if err != nil || !strings.Contains(string(page), "action=download") {
		t.Fatalf("%q %v", page, err)
	}
	if len(doer.calls) != 2 || !isSolverCall(doer.calls[1]) {
		t.Fatalf("calls = %v", doer.calls)
	}
}

// A detail page that simply refuses is not a challenge and is not replayed.
func TestDetailPlainForbiddenIsNotReplayed(t *testing.T) {
	doer := &scriptDoer{answer: func(req *http.Request, body string) *http.Response {
		if isSolverCall(req.URL.String()) {
			t.Error("the solver was called for a plain 403 detail page")
			return solverReply(t, "<html>unexpected</html>")
		}
		return reply(403, `<html>Not allowed</html>`)
	}}
	if _, err := (&Client{Doer: doer}).ReadDetail(context.Background(), Query{
		Site: "dicmusic", Cookie: "session=virtual", SolverURL: "http://127.0.0.1:8191",
	}, "/ajax.php", url.Values{"action": {"torrent"}, "id": {"11"}}); err == nil {
		t.Fatal("a plain 403 detail page was accepted")
	}
	if len(doer.calls) != 1 {
		t.Fatalf("calls = %v", doer.calls)
	}
}

// The token balance page is read the same way. A challenge that cannot be
// solved must not be parsed as a zero balance.
func TestAccountChallengeIsSolvedOrReported(t *testing.T) {
	doer := &scriptDoer{answer: func(req *http.Request, body string) *http.Response {
		if isSolverCall(req.URL.String()) {
			return solverReply(t, `{"status":"success","response":{"username":"me","tokens":42}}`)
		}
		return reply(403, challenge)
	}}
	page, status, _, err := (&Client{Doer: doer}).ReadAccount(context.Background(), Query{
		Site: "redacted", Cookie: "session=virtual", SolverURL: "http://127.0.0.1:8191",
	}, url.Values{"action": {"index"}})
	if err != nil || status != 200 || !strings.Contains(string(page), `"tokens":42`) {
		t.Fatalf("%q %d %v", page, status, err)
	}

	unsolved := &scriptDoer{answer: func(req *http.Request, body string) *http.Response {
		if isSolverCall(req.URL.String()) {
			return reply(500, `{"status":"error","message":"timeout"}`)
		}
		return reply(403, challenge)
	}}
	if _, _, _, err := (&Client{Doer: unsolved}).ReadAccount(context.Background(), Query{
		Site: "redacted", Cookie: "session=virtual", SolverURL: "http://127.0.0.1:8191",
	}, url.Values{"action": {"index"}}); err == nil {
		t.Fatal("an unsolved challenge was returned as a balance document")
	}
}

func TestSolverBaseOnlyAcceptsLoopback(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:8191", "http://localhost:8191/", "http://[::1]:8191"} {
		if solverBase(raw) == "" {
			t.Fatal("rejected loopback:", raw)
		}
	}
	for _, raw := range []string{"", "https://solver.example", "http://192.168.1.5:8191", "ftp://127.0.0.1:8191", "http://127.0.0.1:8191/?x=1", "http://user:pw@127.0.0.1:8191"} {
		if got := solverBase(raw); got != "" {
			t.Fatalf("accepted %q as %q", raw, got)
		}
	}
}
