package sqlitemigrate

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestProjectTypedHistoryResumesAndKeepsTheArchive(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "musicops.sqlite")
	createLegacy(t, source, 2)
	engine := openEngine(t)
	if _, err := Project(engine, 0); !errors.Is(err, ErrSchema) {
		t.Fatal(err)
	}
	if _, err := Import(engine, source); err != nil {
		t.Fatal(err)
	}
	if _, err := Project(engine, 1); !errors.Is(err, ErrPaused) {
		t.Fatal(err)
	}
	report, err := Project(engine, 0)
	if err != nil || !report.Verified || !report.Resumed || report.Rows != 4 || report.CredentialRefs != 1 || report.Decoded != 4 {
		t.Fatal(report, err)
	}
	task, found, err := DomainTask(engine, "task-1")
	if err != nil || !found || task.Kind != "task" || string(task.Payload) == "" {
		t.Fatal(task, found, err)
	}
	token, found, err := DomainToken(engine, "res-1")
	if err != nil || !found || token.Kind != "token" || token.Site != "dicmusic" || token.State != "REJECTED" || token.Cost == nil || *token.Cost != 0 {
		t.Fatal(token, found, err)
	}
	replay, err := Project(engine, 0)
	if err != nil || !replay.Replayed || replay.Rows != report.Rows {
		t.Fatal(replay, err)
	}
	deleted, err := RollbackDomain(engine)
	if err != nil || deleted == 0 {
		t.Fatal(deleted, err)
	}
	if _, found, err = DomainTask(engine, "task-1"); err != nil || found {
		t.Fatal(found, err)
	}
	record, found, err := Get(engine, "evidence", "cred-1")
	if err != nil || !found || !contains(record, "vault://pt/dic") {
		t.Fatal(record, found, err)
	}
}
