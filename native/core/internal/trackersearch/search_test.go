package trackersearch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

type recordDoer struct {
	urls []string
	next *http.Response
	err  error
}

func (d *recordDoer) Do(req *http.Request) (*http.Response, error) {
	d.urls = append(d.urls, req.URL.String())
	if strings.Contains(req.URL.Path, "download") {
		return nil, io.EOF
	}
	if d.err != nil {
		return nil, d.err
	}
	return d.next, nil
}

func jsonResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func TestGazelleHitStaysReadOnlyAndUsesTheOfficialOrigin(t *testing.T) {
	body := `{"status":"success","response":{"currentPage":1,"pages":2,"results":[{"groupId":10,"groupName":"Example &amp; Test","artist":"Artist","groupYear":2000,"torrents":[{"torrentId":11,"format":"FLAC","encoding":"Lossless","seeders":3,"isFreeleech":true,"canUseToken":true}]}]}}`
	doer := &recordDoer{next: jsonResponse(body)}
	out, err := (&Client{Doer: doer}).Search(context.Background(), Query{Site: "dicmusic", Text: "Artist", Cookie: "session=virtual"})
	if err != nil || out.Status != "OK" || out.SearchComplete || out.Downloaded || out.ProductionVerified || out.DownloadStatus != "POLICY_BLOCKED" {
		t.Fatal(out, err)
	}
	if len(out.Candidates) != 1 || out.Candidates[0].ResourceID != "11" || out.Candidates[0].Title != "Example & Test" || out.Candidates[0].Eligibility["state"] != "UNKNOWN" || out.Candidates[0].EstimatedCost != nil {
		t.Fatal(out.Candidates)
	}
	if len(doer.urls) != 1 || !strings.HasPrefix(doer.urls[0], "https://dicmusic.com/ajax.php?") || !strings.Contains(doer.urls[0], "action=browse") {
		t.Fatal(doer.urls)
	}
	if out.Candidates[0].EvidenceLevel != "live_readonly" || out.OfficialHost != true || out.Synthetic {
		t.Fatal(out)
	}
}

func TestEmptyGazelleShapeIsNoResource(t *testing.T) {
	doer := &recordDoer{next: jsonResponse(`{"status":"success","response":{"results":[],"pages":1}}`)}
	out, err := (&Client{Doer: doer}).Search(context.Background(), Query{Site: "orpheus", Text: "Missing", Cookie: "session=virtual"})
	if err != nil || out.Status != "NO_RESOURCE" || !out.SearchComplete || len(out.Candidates) != 0 {
		t.Fatal(out, err)
	}
}

func TestNonMusicRowsAreSkipped(t *testing.T) {
	body := `{"status":"success","response":{"pages":1,"results":[{"groupId":10,"groupName":"Album","artist":"Artist","torrents":[{"torrentId":11,"seeders":1}]},{"groupId":"22","torrentId":23,"category":"E-Books","groupName":"Book"}]}}`
	out, err := (&Client{Doer: &recordDoer{next: jsonResponse(body)}}).Search(context.Background(), Query{Site: "redacted", Text: "Album", Cookie: "session=virtual"})
	if err != nil || out.Status != "OK" || len(out.Candidates) != 1 {
		t.Fatal(out, err)
	}
}

func TestAuthAndContractFailuresAreNotEmptyResults(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{401, "", "AUTH_FAILED"},
		{200, "<html>login</html>", "ACCESS_BLOCKED"},
		{200, `{"status":"failure"}`, "API_CONTRACT_ERROR"},
		{200, `{"status":"success","response":{}}`, "PARSE_ERROR"},
	}
	for _, item := range cases {
		doer := &recordDoer{next: &http.Response{StatusCode: item.status, Body: io.NopCloser(strings.NewReader(item.body)), Header: http.Header{}}}
		out, err := (&Client{Doer: doer}).Search(context.Background(), Query{Site: "dicmusic", Text: "Artist", Cookie: "session=virtual"})
		if err != nil || out.Status != item.want || len(out.Candidates) != 0 || out.Downloaded {
			t.Fatal(item.want, out, err)
		}
	}
}

