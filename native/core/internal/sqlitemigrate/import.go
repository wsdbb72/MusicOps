// Package sqlitemigrate copies a quiescent MusicOps SQLite v1/v2 file into Pebble.
// The source is opened read-only. There is no SQL query layer over the native store.
package sqlitemigrate

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	_ "modernc.org/sqlite"
)

var (
	ErrConflict      = errors.New("sqlite archive conflicts with an existing snapshot")
	ErrSchema        = errors.New("sqlite schema cannot be imported")
	ErrSourceChanged = errors.New("sqlite source changed during import")
	ErrPaused        = errors.New("sqlite import paused before verification")
)

const maxRowBytes = 32 << 20

// Report is the verification summary. It does not claim the native store is the live profile.
type Report struct {
	UserVersion  int            `json:"user_version"`
	SourceSHA256 string         `json:"source_sha256"`
	SourceBytes  int64          `json:"source_bytes"`
	Tables       map[string]int `json:"tables"`
	Rows         int            `json:"rows"`
	State        string         `json:"state"`
	Replayed     bool           `json:"replayed"`
	Resumed      bool           `json:"resumed"`
	Verified     bool           `json:"verified"`
	ElapsedMS    int64          `json:"elapsed_ms"`
}

type Record struct {
	Table   string   `json:"table"`
	PK      string   `json:"pk"`
	SHA256  string   `json:"sha256"`
	Columns []Column `json:"columns"`
}

type Column struct {
	Name string `json:"name"`
	Enc  string `json:"enc"`
}

type manifest struct {
	State        string         `json:"state"`
	UserVersion  int            `json:"user_version"`
	SourceSHA256 string         `json:"source_sha256"`
	SourceBytes  int64          `json:"source_bytes"`
	Tables       map[string]int `json:"tables"`
	Rows         int            `json:"rows"`
}

type cursor struct {
	Table string `json:"table"`
	PK    string `json:"pk"`
	Rows  int    `json:"rows"`
}

// Import copies every user table, verifies per-record hashes, and can resume
// from the last committed cursor. stopAfter is zero in production; tests use it
// to simulate a crash between batches.
func Import(engine *dataengine.Engine, source string) (Report, error) {
	return importSource(engine, source, 0)
}

func importSource(engine *dataengine.Engine, source string, stopAfter int) (Report, error) {
	started := time.Now()
	if engine == nil || stopAfter < 0 {
		return Report{}, dataengine.ErrInvalidRequest
	}
	before, err := fingerprint(source)
	if err != nil {
		return Report{}, err
	}
	existing, err := readManifest(engine)
	if err != nil {
		return Report{}, err
	}
	resumed := false
	if existing != nil {
		if existing.SourceSHA256 != before.sum {
			return Report{}, ErrConflict
		}
		if existing.State == "verified" {
			report, err := verify(engine, source, *existing)
			if err != nil {
				return Report{}, err
			}
			report.Replayed = true
			report.ElapsedMS = time.Since(started).Milliseconds()
			return report, nil
		}
		resumed = existing.State == "copying"
	}
	copyPath, cleanup, err := isolate(source)
	if err != nil {
		return Report{}, err
	}
	defer cleanup()
	db, err := sql.Open("sqlite", sqliteDSN(copyPath))
	if err != nil {
		return Report{}, err
	}
	defer db.Close()
	version, err := userVersion(db)
	if err != nil {
		return Report{}, err
	}
	if version != 1 && version != 2 {
		return Report{}, ErrSchema
	}
	tables, err := userTables(db)
	if err != nil {
		return Report{}, err
	}
	state := manifest{State: "copying", UserVersion: version, SourceSHA256: before.sum, SourceBytes: before.bytes, Tables: map[string]int{}}
	if existing != nil {
		state.Rows = existing.Rows
		state.Tables = existing.Tables
	}
	if err = writeManifest(engine, state); err != nil {
		return Report{}, err
	}
	position, err := readCursor(engine)
	if err != nil {
		return Report{}, err
	}
	written := 0
	for _, table := range tables {
		if position != nil && table < position.Table {
			continue
		}
		rows, err := readTable(db, table)
		if err != nil {
			return Report{}, err
		}
		for _, row := range rows {
			if position != nil && table == position.Table && row.PK <= position.PK {
				continue
			}
			if err = putRow(engine, row); err != nil {
				return Report{}, err
			}
			state.Rows++
			state.Tables[table]++
			written++
			if err = writeCursor(engine, cursor{Table: table, PK: row.PK, Rows: state.Rows}); err != nil {
				return Report{}, err
			}
			if err = writeManifest(engine, state); err != nil {
				return Report{}, err
			}
			if stopAfter > 0 && written >= stopAfter {
				return Report{UserVersion: version, SourceSHA256: before.sum, SourceBytes: before.bytes, Tables: state.Tables, Rows: state.Rows, State: "copying", Resumed: resumed}, ErrPaused
			}
		}
	}
	after, err := fingerprint(source)
	if err != nil {
		return Report{}, err
	}
	if after.sum != before.sum || after.bytes != before.bytes {
		return Report{}, ErrSourceChanged
	}
	state.State = "verified"
	if err = writeManifest(engine, state); err != nil {
		return Report{}, err
	}
	if err = deleteKey(engine, []byte("archive/meta/cursor")); err != nil {
		return Report{}, err
	}
	report, err := verify(engine, source, state)
	if err != nil {
		return Report{}, err
	}
	report.Resumed = resumed
	report.ElapsedMS = time.Since(started).Milliseconds()
	return report, nil
}

