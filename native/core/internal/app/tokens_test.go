package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/tokenledger"
)

func tokenSpendServer(t *testing.T) *httptest.Server {
	t.Helper()
	torrent := []byte("d8:announce12:http://a.com4:infod6:lengthi1e4:name4:test12:piece lengthi16384e6:pieces20:01234567890123456789ee")
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if noteTokenEvidence(w, r) {
			return
		}
		if r.URL.Query().Get("action") == "index" {
			io.WriteString(w, `{"status":"success","response":{"id":7,"userstats":{"tokens":4}}}`)
			return
		}
		if r.URL.Query().Get("action") == "download" {
			w.Write(torrent)
			return
		}
		io.WriteString(w, gazelleHit("11"))
	}))
}

// runTokenSpend posts a token-cost search and waits for the process job to
// reach needs_review. The reservation it creates is (dicmusic, account 7,
// resource 11, request process:search:<jobID>).
func runTokenSpend(t *testing.T, application *App, server *httptest.Server, jobID string) {
	t.Helper()
	body := searchBody(jobID, server.URL, []string{"dicmusic"}, false)
	body["token_cost"] = 1
	body["balance_known"] = false
	body["balance"] = 0
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	waitJob(t, application, "process:search:"+jobID, "needs_review")
}

func writeSpendProof(t *testing.T, application *App, line string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(application.Dir, "spend-proofs.jsonl"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTokenFetchReconcilesFromProofFile(t *testing.T) {
	server := tokenSpendServer(t)
	defer server.Close()
	application := newApp(t)
	writeSpendProof(t, application, `{"contract":"musicops.fixture.v1","account_id":"7","resource_id":"11","request_id":"process:search:album-proof","state":"CONSUMED"}`)
	runTokenSpend(t, application, server, "album-proof")
	job := inspect(t, application, "process:search:album-proof")
	if job.Job.ReviewKind != "token" || job.Job.ErrorClass != "SPEND_CONFIRMED" || !strings.Contains(job.Job.Message, "已确认消耗") {
		t.Fatal(job.Job)
	}
	reservation, found, err := application.ledger.Get("dicmusic", "process:search:album-proof")
	if err != nil || !found || reservation.State != tokenledger.StateConfirmed {
		t.Fatal(reservation, found, err)
	}
}

func TestTokenFetchNotConsumedReleases(t *testing.T) {
	server := tokenSpendServer(t)
	defer server.Close()
	application := newApp(t)
	writeSpendProof(t, application, `{"contract":"musicops.fixture.v1","account_id":"7","resource_id":"11","request_id":"process:search:album-free-proof","state":"NOT_CONSUMED"}`)
	runTokenSpend(t, application, server, "album-free-proof")
	job := inspect(t, application, "process:search:album-free-proof")
	if job.Job.ReviewKind != "token" || job.Job.ErrorClass != "SPEND_NOT_CONSUMED" || !strings.Contains(job.Job.Message, "未消耗") {
		t.Fatal(job.Job)
	}
	reservation, found, err := application.ledger.Get("dicmusic", "process:search:album-free-proof")
	if err != nil || !found || reservation.State != tokenledger.StateReleased {
		t.Fatal(reservation, found, err)
	}
}

func TestTokenFetchIgnoresMismatchedProof(t *testing.T) {
	server := tokenSpendServer(t)
	defer server.Close()
	application := newApp(t)
	// Wrong account: the proof does not match the reservation, so the
	// reservation stays REQUEST_UNKNOWN exactly as without any proof.
	writeSpendProof(t, application, `{"contract":"musicops.fixture.v1","account_id":"8","resource_id":"11","request_id":"process:search:album-mismatch","state":"CONSUMED"}`)
	runTokenSpend(t, application, server, "album-mismatch")
	job := inspect(t, application, "process:search:album-mismatch")
	if job.Job.ReviewKind != "token" || job.Job.ErrorClass != "REQUEST_UNKNOWN" {
		t.Fatal(job.Job)
	}
}

func TestTokenRecheckConfirmsWithProof(t *testing.T) {
	server := tokenSpendServer(t)
	defer server.Close()
	application := newApp(t)
	runTokenSpend(t, application, server, "album-recheck")
	before := inspect(t, application, "process:search:album-recheck")
	if before.Job.ErrorClass != "REQUEST_UNKNOWN" {
		t.Fatal(before.Job)
	}
	posted := postCode(t, application, "/v1/tokens/recheck", map[string]any{
		"site": "dicmusic",
		"proof": map[string]any{
			"contract": "musicops.fixture.v1", "account_id": "7", "resource_id": "11",
			"request_id": "process:search:album-recheck", "state": "CONSUMED",
		},
	})
	if posted.Code != http.StatusOK || !strings.Contains(posted.Body.String(), `"state":"CONFIRMED"`) {
		t.Fatal(posted.Code, posted.Body.String())
	}
	reservation, found, err := application.ledger.Get("dicmusic", "process:search:album-recheck")
	if err != nil || !found || reservation.State != tokenledger.StateConfirmed {
		t.Fatal(reservation, found, err)
	}
	// A proof that does not parse changes nothing.
	rejected := postCode(t, application, "/v1/tokens/recheck", map[string]any{
		"site": "dicmusic",
		"proof": map[string]any{
			"contract": "something.else", "account_id": "7", "resource_id": "11",
			"request_id": "process:search:album-recheck", "state": "CONSUMED",
		},
	})
	if rejected.Code != http.StatusOK || !strings.Contains(rejected.Body.String(), "token_uncertain") {
		t.Fatal(rejected.Code, rejected.Body.String())
	}
}
