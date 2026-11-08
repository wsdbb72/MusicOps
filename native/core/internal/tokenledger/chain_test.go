package tokenledger

import (
	"sync"
	"testing"
	"time"
)

func TestStaleReadDoesNotClearANewerBalance(t *testing.T) {
	ledger := openLedger(t)
	now := time.Unix(1_700_000_100, 0)
	current := Observation{Site: "dicmusic", AccountID: "7", CookieID: "cookie-a", Balance: 4, Known: true, Source: "userstats.tokens", ObservedAtNS: now.UnixNano(), Generation: 2}
	if _, err := ledger.CommitObservation(current); err != nil {
		t.Fatal(err)
	}
	late := Observation{Site: "dicmusic", AccountID: "7", CookieID: "cookie-a", Known: false, ErrorClass: "network", ReadBase: 0, ReadBaseSet: true}
	got, err := ledger.CommitObservation(late)
	if err != nil || !got.Known || got.Balance != 4 || got.Generation != 2 {
		t.Fatal(got, err)
	}
	fresh := Observation{Site: "dicmusic", AccountID: "7", CookieID: "cookie-a", Balance: 3, Known: true, Source: "userstats.tokens", ObservedAtNS: now.Add(time.Second).UnixNano(), ReadBase: 2, ReadBaseSet: true}
	got, err = ledger.CommitObservation(fresh)
	if err != nil || got.Balance != 3 || got.Generation != 3 {
		t.Fatal(got, err)
	}
}

func TestCookieBindsOneAccountAndADifferentCookieCannotSpend(t *testing.T) {
	ledger := openLedger(t)
	now := time.Unix(1_700_000_100, 0)
	first := Observation{Site: "orpheus", AccountID: "8", CookieID: "cookie-a", Balance: 2, Known: true, Source: "stats.tokens", ObservedAtNS: now.UnixNano(), Generation: 1}
	if _, err := ledger.CommitObservation(first); err != nil {
		t.Fatal(err)
	}
	other := first
	other.AccountID = "9"
	other.Balance = 99
	other.Generation = 1
	rejected, err := ledger.CommitObservation(other)
	if err != nil || rejected.Known || rejected.ErrorClass != "binding" {
		t.Fatal(rejected, err)
	}
	stored, found, err := ledger.Load("orpheus", "8")
	if err != nil || !found || stored.Balance != 2 {
		t.Fatal(stored, found, err)
	}
	if _, found, err = ledger.Load("orpheus", "9"); err != nil || found {
		t.Fatal(found, err)
	}
	quote := quote("orpheus", "11", "request-1", 1, 99, 99, 0)
	quote.AccountID = "8"
	quote.CookieID = "cookie-b"
	mismatch, err := ledger.ReserveObserved(now, quote, "8")
	if err != nil || mismatch.Class != ClassBalanceUnknown || mismatch.Spent {
		t.Fatal(mismatch, err)
	}
	quote.CookieID = "cookie-a"
	quote.RequestID = "request-2"
	spent, err := ledger.ReserveObserved(now, quote, "8")
	if err != nil || !spent.Spent || spent.Cost != 1 {
		t.Fatal(spent, err)
	}
}

