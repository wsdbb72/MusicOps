package tokenledger

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFileSpendSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spend-proofs.jsonl")
	source := NewFileSpendSource(path)
	if _, found, err := source.Spend(context.Background(), "dicmusic", "req-1"); err != nil || found {
		t.Fatal("missing file must report no proof", found, err)
	}
	proof := `{"contract":"musicops.fixture.v1","account_id":"7","resource_id":"11","request_id":"req-1","state":"CONSUMED"}` + "\n"
	other := `{"contract":"musicops.fixture.v1","account_id":"7","resource_id":"12","request_id":"req-2","state":"NOT_CONSUMED"}` + "\n"
	bad := "not json\n"
	wrongContract := `{"contract":"something.else","account_id":"7","resource_id":"11","request_id":"req-3","state":"CONSUMED"}` + "\n"
	if err := os.WriteFile(path, []byte(bad+wrongContract+proof+other), 0o600); err != nil {
		t.Fatal(err)
	}
	got, found, err := source.Spend(context.Background(), "dicmusic", "req-1")
	if err != nil || !found || got.AccountID != "7" || got.ResourceID != "11" || got.State != "CONSUMED" {
		t.Fatal(got, found, err)
	}
	got, found, err = source.Spend(context.Background(), "dicmusic", "req-2")
	if err != nil || !found || got.State != "NOT_CONSUMED" {
		t.Fatal(got, found, err)
	}
	if _, found, err := source.Spend(context.Background(), "dicmusic", "req-3"); err != nil || found {
		t.Fatal("wrong-contract line must not match", found, err)
	}
	if _, found, err := source.Spend(context.Background(), "dicmusic", "req-9"); err != nil || found {
		t.Fatal("unknown request must report no proof", found, err)
	}
}
