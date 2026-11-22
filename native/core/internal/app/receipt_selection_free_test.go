package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/torrentfetch"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestTorrentReceiptSelectsNewestEvidence(t *testing.T) {
	application, _, processID := seedProcessReviewJob(t, false, "candidate", "detail")
	base := time.Now().UnixNano()

	block := func(label string, at int64, receipt torrentfetch.Receipt) {
		t.Helper()
		if _, err := application.flows.Review("retry-"+label, processID, workflow.ReviewDecision{Action: "retry"}, at); err != nil {
			t.Fatal(err)
		}
		token := claimAndBeginWorkflowTask(t, application, processID+"-"+label, "aux", at+1)
		payload, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := application.flows.Complete("block-"+label, token, workflow.Complete{
			JobID: processID, Edge: workflow.EdgeTerminal, NextStage: "needs_review", ReviewKind: "candidate",
			TaskState: "blocked", ErrorClass: "detail", NowNS: at + 2,
			Evidence: []workflow.EvidenceInput{{ID: "receipt-" + label, Kind: "api", Source: "dicmusic", Payload: payload}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	block("old", base+100, torrentfetch.Receipt{
		Site: "dicmusic", ResourceID: "11", MetaSHA256: strings.Repeat("a", 64), Path: "torrents/old.torrent", FetchedAtNS: 100,
	})
	block("new", base+200, torrentfetch.Receipt{
		Site: "dicmusic", ResourceID: "22", MetaSHA256: strings.Repeat("b", 64), Path: "torrents/new.torrent", FetchedAtNS: 200,
	})

	receipt, err := application.torrentReceipt(processID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ResourceID != "22" || receipt.Path != "torrents/new.torrent" || receipt.FetchedAtNS != 200 {
		t.Fatalf("selected stale receipt: %+v", receipt)
	}
}

func TestPlaylistStrictFLReceiptsAllowPersonalAndGlobalFree(t *testing.T) {
	for _, test := range []struct {
		name    string
		details string
		page    string
		want    string
	}{
		{name: "personal", details: `{"status":"success","response":{"group":{"id":10},"torrent":{"id":"11","isPersonalFreeleech":true}}}`, page: `<div>没有普通下载</div>`, want: "personal"},
		{name: "global", details: `{"status":"success","response":{"group":{"id":10},"torrent":{"id":"11","isPersonalFreeleech":false,"isFreeleech":true}}}`, page: `<div>普通下载</div>`, want: "global"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				values := r.URL.Query()
				switch {
				case values.Get("action") == "torrent":
					io.WriteString(w, test.details)
				case values.Get("torrentid") == "11":
					io.WriteString(w, test.page)
				case values.Get("action") == "download" || strings.HasPrefix(r.URL.Path, "/download.php"):
					io.WriteString(w, sampleTorrent)
				default:
					io.WriteString(w, gazelleHit("11"))
				}
			}))
			defer server.Close()

			application := newApp(t)
			jobID := "album-playlist-receipt-" + test.name
			body := tokenBody(jobID, server.URL, []string{"dicmusic"})
			body["batch_id"] = "pl-netease-receipt-" + test.name
			body["token_cost"] = 1
			body["dicmusic_authkey"] = "sentinel-dic-auth"
			post(t, application, "/v1/search", body)
			job := waitJob(t, application, "process:search:"+jobID, "needs_review")
			if job.ReviewKind != "candidate" || job.ErrorClass != "" {
				t.Fatalf("job=%+v", job)
			}

			receipt, err := application.torrentReceipt(job.ID, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if receipt.UseToken || receipt.FreeMethod != test.want || receipt.FetchedAtNS <= 0 {
				t.Fatalf("receipt=%+v", receipt)
			}
			plan, err := application.buildDownloadPlan(job, "", "musicops", "MusicOps")
			if err != nil {
				t.Fatal(err)
			}
			if plan.FreeMethod != test.want || plan.ResourceID != "11" || !plan.FreeConfirmed {
				t.Fatalf("plan=%+v", plan)
			}
		})
	}
}

func TestLegacyStrictFLNoTokenReceiptRemainsCompatible(t *testing.T) {
	legacyFree := torrentfetch.Receipt{Site: "dicmusic", ResourceID: "11", UseToken: false}
	if !receiptSiteFree(legacyFree) {
		t.Fatal("a legacy no-token receipt from the strict-free acquisition path must remain usable")
	}
	if receiptSiteFree(torrentfetch.Receipt{Site: "dicmusic", UseToken: true}) {
		t.Fatal("a token receipt must still require ledger settlement")
	}
	if receiptSiteFree(torrentfetch.Receipt{Site: "dicmusic", FreeMethod: "unverified"}) {
		t.Fatal("an unknown free method must not authorize a strict-FL download")
	}
}