// Verify re-reads the source and compares counts and per-record hashes.
func Verify(engine *dataengine.Engine, source string) (Report, error) {
	existing, err := readManifest(engine)
	if err != nil {
		return Report{}, err
	}
	if existing == nil || existing.State != "verified" {
		return Report{}, ErrSchema
	}
	return verify(engine, source, *existing)
}

func verify(engine *dataengine.Engine, source string, expected manifest) (Report, error) {
	sum, err := fingerprint(source)
	if err != nil {
		return Report{}, err
	}
	if sum.sum != expected.SourceSHA256 {
		return Report{}, ErrSourceChanged
	}
	copyPath, cleanup, err := isolate(source)
	if err != nil {
		return Report{}, err
	}
	defer cleanup()
	db, err := sql.Open("sqlite", sqliteDSN(copyPath))
	if err != nil {
		return Report{}, err
	}
	defer db.Close()
	version, err := userVersion(db)
	if err != nil {
		return Report{}, err
	}
	if version != expected.UserVersion {
		return Report{}, ErrConflict
	}
	tables, err := userTables(db)
	if err != nil {
		return Report{}, err
	}
	counts := map[string]int{}
	rows := 0
	for _, table := range tables {
		read, err := readTable(db, table)
		if err != nil {
			return Report{}, err
		}
		counts[table] = len(read)
		rows += len(read)
		for _, row := range read {
			stored, found, err := Get(engine, table, row.PK)
			if err != nil {
				return Report{}, err
			}
			if !found || stored.SHA256 != row.SHA256 {
				return Report{}, ErrConflict
			}
		}
	}
	if rows != expected.Rows {
		return Report{}, ErrConflict
	}
	for table, count := range counts {
		if expected.Tables[table] != count {
			return Report{}, ErrConflict
		}
	}
	return Report{UserVersion: version, SourceSHA256: expected.SourceSHA256, SourceBytes: expected.SourceBytes, Tables: counts, Rows: rows, State: "verified", Verified: true}, nil
}

// Rollback removes the imported archive and leaves the SQLite file untouched.
func Rollback(engine *dataengine.Engine) (int, error) {
	if engine == nil {
		return 0, dataengine.ErrInvalidRequest
	}
	deleted := 0
	for _, prefix := range [][]byte{[]byte("archive/row/"), []byte("archive/id/"), []byte("archive/meta/")} {
		for {
			var page dataengine.Page
			err := engine.View(func(v *dataengine.View) error {
				var scanErr error
				page, scanErr = v.Scan(prefix, nil, 200)
				return scanErr
			})
			if err != nil {
				return deleted, err
			}
			if len(page.Items) == 0 {
				break
			}
			mutations := make([]dataengine.Mutation, 0, len(page.Items))
			for _, item := range page.Items {
				mutations = append(mutations, dataengine.Mutation{Key: item.Key, Delete: true})
			}
			ok, err := engine.CompareAndSwap(nil, mutations)
			if err != nil || !ok {
				if err == nil {
					err = ErrConflict
				}
				return deleted, err
			}
			deleted += len(page.Items)
		}
	}
	return deleted, nil
}

// Get returns one imported record by its primary-key display text.
func Get(engine *dataengine.Engine, table, pk string) (Record, bool, error) {
	if !safeName(table) || pk == "" {
		return Record{}, false, dataengine.ErrInvalidRequest
	}
	var raw []byte
	var found bool
	err := engine.View(func(v *dataengine.View) error {
		value, ok, err := v.Get(indexKey(table, pk))
		if err != nil || !ok {
			found = ok
			return err
		}
		raw, found, err = v.Get(value)
		return err
	})
	if err != nil || !found {
		return Record{}, false, err
	}
	var record Record
	if err = json.Unmarshal(raw, &record); err != nil {
		return Record{}, false, err
	}
	return record, true, nil
}

