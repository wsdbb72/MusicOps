package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/reviewmatch"
	"github.com/wsdbb72/MusicOps/native/core/internal/tokenledger"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestAcquireCandidatesKeepSelectedFirstThenRankedSameRelease(t *testing.T) {
	openHit := reviewmatch.Hit{Title: "Artist - Album", Artists: []string{"Artist"}, Format: "FLAC"}
	hit := reviewmatch.Hit{Title: "Album", Artists: []string{"Artist"}, Format: "FLAC"}
	other := reviewmatch.Hit{Title: "Album", Artists: []string{"Different Artist"}, Format: "FLAC"}
	picks := []identityPick{
		{site: "dicmusic", id: "dicmusic:9", hit: hit, quality: candidateQualityRank{Lossless: 1, BitDepth: 24, SampleRate: 96000, Seeders: 4}, qualityLabel: "FLAC 24-bit/96 kHz"},
		{site: "open.cd", id: "open.cd:2", hit: openHit, quality: candidateQualityRank{Lossless: 1, BitDepth: 16, SampleRate: 44100, Seeders: 40}, qualityLabel: "FLAC 16-bit/44.1 kHz"},
		{site: "orpheus", id: "orpheus:3", hit: other, quality: candidateQualityRank{Lossless: 1, BitDepth: 24, SampleRate: 192000, Seeders: 50}, qualityLabel: "FLAC 24-bit/192 kHz"},
	}
	got := acquireCandidatesForSelection(SearchPlan{ExpectedTitle: "Album", ExpectedArtist: "Artist"}, picks, "open.cd", "open.cd:2")
	if len(got) != 2 || got[0].CandidateID != "open.cd:2" || got[1].CandidateID != "dicmusic:9" {
		t.Fatalf("ordered candidates = %+v", got)
	}
	plan := SearchPlan{SelectedSite: "open.cd", SelectedCandidateID: "open.cd:2", AcquireCandidates: got}
	again := acquireCandidateOrder(plan)
	if len(again) != 2 || again[0].CandidateID != "open.cd:2" || again[1].CandidateID != "dicmusic:9" {
		t.Fatalf("persisted order = %+v", again)
	}
}

