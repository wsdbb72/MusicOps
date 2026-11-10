package app

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/tokenledger"

	_ "modernc.org/sqlite"
)

func TestLegacyProfileBecomesLiveDataWithoutTouchingAudio(t *testing.T) {
	profile := t.TempDir()
	library := filepath.Join(profile, "library")
	if err := os.MkdirAll(filepath.Join(profile, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(library, 0o700); err != nil {
		t.Fatal(err)
	}
	audio := filepath.Join(library, "song.flac")
	body := []byte("FLAC-bytes-not-modified\n")
	if err := os.WriteFile(audio, body, 0o600); err != nil {
		t.Fatal(err)
	}
	beforeAudio := sha256.Sum256(body)
	writeLegacyDB(t, profile, library, audio)
	sqlitePath := filepath.Join(profile, "state", "musicops.sqlite")
	beforeDB := fileSum(t, sqlitePath)

	application := openProfile(t, profile)
	jobs := migrationGet(t, application, "/v1/jobs")
	jobRows, _ := jobs["jobs"].([]any)
	if len(jobRows) != 1 {
		t.Fatalf("jobs = %v", jobs)
	}
	job := jobRows[0].(map[string]any)
	if job["stage"] != "needs_review" || job["downloaded"] == true || job["error_class"] != "migrated" {
		t.Fatalf("job = %v", job)
	}
	message, _ := job["message"].(string)
	if message == "" || !contains(message, "尚未继续下载") {
		t.Fatalf("message = %q", message)
	}
	libraryBody := migrationGet(t, application, "/v1/library")
	if libraryBody["count"] != float64(1) {
		t.Fatalf("library = %v", libraryBody)
	}
	entries := libraryBody["entries"].([]any)
	entry := entries[0].(map[string]any)
	if entry["file_present"] != true || entry["downloaded"] == true {
		t.Fatalf("entry = %v", entry)
	}
	logs := migrationGet(t, application, "/v1/logs")
	encoded, _ := json.Marshal(logs)
	if contains(string(encoded), "secret-cookie") {
		t.Fatal(string(encoded))
	}
	status := migrationGet(t, application, "/v1/migration")
	if status["found"] != true || status["cold_archive"] != true || status["auto_install"] != false {
		t.Fatalf("status = %v", status)
	}
	now := time.Now()
	blocked, err := application.ledger.Reserve(now, tokenledger.Quote{
		Site: "dicmusic", ResourceID: "99", RequestID: "after-upgrade", Cost: 1,
		BalanceKnown: true, Balance: 20, Budget: 10, SafetyMargin: 1,
		ObservedAt: now, BudgetRef: "caller-budget", Scene: "complete",
	})
	if err != nil || blocked.Class != tokenledger.ClassAccountHold || blocked.Spent {
		t.Fatalf("blocked = %+v %v", blocked, err)
	}
	same, err := application.ledger.Reserve(now, tokenledger.Quote{
		Site: "dicmusic", ResourceID: "11", RequestID: "same-resource", Cost: 1,
		BalanceKnown: true, Balance: 20, Budget: 10, SafetyMargin: 1,
		ObservedAt: now, BudgetRef: "caller-budget", Scene: "complete",
	})
	if err != nil || same.Class != tokenledger.ClassPending || same.Spent {
		t.Fatalf("same = %+v %v", same, err)
	}
	if fileSum(t, sqlitePath) != beforeDB || fileSum(t, audio) != hex.EncodeToString(beforeAudio[:]) {
		t.Fatal("migration changed the old database or the audio file")
	}

	application.Close()
	again := openProfile(t, profile)
	replayed := migrationGet(t, again, "/v1/migration")
	report, _ := replayed["report"].(map[string]any)
	if report["jobs"] != float64(1) || report["library"] != float64(1) || report["tokens"] != float64(1) {
		t.Fatalf("replay = %v", replayed)
	}
	againJobs := migrationGet(t, again, "/v1/jobs")
	if rows, _ := againJobs["jobs"].([]any); len(rows) != 1 {
		t.Fatalf("second start jobs = %v", againJobs)
	}
	againLibrary := migrationGet(t, again, "/v1/library")
	if againLibrary["count"] != float64(1) {
		t.Fatalf("second start library = %v", againLibrary)
	}
	rolled := post(t, again, "/v1/migration/rollback", map[string]any{})
	if rolled["audio_unchanged"] != true {
		t.Fatal(rolled)
	}
	after := migrationGet(t, again, "/v1/library")
	if after["count"] != float64(0) {
		t.Fatalf("library after rollback = %v", after)
	}
	jobs = migrationGet(t, again, "/v1/jobs")
	if rows, _ := jobs["jobs"].([]any); len(rows) != 0 {
		t.Fatalf("jobs after rollback = %v", jobs)
	}
	kept := migrationGet(t, again, "/v1/migration")
	if kept["cold_archive"] != true || kept["found"] == true {
		t.Fatalf("archive = %v", kept)
	}
	if fileSum(t, audio) != hex.EncodeToString(beforeAudio[:]) || fileSum(t, sqlitePath) != beforeDB {
		t.Fatal("rollback changed the audio file or the old database")
	}
	afterNow := time.Now()
	allowed, err := again.ledger.Reserve(afterNow, tokenledger.Quote{
		Site: "dicmusic", ResourceID: "99", RequestID: "after-rollback", Cost: 1,
		BalanceKnown: true, Balance: 20, Budget: 10, SafetyMargin: 1,
		ObservedAt: afterNow, BudgetRef: "caller-budget", Scene: "complete",
	})
	if err != nil || allowed.State != tokenledger.StateReserved || allowed.Class == tokenledger.ClassAccountHold {
		t.Fatalf("after rollback = %+v %v", allowed, err)
	}
}

func openProfile(t *testing.T, profile string) *App {
	t.Helper()
	application, err := Open(filepath.Join(profile, "state", "native"), filepath.Join(profile, "backups"), "test-password-16b")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { application.Close() })
	return application
}

func writeLegacyDB(t *testing.T, profile, library, audio string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(profile, "state", "musicops.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE config_versions (id TEXT PRIMARY KEY, payload TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE library_files (id TEXT PRIMARY KEY, payload TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE evidence (id TEXT PRIMARY KEY, payload TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE token_reservations (id TEXT PRIMARY KEY, site TEXT NOT NULL, resource_id TEXT NOT NULL, cost INTEGER NOT NULL, state TEXT NOT NULL)`,
		`CREATE TABLE events (seq INTEGER PRIMARY KEY, at TEXT NOT NULL, phase TEXT NOT NULL, task_id TEXT, payload TEXT NOT NULL)`,
	}
	for _, statement := range statements {
		if _, err = db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	config := `{"version":2,"values":{"streaming":{"enabled":false},"providers":{"dicmusic":{"enabled":true}},"updates":{"enabled":true},"paths":{"library":"` + library + `"},"offline":false,"workflow":{"setup_done":true}}}`
	filePayload := `{"path":"` + audio + `","albumartist":"测试艺人","album":"测试专辑","tracknumber":1,"location_state":"library"}`
	active := `{"stage":"searching","scene":"complete","label":"旧任务","message":"上次停在检索"}`
	done := `{"stage":"completed","scene":"complete","label":"已完成"}`
	hidden := `{"stage":"searching","scene":"complete","label":"隐藏","hidden":true}`
	if _, err = db.Exec(`INSERT INTO config_versions(id, payload, updated_at) VALUES('2', ?, '2026-01-01')`, config); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO library_files(id, payload, updated_at) VALUES('f1', ?, '2026-01-01')`, filePayload); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO evidence(id, payload, updated_at) VALUES('workflow:job-legacy-1', ?, '2026-01-01')`, active); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO evidence(id, payload, updated_at) VALUES('workflow:job-done', ?, '2026-01-01')`, done); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO evidence(id, payload, updated_at) VALUES('workflow:job-hidden', ?, '2026-01-01')`, hidden); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO token_reservations(id, site, resource_id, cost, state) VALUES('req-old-1', 'dicmusic', '11', 1, 'REQUESTING')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO token_reservations(id, site, resource_id, cost, state) VALUES('req-old-2', 'dicmusic', '12', 1, 'RELEASED')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO events(seq, at, phase, task_id, payload) VALUES(1, '2026-01-01T00:00:00Z', 'search', 'job-legacy-1', 'cookie=secret-cookie')`); err != nil {
		t.Fatal(err)
	}
}

func migrationGet(t *testing.T, application *App, path string) map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.SetBasicAuth("musicops", "test-password-16b")
	recorder := httptest.NewRecorder()
	application.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func fileSum(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func contains(value, part string) bool {
	return len(part) == 0 || (len(value) >= len(part) && (value == part || len(value) > 0 && stringIndex(value, part) >= 0))
}

func stringIndex(value, part string) int {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return i
		}
	}
	return -1
}
