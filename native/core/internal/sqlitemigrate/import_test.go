package sqlitemigrate

import (
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	_ "modernc.org/sqlite"
)

func TestImportVerifiesHashesResumesAndRollsBack(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "musicops.sqlite")
	createLegacy(t, source, 2)
	before := hashFile(t, source)
	engine := openEngine(t)
	if _, err := importSource(engine, source, 1); !errors.Is(err, ErrPaused) {
		t.Fatal(err)
	}
	report, err := Import(engine, source)
	if err != nil || !report.Resumed || !report.Verified || report.Rows < 3 {
		t.Fatal(report, err)
	}
	record, found, err := Get(engine, "evidence", "cred-1")
	if err != nil || !found || !contains(record, "vault://pt/dic") || !contains(record, "凭据引用") {
		t.Fatal(record, found, err)
	}
	replay, err := Import(engine, source)
	if err != nil || !replay.Replayed || replay.Rows != report.Rows {
		t.Fatal(replay, err)
	}
	if hashFile(t, source) != before {
		t.Fatal("source file changed")
	}
	mutated := filepath.Join(dir, "changed.sqlite")
	if err = copyReadOnly(source, mutated); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(mutated)+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO tasks(id,payload,updated_at) VALUES('task-2','{"id":"task-2"}','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	fresh := openEngine(t)
	if _, err = Import(fresh, source); err != nil {
		t.Fatal(err)
	}
	if _, err = Import(fresh, mutated); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	deleted, err := Rollback(fresh)
	if err != nil || deleted == 0 {
		t.Fatal(deleted, err)
	}
	if _, found, err = Get(fresh, "evidence", "cred-1"); err != nil || found {
		t.Fatal(found, err)
	}
	if hashFile(t, source) != before {
		t.Fatal("rollback touched the source")
	}
}

func TestSchemaTooNewImportsNothing(t *testing.T) {
	source := filepath.Join(t.TempDir(), "future.sqlite")
	createLegacy(t, source, 9)
	engine := openEngine(t)
	if _, err := Import(engine, source); !errors.Is(err, ErrSchema) {
		t.Fatal(err)
	}
	if _, found, err := Get(engine, "evidence", "cred-1"); err != nil || found {
		t.Fatal(found, err)
	}
}

func TestImportBaseline(t *testing.T) {
	source := filepath.Join(t.TempDir(), "baseline.sqlite")
	db := openWriter(t, source, 2)
	if _, err := db.Exec(`CREATE TABLE evidence (id TEXT PRIMARY KEY, payload TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	payload := stringsRepeat(256 << 10)
	for i := 0; i < 200; i++ {
		id := "row-" + strconvItoa(i)
		body := `{"id":"` + id + `","credential_ref":"vault://pt/dic"}`
		if i == 0 {
			body = `{"id":"` + id + `","blob":"` + payload + `"}`
		}
		if _, err := db.Exec(`INSERT INTO evidence(id,payload,updated_at) VALUES(?,?,?)`, id, body, "2026-01-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	scanStarted := time.Now()
	scanDB := openWriter(t, source, 2)
	rows, err := scanDB.Query(`SELECT id, payload, updated_at FROM evidence`)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		var id, payload, updated string
		if err = rows.Scan(&id, &payload, &updated); err != nil {
			t.Fatal(err)
		}
		count++
	}
	rows.Close()
	scanDB.Close()
	scanElapsed := time.Since(scanStarted)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	report, err := Import(openEngine(t), source)
	elapsed := time.Since(started)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if err != nil || report.Rows != count {
		t.Fatal(report, err)
	}
	t.Logf("sqlite_scan rows=%d elapsed=%s", count, scanElapsed)
	t.Logf("pebble_import rows=%d elapsed=%s total_alloc_delta=%d heap_alloc_delta=%d", report.Rows, elapsed, after.TotalAlloc-before.TotalAlloc, int64(after.Alloc)-int64(before.Alloc))
}

func createLegacy(t *testing.T, path string, version int) {
	t.Helper()
	db := openWriter(t, path, version)
	defer db.Close()
	statements := []string{
		`CREATE TABLE evidence (id TEXT PRIMARY KEY, payload TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE tasks (id TEXT PRIMARY KEY, payload TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE events (seq INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, phase TEXT NOT NULL, task_id TEXT, tx_id TEXT, payload TEXT NOT NULL)`,
		`CREATE TABLE token_reservations (id TEXT PRIMARY KEY, site TEXT NOT NULL, resource_id TEXT NOT NULL, cost INTEGER NOT NULL, state TEXT NOT NULL, payload TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`INSERT INTO evidence(id,payload,updated_at) VALUES('cred-1','{"credential_ref":"vault://pt/dic","note":"凭据引用"}','2026-01-01T00:00:00Z')`,
		`INSERT INTO tasks(id,payload,updated_at) VALUES('task-1','{"state":"completed","command":"search"}','2026-01-01T00:00:00Z')`,
		`INSERT INTO events(at,phase,task_id,tx_id,payload) VALUES('2026-01-01T00:00:00Z','search','task-1',NULL,'{"result":"kept"}')`,
		`INSERT INTO token_reservations(id,site,resource_id,cost,state,payload,updated_at) VALUES('res-1','dicmusic','11',0,'REJECTED','{"site":"dicmusic","resource_id":"11"}','2026-01-01T00:00:00Z')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func openWriter(t *testing.T, path string, version int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("PRAGMA user_version = " + strconvItoa(version)); err != nil {
		t.Fatal(err)
	}
	return db
}

func openEngine(t *testing.T) *dataengine.Engine {
	t.Helper()
	engine, err := dataengine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine
}

func hashFile(t *testing.T, path string) string {
	t.Helper()
	sum, err := fingerprint(path)
	if err != nil {
		t.Fatal(err)
	}
	return sum.sum
}

func contains(record Record, text string) bool {
	for _, column := range record.Columns {
		if len(column.Enc) >= len(text) && (column.Enc == "T"+text || len(column.Enc) > 0 && containsString(column.Enc, text)) {
			return true
		}
	}
	return false
}

func containsString(value, needle string) bool {
	return len(needle) == 0 || (len(value) >= len(needle) && (value == needle || len(value) > len(needle) && search(value, needle)))
}

func search(value, needle string) bool {
	for i := 0; i+len(needle) <= len(value); i++ {
		if value[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func stringsRepeat(n int) string {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = 'a'
	}
	return string(buf)
}

func strconvItoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