func TestConfirmedSpendDoesNotRepeatAndAWrongRecordDoesNotRelease(t *testing.T) {
	ledger := openLedger(t)
	now := time.Unix(1_700_000_100, 0)
	obs := Observation{Site: "dicmusic", AccountID: "7", Balance: 2, Known: true, Source: "userstats.tokens", ObservedAtNS: now.UnixNano(), Generation: 1}
	if _, err := ledger.CommitObservation(obs); err != nil {
		t.Fatal(err)
	}
	first := quote("dicmusic", "11", "request-1", 1, 99, 2, 0)
	first.AccountID = "7"
	if _, err := ledger.ReserveObserved(now, first, "7"); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Advance(now.Add(time.Second), "dicmusic", "request-1", StateRequestUnknown, "response-lost"); err != nil {
		t.Fatal(err)
	}
	wrong, class := ParseSpend([]byte(`{"contract":"musicops.fixture.v1","state":"CONSUMED","account_id":"7","resource_id":"22","request_id":"request-1"}`))
	if class != "" {
		t.Fatal(class)
	}
	unchanged, err := ledger.Reconcile(now.Add(2*time.Second), "dicmusic", wrong)
	if err != nil || unchanged.Class != ClassUncertain {
		t.Fatal(unchanged, err)
	}
	for _, body := range []string{
		`d8:announce12:http://a.com4:infod6:lengthi1eee`,
		`{"status":"success","response":{"id":7,"userstats":{"tokens":1}}}`,
		`{"contract":"gazelle-balance.v1","state":"CONSUMED","account_id":"7","resource_id":"11","request_id":"request-1"}`,
	} {
		if _, class = ParseSpend([]byte(body)); class == "" {
			t.Fatal(body)
		}
	}
	proof, class := ParseSpend([]byte(`{"contract":"musicops.fixture.v1","state":"CONSUMED","account_id":"7","resource_id":"11","request_id":"request-1"}`))
	if class != "" || proof.ProductionVerified {
		t.Fatal(proof, class)
	}
	confirmed, err := ledger.Reconcile(now.Add(3*time.Second), "dicmusic", proof)
	if err != nil || confirmed.State != StateConfirmed || confirmed.Downloaded || confirmed.ProductionVerified {
		t.Fatal(confirmed, err)
	}
	again, err := ledger.Reconcile(now.Add(4*time.Second), "dicmusic", proof)
	if err != nil || again.State != StateConfirmed {
		t.Fatal(again, err)
	}
	opposite, class := ParseSpend([]byte(`{"contract":"musicops.fixture.v1","state":"NOT_CONSUMED","account_id":"7","resource_id":"11","request_id":"request-1"}`))
	if class != "" {
		t.Fatal(class)
	}
	kept, err := ledger.Reconcile(now.Add(5*time.Second), "dicmusic", opposite)
	if err != nil || kept.Class != ClassUncertain || kept.State != StateConfirmed {
		t.Fatal(kept, err)
	}
	repeat := quote("dicmusic", "11", "request-2", 1, 99, 2, 0)
	repeat.AccountID = "7"
	blocked, err := ledger.ReserveObserved(now.Add(6*time.Second), repeat, "7")
	if err != nil || blocked.Class != ClassSpent || blocked.Scope != "resource" || blocked.Spent {
		t.Fatal(blocked, err)
	}
	other := quote("dicmusic", "22", "request-3", 1, 99, 2, 0)
	other.AccountID = "7"
	allowed, err := ledger.ReserveObserved(now.Add(6*time.Second), other, "7")
	if err != nil || !allowed.Spent || allowed.Cost != 1 {
		t.Fatal(allowed, err)
	}
}

func TestUnknownReservationSurvivesRestartAndDoesNotBlockAnotherSite(t *testing.T) {
	dir := t.TempDir()
	engine := openAt(t, dir)
	ledger := New(engine)
	now := time.Unix(1_700_000_100, 0)
	obs := Observation{Site: "dicmusic", AccountID: "7", Balance: 4, Known: true, Source: "userstats.tokens", ObservedAtNS: now.UnixNano(), Generation: 1}
	if _, err := ledger.CommitObservation(obs); err != nil {
		t.Fatal(err)
	}
	pending := quote("dicmusic", "11", "request-1", 1, 4, 4, 0)
	pending.AccountID = "7"
	if _, err := ledger.ReserveObserved(now, pending, "7"); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Advance(now.Add(time.Second), "dicmusic", "request-1", StateRequestUnknown, "response-lost"); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openAt(t, dir)
	t.Cleanup(func() { reopened.Close() })
	ledger = New(reopened)
	stored, found, err := ledger.Get("dicmusic", "request-1")
	if err != nil || !found || stored.State != StateRequestUnknown || stored.Downloaded {
		t.Fatal(stored, found, err)
	}
	again := quote("dicmusic", "11", "request-2", 1, 99, 4, 0)
	again.AccountID = "7"
	blocked, err := ledger.ReserveObserved(now.Add(2*time.Second), again, "7")
	if err != nil || blocked.Class != ClassPending || blocked.Scope != "resource" || blocked.Spent {
		t.Fatal(blocked, err)
	}
	other := quote("orpheus", "31", "request-9", 1, 3, 3, 0)
	got, err := ledger.Reserve(now.Add(2*time.Second), other)
	if err != nil || !got.Spent || got.Class != ClassReserved {
		t.Fatal(got, err)
	}
}