type fileSum struct {
	sum   string
	bytes int64
}

func fingerprint(path string) (fileSum, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return fileSum{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fileSum{}, dataengine.ErrInvalidRequest
	}
	if info.Size() > 8<<30 {
		return fileSum{}, dataengine.ErrInvalidRequest
	}
	hash := sha256.New()
	total := int64(0)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		name := path + suffix
		stat, err := os.Lstat(name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && suffix != "" {
				continue
			}
			if suffix == "" {
				return fileSum{}, err
			}
			return fileSum{}, err
		}
		if !stat.Mode().IsRegular() || stat.Mode()&os.ModeSymlink != 0 {
			return fileSum{}, dataengine.ErrInvalidRequest
		}
		file, err := os.Open(name)
		if err != nil {
			return fileSum{}, err
		}
		n, err := io.Copy(hash, file)
		closeErr := file.Close()
		if err != nil {
			return fileSum{}, err
		}
		if closeErr != nil {
			return fileSum{}, closeErr
		}
		total += n
		hash.Write([]byte(suffix))
	}
	return fileSum{sum: hex.EncodeToString(hash.Sum(nil)), bytes: total}, nil
}

func isolate(source string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "musicops-sqlite-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	dest := filepath.Join(dir, "source.sqlite")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err = os.Lstat(source + suffix); err != nil {
			if errors.Is(err, os.ErrNotExist) && suffix != "" {
				continue
			}
			if suffix == "" {
				cleanup()
				return "", nil, err
			}
			cleanup()
			return "", nil, err
		}
		if err = copyReadOnly(source+suffix, dest+suffix); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return dest, cleanup, nil
}

func copyReadOnly(source, dest string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func sqliteDSN(path string) string {
	return "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=query_only(1)"
}

func userVersion(db *sql.DB) (int, error) {
	var version int
	err := db.QueryRow("PRAGMA user_version").Scan(&version)
	return version, err
}

func userTables(db *sql.DB) ([]string, error) {
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		if !safeName(name) {
			return nil, ErrSchema
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

type columnInfo struct {
	name string
	pk   int
}

func readTable(db *sql.DB, table string) ([]Record, error) {
	infoRows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	columns := []columnInfo{}
	for infoRows.Next() {
		var cid, pk int
		var name, colType string
		var notNull int
		var dflt any
		if err = infoRows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			infoRows.Close()
			return nil, err
		}
		if !safeName(name) {
			infoRows.Close()
			return nil, ErrSchema
		}
		columns = append(columns, columnInfo{name: name, pk: pk})
	}
	if err = infoRows.Err(); err != nil {
		infoRows.Close()
		return nil, err
	}
	infoRows.Close()
	if len(columns) == 0 {
		return nil, ErrSchema
	}
	order := append([]columnInfo(nil), columns...)
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].pk == 0 || order[j].pk == 0 {
			return order[i].pk > order[j].pk
		}
		return order[i].pk < order[j].pk
	})
	query := "SELECT rowid, " + quotedColumns(columns) + " FROM " + table + " ORDER BY "
	parts := []string{}
	for _, column := range order {
		if column.pk > 0 {
			parts = append(parts, `"`+column.name+`"`)
		}
	}
	parts = append(parts, "rowid")
	rows, err := db.Query(query + strings.Join(parts, ", "))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []Record{}
	for rows.Next() {
		values := make([]any, len(columns)+1)
		targets := make([]any, len(values))
		for i := range values {
			targets[i] = &values[i]
		}
		if err = rows.Scan(targets...); err != nil {
			return nil, err
		}
		record, err := encodeRow(table, columns, values[1:])
		if err != nil {
			return nil, err
		}
		if record.PK == "" {
			record.PK = strconv.FormatInt(asInt(values[0]), 10)
			record.SHA256 = hashRecord(record)
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func quotedColumns(columns []columnInfo) string {
	parts := make([]string, len(columns))
	for i, column := range columns {
		parts[i] = `"` + column.name + `"`
	}
	return strings.Join(parts, ", ")
}

func encodeRow(table string, columns []columnInfo, values []any) (Record, error) {
	record := Record{Table: table, Columns: make([]Column, 0, len(columns))}
	pkParts := []string{}
	var builder strings.Builder
	builder.WriteString(table)
	builder.WriteByte('\n')
	for i, column := range columns {
		enc, err := encodeValue(values[i])
		if err != nil {
			return Record{}, err
		}
		if len(enc) > maxRowBytes {
			return Record{}, dataengine.ErrInvalidRequest
		}
		record.Columns = append(record.Columns, Column{Name: column.name, Enc: enc})
		builder.WriteString(column.name)
		builder.WriteByte('=')
		builder.WriteString(enc)
		builder.WriteByte('\n')
		if column.pk > 0 {
			pkParts = append(pkParts, display(values[i]))
		}
	}
	record.PK = strings.Join(pkParts, "\x1f")
	sum := sha256.Sum256([]byte(builder.String()))
	record.SHA256 = hex.EncodeToString(sum[:])
	return record, nil
}

func hashRecord(record Record) string {
	raw, _ := json.Marshal(record.Columns)
	sum := sha256.Sum256(append([]byte(record.Table+"\n"+record.PK+"\n"), raw...))
	return hex.EncodeToString(sum[:])
}

func encodeValue(value any) (string, error) {
	switch typed := value.(type) {
	case nil:
		return "N", nil
	case int64:
		return "I" + strconv.FormatInt(typed, 10), nil
	case float64:
		return "F" + strconv.FormatFloat(typed, 'g', -1, 64), nil
	case string:
		return "T" + typed, nil
	case []byte:
		return "B" + hex.EncodeToString(typed), nil
	default:
		return "", ErrSchema
	}
}

func display(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case string:
		return typed
	case []byte:
		return hex.EncodeToString(typed)
	default:
		return ""
	}
}

func asInt(value any) int64 {
	typed, _ := value.(int64)
	return typed
}

func safeName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		if r == '_' || r >= '0' && r <= '9' && i > 0 || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
			continue
		}
		return false
	}
	return true
}