func TestPreflightOmitsSecretsAndDoesNotSpend(t *testing.T) {
	body := `{"status":"success","response":{"id":1,"username":"private-name","authkey":"private-key","passkey":"private-pass","userstats":{"tokens":12}}}`
	out, err := (&Client{Doer: &recordDoer{next: jsonResponse(body)}}).Preflight(context.Background(), Query{Site: "dicmusic", Cookie: "session=virtual"})
	if err != nil || !out.Authenticated || out.TokenBalance == nil || *out.TokenBalance != 12 || out.DownloadStatus != "POLICY_BLOCKED" {
		t.Fatal(out, err)
	}
	encoded, _ := json.Marshal(out)
	if strings.Contains(string(encoded), "private") {
		t.Fatal(string(encoded))
	}
}

func TestSyntheticLoopbackIsLabeledAndRejectsOtherHosts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ajax.php" || r.Header.Get("X-MusicOps-Site") != "dicmusic" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"status":"success","response":{"results":[],"pages":1}}`))
	}))
	defer server.Close()
	out, err := (&Client{}).Search(context.Background(), Query{Site: "dicmusic", Text: "Album", Cookie: "session=virtual", Synthetic: true, BaseURL: server.URL})
	if err != nil || out.Status != "NO_RESOURCE" || !out.Synthetic || out.OfficialHost || out.ProductionVerified {
		t.Fatal(out, err)
	}
	if _, err = (&Client{}).Search(context.Background(), Query{Site: "dicmusic", Text: "Album", Cookie: "session=virtual", Synthetic: true, BaseURL: "https://dicmusic.com"}); !errors.Is(err, dataengine.ErrInvalidRequest) {
		t.Fatal(err)
	}
	if _, err = (&Client{}).Search(context.Background(), Query{Site: "dicmusic", Text: "Album", Cookie: "session=virtual", Synthetic: true, BaseURL: "http://example.com:9"}); !errors.Is(err, dataengine.ErrInvalidRequest) {
		t.Fatal(err)
	}
}

func TestOpenCDRowAndMissingCookieDoesNotDial(t *testing.T) {
	raw := `<html><a href="logout.php">logout</a><table><tr><td><table><tr><td><a href="details.php?id=15&amp;hit=1" title="Album">Album</a></td></tr></table></td><td>100 MiB</td></tr></table></html>`
	doer := &recordDoer{next: &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(raw)), Header: http.Header{}}}
	out, err := (&Client{Doer: doer}).Search(context.Background(), Query{Site: "open.cd", Text: "Album", Cookie: "session=virtual"})
	if err != nil || out.Status != "OK" || out.Candidates[0].ResourceID != "15" || out.Candidates[0].Media["size_display"] != "100 MiB" {
		t.Fatal(out, err)
	}
	if !strings.HasPrefix(doer.urls[0], "https://www.open.cd/torrents.php?") {
		t.Fatal(doer.urls)
	}
	silent := &recordDoer{}
	missing, err := (&Client{Doer: silent}).Search(context.Background(), Query{Site: "open.cd", Text: "Album"})
	if err != nil || missing.Status != "AUTH_REQUIRED" || len(silent.urls) != 0 {
		t.Fatal(missing, err, silent.urls)
	}
}

func TestRateLimitIsRememberedWithoutAnotherRequest(t *testing.T) {
	doer := &recordDoer{next: &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"30"}}, Body: io.NopCloser(strings.NewReader(""))}}
	client := &Client{Doer: doer}
	first, err := client.Search(context.Background(), Query{Site: "dicmusic", Text: "Album", Cookie: "session=virtual"})
	second, err2 := client.Search(context.Background(), Query{Site: "dicmusic", Text: "Album", Cookie: "session=virtual"})
	if err != nil || err2 != nil || first.Status != "RATE_LIMITED" || second.Status != "RATE_LIMITED" || len(doer.urls) != 1 || second.RetryAfterSec < 1 {
		t.Fatal(first, second, err, doer.urls)
	}
}