func TestAcquireTriesNextPersistedCandidateBeforeSourceFallback(t *testing.T) {
	var downloads atomic.Int32
	var lastResource atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		switch {
		case values.Get("action") == "torrent":
			id := values.Get("id")
			personal := id == "22"
			io.WriteString(w, `{"status":"success","response":{"group":{"id":10},"torrent":{"id":"`+id+`","isPersonalFreeleech":`+strconv.FormatBool(personal)+`,"isFreeleech":false}}}`)
		case values.Get("torrentid") != "":
			if values.Get("torrentid") == "22" {
				io.WriteString(w, `<span>Personal free</span>`)
			} else {
				io.WriteString(w, `<span>No freeleech token</span>`)
			}
		case values.Get("action") == "download":
			downloads.Add(1)
			lastResource.Store(values.Get("id"))
			io.WriteString(w, sampleTorrent)
		case values.Get("action") == "browse":
			io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"Example &amp; Test","artist":"Artist","torrents":[{"torrentId":11,"format":"FLAC","encoding":"24bit Lossless","seeders":3},{"torrentId":22,"format":"FLAC","encoding":"Lossless","seeders":50}]}]}}`)
		default:
			io.WriteString(w, `{"status":"success","response":{}}`)
		}
	}))
	defer server.Close()
	application := newApp(t)
	body := tokenBody("album-ranked-fallback", server.URL, []string{"dicmusic"})
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-ranked-fallback", "needs_review")
	task, err := application.tasks.Get(job.ActiveTaskID)
	if err != nil || task == nil {
		t.Fatal(err, task)
	}
	raw, found, err := application.tasks.Data(task.ID, "arguments", 0)
	if err != nil || !found {
		t.Fatal(err, found)
	}
	var plan SearchPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	if job.ReviewKind != "candidate" || downloads.Load() != 1 || lastResource.Load() != "22" {
		t.Fatalf("job=%+v downloads=%d resource=%v", job, downloads.Load(), lastResource.Load())
	}
	if len(plan.AcquireCandidates) != 2 || plan.AcquireCandidates[0].CandidateID != "dicmusic:11" || plan.AcquireCandidates[1].CandidateID != "dicmusic:22" {
		t.Fatalf("persisted candidates = %+v", plan.AcquireCandidates)
	}
}

func TestSameSiteNextCandidateUsesIndependentTokenLedgerIdentity(t *testing.T) {
	var downloads atomic.Int32
	var downloaded atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		switch {
		case values.Get("action") == "index":
			io.WriteString(w, `{"status":"success","response":{"id":7,"userstats":{"tokens":10}}}`)
		case values.Get("action") == "torrent":
			id := values.Get("id")
			io.WriteString(w, `{"status":"success","response":{"group":{"id":10},"torrent":{"id":"`+id+`","isPersonalFreeleech":false,"isFreeleech":false}}}`)
		case values.Get("torrentid") != "":
			id := values.Get("torrentid")
			io.WriteString(w, `<a href="torrents.php?action=download&amp;id=`+id+`&amp;usetoken=1">FL</a>`)
		case values.Get("action") == "browse":
			io.WriteString(w, `{"status":"success","response":{"currentPage":1,"pages":1,"results":[{"groupId":10,"groupName":"Example &amp; Test","artist":"Artist","torrents":[{"torrentId":11,"format":"FLAC","encoding":"24bit Lossless","seeders":3},{"torrentId":22,"format":"FLAC","encoding":"Lossless","seeders":2}]}]}}`)
		case values.Get("action") == "download":
			downloads.Add(1)
			downloaded.Store(values.Get("id"))
			io.WriteString(w, sampleTorrent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	application := newApp(t)
	now := application.now()
	seed, err := application.ledger.Reserve(now, tokenledger.Quote{
		Site: "dicmusic", AccountID: "7", CookieID: "seed-cookie", ResourceID: "11", RequestID: "seed-confirmed-11", JobID: "seed-job-11",
		Cost: 1, BalanceKnown: true, Balance: 10, Budget: 10, ObservedAt: now, BudgetRef: "seed-batch",
		Scene: "expand", IdentityConfirmed: true, StreamingEnabled: true,
	})
	if err != nil || seed.State != tokenledger.StateReserved {
		t.Fatalf("seed reservation=%+v err=%v", seed, err)
	}
	if _, err := application.ledger.Advance(now, "dicmusic", "seed-confirmed-11", tokenledger.StateConfirmed, "seed-confirmed"); err != nil {
		t.Fatal(err)
	}

	body := tokenBody("album-ledger-candidates", server.URL, []string{"dicmusic"})
	body["token_cost"] = 1
	body["dicmusic_authkey"] = "sentinel-dic-auth"
	post(t, application, "/v1/search", body)
	job := waitJob(t, application, "process:search:album-ledger-candidates", "needs_review")
	if downloads.Load() != 1 || downloaded.Load() != "22" || job.ErrorClass != "TOKEN_PENDING_RECONCILE" {
		t.Fatalf("downloads=%d id=%v class=%s message=%s", downloads.Load(), downloaded.Load(), job.ErrorClass, job.Message)
	}
	receipt, err := application.torrentReceipt(job.ID, "", "")
	if err != nil || receipt.ResourceID != "22" || receipt.TokenRequestID == "" || receipt.TokenRequestID == job.ID {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	reservation, found, err := application.ledger.Get("dicmusic", receipt.TokenRequestID)
	if err != nil || !found || reservation.State != tokenledger.StateRequestUnknown {
		t.Fatalf("reservation=%+v found=%v err=%v", reservation, found, err)
	}
	planForDownload, err := application.buildDownloadPlan(job, t.TempDir(), "", "")
	if err != nil || planForDownload.ResourceID != "22" || planForDownload.FreeMethod != "token-provisional" {
		t.Fatalf("download plan=%+v err=%v", planForDownload, err)
	}

	inspection := inspect(t, application, job.ID)
	raw, found, err := application.tasks.Data(inspection.Task.ID, "arguments", 0)
	if err != nil || !found {
		t.Fatal(err, found)
	}
	var plan SearchPlan
	if err := json.Unmarshal(raw, &plan); err != nil || len(plan.AcquireCandidates) != 2 {
		t.Fatalf("plan candidates=%+v err=%v", plan.AcquireCandidates, err)
	}
	if firstID := acquireTokenRequestID(job, plan.AcquireCandidates, plan.AcquireCandidates[0]); firstID == receipt.TokenRequestID {
		t.Fatalf("candidates reused ledger id %q", firstID)
	}
}

func TestReleasedCandidateGetsFreshTokenKeyAfterReviewRetry(t *testing.T) {
	application := newApp(t)
	now := application.now()
	job := workflow.Job{ID: "process:search:released-candidate-retry", Revision: 4}
	candidate := AcquireCandidate{Site: "dicmusic", CandidateID: "dicmusic:22"}
	firstKey := acquireTokenRequestID(job, []AcquireCandidate{candidate}, candidate)
	quote := candidateTokenQuote(job, firstKey, "22", now)
	result, err := application.ledger.Reserve(now, quote)
	if err != nil || result.State != tokenledger.StateReserved {
		t.Fatalf("first reservation=%+v err=%v", result, err)
	}
	if _, err := application.ledger.Advance(now, "dicmusic", firstKey, tokenledger.StateReleased, "read-hold-without-request"); err != nil {
		t.Fatal(err)
	}

	job.Revision++
	job.TokenRetryCount++
	secondKey := acquireTokenRequestID(job, []AcquireCandidate{candidate}, candidate)
	if secondKey == firstKey {
		t.Fatal("a reviewed retry reused the released request key")
	}
	second := candidateTokenQuote(job, secondKey, "22", now.Add(time.Second))
	result, err = application.ledger.Reserve(now.Add(time.Second), second)
	if err != nil || result.State != tokenledger.StateReserved {
		t.Fatalf("released candidate could not reserve again: result=%+v err=%v", result, err)
	}
}

func TestCandidateKeyRotationCannotReplayUnknownOrConsumedSpend(t *testing.T) {
	for _, state := range []string{tokenledger.StateRequestUnknown, tokenledger.StateConsumed} {
		t.Run(state, func(t *testing.T) {
			application := newApp(t)
			now := application.now()
			job := workflow.Job{ID: "process:search:unsafe-candidate-retry", Revision: 7}
			candidate := AcquireCandidate{Site: "dicmusic", CandidateID: "dicmusic:22"}
			firstKey := acquireTokenRequestID(job, []AcquireCandidate{candidate}, candidate)
			if _, err := application.ledger.Reserve(now, candidateTokenQuote(job, firstKey, "22", now)); err != nil {
				t.Fatal(err)
			}
			if _, err := application.ledger.Advance(now, "dicmusic", firstKey, state, "test state"); err != nil {
				t.Fatal(err)
			}

			job.Revision++
			job.TokenRetryCount++
			secondKey := acquireTokenRequestID(job, []AcquireCandidate{candidate}, candidate)
			if secondKey == firstKey {
				t.Fatal("expected a distinct candidate key after revision change")
			}
			result, err := application.ledger.Reserve(now.Add(time.Second), candidateTokenQuote(job, secondKey, "22", now.Add(time.Second)))
			if err != nil {
				t.Fatal(err)
			}
			if result.State == tokenledger.StateReserved {
				t.Fatalf("%s allowed a second reservation: %+v", state, result)
			}
			wantClass := tokenledger.ClassPending
			if state == tokenledger.StateConsumed {
				wantClass = tokenledger.ClassSpent
			}
			if result.Class != wantClass {
				t.Fatalf("class=%s want=%s result=%+v", result.Class, wantClass, result)
			}
		})
	}
}

func candidateTokenQuote(job workflow.Job, requestID, resource string, observedAt time.Time) tokenledger.Quote {
	budgetRef := job.BatchID
	if budgetRef == "" {
		budgetRef = job.ID
	}
	scene := job.Scene
	if scene == "" {
		scene = "expand"
	}
	return tokenledger.Quote{
		Site: "dicmusic", AccountID: "7", CookieID: "candidate-cookie", ResourceID: resource,
		RequestID: requestID, JobID: job.ID, Cost: 1, BalanceKnown: true, Balance: 10,
		Budget: 10, SafetyMargin: 0, ObservedAt: observedAt, BudgetRef: budgetRef,
		Scene: scene, IdentityConfirmed: true, StreamingEnabled: true,
	}
}

func TestReleasedCandidateReviewRetryActuallyDownloadsSameCandidate(t *testing.T) {
	vaultKey(t)
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if noteTokenEvidence(w, r) {
			return
		}
		switch r.URL.Query().Get("action") {
		case "index":
			io.WriteString(w, `{"status":"success","response":{"id":7,"userstats":{"tokens":4}}}`)
		case "download":
			downloads.Add(1)
			io.WriteString(w, sampleTorrent)
		default:
			io.WriteString(w, gazelleHit("11"))
		}
	}))
	defer server.Close()

	application, rootID, processID := seedTokenReviewJobWithPlan(t, false, "rate_limited", func(plan *SearchPlan) {
		plan.Synthetic = true
		plan.BaseURL = server.URL
		plan.Budget = 10
	})
	saveTestCookie(t, application, "dicmusic")
	application.rememberDownload(rootID, "dicmusic", "1234567890abcdef")
	job, err := application.flows.Job(processID)
	if err != nil || job == nil {
		t.Fatal(job, err)
	}
	candidate := AcquireCandidate{Site: "dicmusic", CandidateID: "dicmusic:11"}
	releasedKey := acquireTokenRequestID(*job, []AcquireCandidate{candidate}, candidate)
	if _, err := application.ledger.Reserve(application.now(), candidateTokenQuote(*job, releasedKey, "11", application.now())); err != nil {
		t.Fatal(err)
	}
	if _, err := application.ledger.Advance(application.now(), "dicmusic", releasedKey, tokenledger.StateReleased, "read-hold-without-request"); err != nil {
		t.Fatal(err)
	}

	recorder := postCode(t, application, "/v1/jobs/"+processID+"/review", map[string]any{"action": "ai"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("review code=%d body=%s", recorder.Code, recorder.Body.String())
	}
	application.mu.Lock()
	application.paused = false
	application.mu.Unlock()

	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		current, inspectErr := application.flows.Job(processID)
		if inspectErr == nil && current != nil && downloads.Load() == 1 &&
			current.Stage == "needs_review" && current.ErrorClass == "TOKEN_PENDING_RECONCILE" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if downloads.Load() != 1 {
		current, _ := application.flows.Job(processID)
		inspection, _ := application.flows.Inspect(processID)
		task, taskErr := application.tasks.Get(current.ActiveTaskID)
		t.Fatalf("released candidate retry did not download again: job=%+v task=%+v taskErr=%v fields=%+v", current, task, taskErr, inspection.Task)
	}
	receipt, err := application.torrentReceipt(processID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ResourceID != "11" || receipt.TokenRequestID == "" || receipt.TokenRequestID == releasedKey {
		t.Fatalf("receipt=%+v releasedKey=%s", receipt, releasedKey)
	}
	reservation, found, err := application.ledger.Get("dicmusic", receipt.TokenRequestID)
	if err != nil || !found || reservation.State != tokenledger.StateRequestUnknown {
		t.Fatalf("reservation=%+v found=%v err=%v", reservation, found, err)
	}
}