func putRow(engine *dataengine.Engine, row Record) error {
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	key := rowKey(row.Table, row.PK)
	index := indexKey(row.Table, row.PK)
	var current []byte
	var found bool
	err = engine.View(func(v *dataengine.View) error {
		var getErr error
		current, found, getErr = v.Get(key)
		return getErr
	})
	if err != nil {
		return err
	}
	if found {
		if string(current) != string(raw) {
			return ErrConflict
		}
		return nil
	}
	ok, err := engine.CompareAndSwap(
		[]dataengine.Condition{{Key: key, Absent: true}, {Key: index, Absent: true}},
		[]dataengine.Mutation{{Key: key, Value: raw}, {Key: index, Value: key}},
	)
	if err != nil {
		return err
	}
	if !ok {
		return putRow(engine, row)
	}
	return nil
}

func rowKey(table, pk string) []byte {
	sum := sha256.Sum256([]byte(pk))
	return []byte("archive/row/" + table + "/" + hex.EncodeToString(sum[:]))
}

func indexKey(table, pk string) []byte {
	sum := sha256.Sum256([]byte(pk))
	return []byte("archive/id/" + table + "/" + hex.EncodeToString(sum[:]))
}

func readManifest(engine *dataengine.Engine) (*manifest, error) {
	var raw []byte
	var found bool
	err := engine.View(func(v *dataengine.View) error {
		var getErr error
		raw, found, getErr = v.Get([]byte("archive/meta/manifest"))
		return getErr
	})
	if err != nil || !found {
		return nil, err
	}
	var decoded manifest
	if err = json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return &decoded, nil
}

func writeManifest(engine *dataengine.Engine, value manifest) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return upsert(engine, []byte("archive/meta/manifest"), raw)
}

func readCursor(engine *dataengine.Engine) (*cursor, error) {
	var raw []byte
	var found bool
	err := engine.View(func(v *dataengine.View) error {
		var getErr error
		raw, found, getErr = v.Get([]byte("archive/meta/cursor"))
		return getErr
	})
	if err != nil || !found {
		return nil, err
	}
	var decoded cursor
	if err = json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return &decoded, nil
}

func writeCursor(engine *dataengine.Engine, value cursor) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return upsert(engine, []byte("archive/meta/cursor"), raw)
}

func upsert(engine *dataengine.Engine, key, value []byte) error {
	for {
		var current []byte
		var found bool
		err := engine.View(func(v *dataengine.View) error {
			var getErr error
			current, found, getErr = v.Get(key)
			return getErr
		})
		if err != nil {
			return err
		}
		if found && string(current) == string(value) {
			return nil
		}
		condition := dataengine.Condition{Key: key, Absent: !found, Value: current}
		ok, err := engine.CompareAndSwap([]dataengine.Condition{condition}, []dataengine.Mutation{{Key: key, Value: value}})
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
}

func deleteKey(engine *dataengine.Engine, key []byte) error {
	ok, err := engine.CompareAndSwap(nil, []dataengine.Mutation{{Key: key, Delete: true}})
	if err != nil || ok {
		return err
	}
	return nil
}