func TestNotConsumedReleasesTheResourceForOneNewReservation(t *testing.T) {
	ledger := openLedger(t)
	now := time.Unix(1_700_000_100, 0)
	obs := Observation{Site: "orpheus", AccountID: "8", Balance: 1, Known: true, Source: "userstats.tokens", ObservedAtNS: now.UnixNano(), Generation: 1}
	if _, err := ledger.CommitObservation(obs); err != nil {
		t.Fatal(err)
	}
	first := quote("orpheus", "11", "request-1", 1, 9, 1, 0)
	first.AccountID = "8"
	if _, err := ledger.ReserveObserved(now, first, "8"); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Advance(now.Add(time.Second), "orpheus", "request-1", StateRequestUnknown, "response-lost"); err != nil {
		t.Fatal(err)
	}
	proof, class := ParseSpend([]byte(`{"contract":"musicops.fixture.v1","state":"NOT_CONSUMED","account_id":"8","resource_id":"11","request_id":"request-1"}`))
	if class != "" {
		t.Fatal(class)
	}
	released, err := ledger.Reconcile(now.Add(2*time.Second), "orpheus", proof)
	if err != nil || released.State != StateReleased || released.Spent || released.ProductionVerified {
		t.Fatal(released, err)
	}
	next := quote("orpheus", "11", "request-2", 1, 9, 1, 0)
	next.AccountID = "8"
	got, err := ledger.ReserveObserved(now.Add(3*time.Second), next, "8")
	if err != nil || !got.Spent || got.Cost != 1 {
		t.Fatal(got, err)
	}
}

func TestConcurrentBalanceReadsKeepOneObservation(t *testing.T) {
	ledger := openLedger(t)
	var wg sync.WaitGroup
	results := make([]Observation, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = ledger.CommitObservation(Observation{
				Site: "dicmusic", AccountID: "7", CookieID: "cookie-a", Balance: 4 + i, Known: true,
				Source: "userstats.tokens", ObservedAtNS: int64(i + 1), ReadBaseSet: true,
			})
		}(i)
	}
	wg.Wait()
	stored, found, err := ledger.Load("dicmusic", "7")
	if err != nil || !found || stored.Generation != 1 || (stored.Balance != 4 && stored.Balance != 5) {
		t.Fatal(stored, found, err)
	}
	for i := range results {
		if errs[i] != nil || results[i].Balance != stored.Balance || results[i].Generation != stored.Generation {
			t.Fatal(i, results[i], errs[i], stored)
		}
	}
}

func TestRateLimitHoldExpiresWithoutBlockingAnotherSite(t *testing.T) {
	ledger := openLedger(t)
	now := time.Unix(1_700_000_100, 0)
	if err := ledger.HoldRead("dicmusic", "rate_limited", now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	allowed, class, until := ledger.ReadAllowed("dicmusic", now.Add(10*time.Second))
	if allowed || class != "rate_limited" || until == 0 {
		t.Fatal(allowed, class, until)
	}
	allowed, class, _ = ledger.ReadAllowed("orpheus", now.Add(10*time.Second))
	if !allowed || class != "" {
		t.Fatal(allowed, class)
	}
	allowed, _, _ = ledger.ReadAllowed("dicmusic", now.Add(31*time.Second))
	if !allowed {
		t.Fatal(allowed)
	}
	if err := ledger.ClearReadHold("dicmusic"); err != nil {
		t.Fatal(err)
	}
	allowed, class, _ = ledger.ReadAllowed("dicmusic", now.Add(10*time.Second))
	if !allowed || class != "" {
		t.Fatal(allowed, class)
	}
}
