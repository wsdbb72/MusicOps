package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/tokenledger"
)

// balanceDoer answers every tracker read with one canned account document, so
// the reconciler can be tested without a network or a site.
type balanceDoer struct {
	body   string
	status int
	reads  int
}

func (d *balanceDoer) Do(req *http.Request) (*http.Response, error) {
	d.reads++
	status := d.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewBufferString(d.body)),
		Request:    req,
	}, nil
}

func reserveForReconcile(t *testing.T, application *App, balance int, observedAt time.Time) string {
	t.Helper()
	const requestID = "job-reconcile-1"
	quote := tokenledger.Quote{
		Site: "dicmusic", AccountID: "7", CookieID: "cookie-7", ResourceID: "11",
		RequestID: requestID, Cost: 1, BalanceKnown: true, Balance: balance,
		Budget: 10, SafetyMargin: 0, ObservedAt: observedAt, BudgetRef: "batch-reconcile",
		Scene: "playlist", IdentityConfirmed: true, StreamingEnabled: true,
	}
	if _, err := application.ledger.Reserve(observedAt, quote); err != nil {
		t.Fatal(err)
	}
	if _, err := application.ledger.Advance(observedAt, "dicmusic", requestID, tokenledger.StateRequestUnknown, "测试：请求已发出"); err != nil {
		t.Fatal(err)
	}
	return requestID
}

func saveTestCookie(t *testing.T, application *App, site string) {
	t.Helper()
	recorder := postCode(t, application, "/v1/settings/cookies", map[string]any{
		"site":        site,
		"cookie_json": `[{"name":"session","value":"reconcile-cookie","domain":"dicmusic.com","path":"/","secure":false,"httpOnly":false}]`,
	})
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}

// A balance lower than the reservation saw is the site saying it charged the
// token: the row is settled as CONFIRMED without waiting for the deadline.
func TestReconcileTokensConfirmsWhenTheBalanceDrops(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	saveTestCookie(t, application, "dicmusic")
	doer := &balanceDoer{body: `{"status":"success","response":{"id":7,"userstats":{"tokens":4}}}`}
	application.searcher.Doer = doer

	start := application.now()
	requestID := reserveForReconcile(t, application, 5, start)
	moved := application.reconcileTokens(context.Background(), start.Add(2*time.Minute))
	if moved != 1 {
		t.Fatal("the reconciler did not settle the row:", moved)
	}
	row, found, err := application.ledger.Get("dicmusic", requestID)
	if err != nil || !found || row.State != tokenledger.StateConfirmed {
		t.Fatal(row, found, err)
	}
	if doer.reads == 0 {
		t.Fatal("the balance was never read")
	}
}

// The same balance after the release window means the request never cost
// anything: RELEASED, and the site is free again without the 30-minute wait.
func TestReconcileTokensReleasesAfterTheWindow(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	saveTestCookie(t, application, "dicmusic")
	application.searcher.Doer = &balanceDoer{body: `{"status":"success","response":{"id":7,"userstats":{"tokens":5}}}`}

	start := application.now().Add(-10 * time.Minute)
	requestID := reserveForReconcile(t, application, 5, start)
	moved := application.reconcileTokens(context.Background(), application.now())
	if moved != 1 {
		t.Fatal("the reconciler did not settle the row:", moved)
	}
	row, found, err := application.ledger.Get("dicmusic", requestID)
	if err != nil || !found || row.State != tokenledger.StateReleased {
		t.Fatal(row, found, err)
	}
	if _, err := application.ledger.Reserve(application.now(), tokenledger.Quote{
		Site: "dicmusic", AccountID: "7", ResourceID: "12", RequestID: "job-reconcile-2",
		Cost: 1, BalanceKnown: true, Balance: 5, Budget: 10, SafetyMargin: 0,
		ObservedAt: application.now(), BudgetRef: "batch-reconcile",
		Scene: "playlist", IdentityConfirmed: true, StreamingEnabled: true,
	}); err != nil {
		t.Fatal("the site stayed held after the release:", err)
	}
}

// A row younger than the release window is left alone: one unchanged read is
// not two.
func TestReconcileTokensWaitsBeforeReleasing(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	saveTestCookie(t, application, "dicmusic")
	application.searcher.Doer = &balanceDoer{body: `{"status":"success","response":{"id":7,"userstats":{"tokens":5}}}`}

	start := application.now()
	requestID := reserveForReconcile(t, application, 5, start)
	if moved := application.reconcileTokens(context.Background(), start.Add(2*time.Minute)); moved != 0 {
		t.Fatal("a young row must not be released:", moved)
	}
	row, found, err := application.ledger.Get("dicmusic", requestID)
	if err != nil || !found || row.State != tokenledger.StateRequestUnknown {
		t.Fatal(row, found, err)
	}
}

// Without a saved cookie the sweep must not touch the site: the background
// path has no other credential, and guessing one is not an option.
func TestReconcileTokensSkipsSitesWithoutASavedCookie(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	doer := &balanceDoer{body: `{"status":"success","response":{"id":7,"userstats":{"tokens":4}}}`}
	application.searcher.Doer = doer
	start := application.now()
	reserveForReconcile(t, application, 5, start)
	if moved := application.reconcileTokens(context.Background(), start.Add(2*time.Minute)); moved != 0 {
		t.Fatal("a row was settled without a cookie:", moved)
	}
	if doer.reads != 0 {
		t.Fatal("the site was read without a cookie")
	}
}
