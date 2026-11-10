package tokenledger

import (
	"testing"
	"time"
)

func TestParseBalanceDistinguishesZeroUnknownAndMalformed(t *testing.T) {
	known := ParseBalance("dicmusic", []byte(`{"status":"success","response":{"id":7,"userstats":{"tokens":0}}}`))
	if !known.Known || known.Balance != 0 || known.Source != "userstats.tokens" || known.AccountID != "7" || known.ProductionVerified {
		t.Fatal(known)
	}
	missing := ParseBalance("orpheus", []byte(`{"status":"success","response":{"id":7,"username":"a"}}`))
	if missing.Known || missing.Balance != 0 || missing.ErrorClass != ClassBalanceUnknown {
		t.Fatal(missing)
	}
	for _, body := range []string{
		`{"status":"success","response":{"id":7,"userstats":{"tokens":-1}}}`,
		`{"status":"success","response":{"id":7,"userstats":{"tokens":1.5}}}`,
		`{"status":"success","response":{"id":7,"userstats":{"tokens":"1000001"}}}`,
		`{"status":"success","response":{"id":7,"personal":{"freeleechTokens":"nope"}}}`,
		``,
		`not-json`,
	} {
		got := ParseBalance("dicmusic", []byte(body))
		if got.Known {
			t.Fatal(body, got)
		}
	}
	page := ParseBalance("dicmusic", []byte(`<html><span>令牌: 4</span></html>`))
	if page.Known || page.ErrorClass != "binding" {
		t.Fatal(page)
	}
	login := ParseBalance("orpheus", []byte(`<html><form action="login.php"><input name="password"></form></html>`))
	if login.Known || login.ErrorClass != "auth" {
		t.Fatal(login)
	}
	prefer := ParseBalance("dicmusic", []byte(`{"status":"success","response":{"id":"7","personal":{"freeleechTokens":"1,000"},"userstats":{"tokens":2}}}`))
	if !prefer.Known || prefer.Balance != 1000 || prefer.Source != "personal.freeleechTokens" {
		t.Fatal(prefer)
	}
}

func TestLateObservationDoesNotOverwrite(t *testing.T) {
	ledger := openLedger(t)
	first := Observation{Site: "dicmusic", AccountID: "7", CookieID: "aaaa", Balance: 4, Known: true, Source: "userstats.tokens", ObservedAtNS: 100, Generation: 2}
	if _, err := ledger.CommitObservation(first); err != nil {
		t.Fatal(err)
	}
	late := first
	late.Balance = 99
	late.Generation = 1
	got, err := ledger.CommitObservation(late)
	if err != nil || got.Balance != 4 || got.Generation != 2 {
		t.Fatal(got, err)
	}
	stored, found, err := ledger.Load("dicmusic", "7")
	if err != nil || !found || stored.Balance != 4 {
		t.Fatal(stored, found, err)
	}
}

func TestHighBalanceReservesWithinTheBatchCap(t *testing.T) {
	ledger := openLedger(t)
	now := time.Unix(1_700_000_100, 0)
	obs := Observation{Site: "dicmusic", AccountID: "7", CookieID: "cookie", Balance: 252, Known: true, Source: "userstats.tokens", ObservedAtNS: now.UnixNano(), Generation: 1}
	if _, err := ledger.CommitObservation(obs); err != nil {
		t.Fatal(err)
	}
	unset := quote("dicmusic", "resource-1", "request-1", 1, 0, 0, 0)
	unset.AccountID = "7"
	unset.CookieID = "cookie"
	unset.BalanceKnown = false
	got, err := ledger.ReserveObserved(now, unset, "7")
	if err != nil || !got.Spent || got.Cost != 1 || got.Class != ClassReserved {
		t.Fatal(got, err)
	}
	limited := quote("dicmusic", "resource-2", "request-2", 1, 0, 1, 0)
	limited.AccountID = "7"
	limited.CookieID = "cookie"
	limited.BalanceKnown = false
	blocked, err := ledger.ReserveObserved(now, limited, "7")
	if err != nil || blocked.Spent || blocked.Class != ClassTokenBudget {
		t.Fatal(blocked, err)
	}
}

func TestCallerBalanceCannotAuthorizeAReservation(t *testing.T) {
	ledger := openLedger(t)
	now := time.Unix(1_700_000_100, 0)
	obs := Observation{Site: "dicmusic", AccountID: "7", CookieID: "cookie", Balance: 1, Known: true, Source: "userstats.tokens", ObservedAtNS: now.UnixNano(), Generation: 1}
	if _, err := ledger.CommitObservation(obs); err != nil {
		t.Fatal(err)
	}
	quote := quote("dicmusic", "resource-1", "request-1", 1, 99, 99, 0)
	quote.AccountID = "7"
	got, err := ledger.ReserveObserved(now, quote, "7")
	if err != nil || !got.Spent || got.Cost != 1 {
		t.Fatal(got, err)
	}
	unknown := quote
	unknown.RequestID = "request-2"
	unknown.ResourceID = "resource-2"
	got, err = ledger.ReserveObserved(now, unknown, "missing")
	if err != nil || got.Class != ClassBalanceUnknown || got.Spent {
		t.Fatal(got, err)
	}
}

func TestSpendRecordMustNameAccountResourceAndRequest(t *testing.T) {
	ledger := openLedger(t)
	now := time.Unix(1_700_000_100, 0)
	obs := Observation{Site: "orpheus", AccountID: "8", Balance: 3, Known: true, Source: "userstats.tokens", ObservedAtNS: now.UnixNano(), Generation: 1}
	if _, err := ledger.CommitObservation(obs); err != nil {
		t.Fatal(err)
	}
	quote := quote("orpheus", "11", "request-1", 1, 1, 1, 0)
	quote.AccountID = "8"
	if _, err := ledger.ReserveObserved(now, quote, "8"); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Advance(now.Add(time.Second), "orpheus", "request-1", StateRequestUnknown, "response-lost"); err != nil {
		t.Fatal(err)
	}
	incomplete, class := ParseSpend([]byte(`{"contract":"musicops.fixture.v1","state":"CONSUMED","resource_id":"11"}`))
	if class != ClassUncertain || incomplete.State != "" {
		t.Fatal(incomplete, class)
	}
	unchanged, err := ledger.Reconcile(now.Add(2*time.Second), "orpheus", incomplete)
	if err != nil || unchanged.Class != ClassUncertain {
		t.Fatal(unchanged, err)
	}
	proof, class := ParseSpend([]byte(`{"contract":"musicops.fixture.v1","state":"CONSUMED","account_id":"8","resource_id":"11","request_id":"request-1"}`))
	if class != "" || proof.ProductionVerified {
		t.Fatal(proof, class)
	}
	confirmed, err := ledger.Reconcile(now.Add(3*time.Second), "orpheus", proof)
	if err != nil || confirmed.State != StateConfirmed || confirmed.Downloaded || confirmed.ProductionVerified {
		t.Fatal(confirmed, err)
	}
}

func TestObservationSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	engine := openAt(t, dir)
	ledger := New(engine)
	obs := Observation{Site: "dicmusic", AccountID: "7", Balance: 2, Known: true, Source: "stats.tokens", ObservedAtNS: 50, Generation: 3}
	if _, err := ledger.CommitObservation(obs); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openAt(t, dir)
	t.Cleanup(func() { reopened.Close() })
	again, found, err := New(reopened).Load("dicmusic", "7")
	if err != nil || !found || again.Balance != 2 || again.Generation != 3 || again.ProductionVerified {
		t.Fatal(again, found, err)
	}
}
