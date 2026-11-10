// Package livemigrate turns a 0.4.31 SQLite profile into the live Go
// library index, review queue and token ledger. It does not move or
// modify audio files. The cold archive from sqlitemigrate is kept.
package livemigrate

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/sqlitemigrate"
	"github.com/wsdbb72/MusicOps/native/core/internal/tokenledger"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"

	_ "modernc.org/sqlite"
)

const receiptKey = "app/migration/live"
const eventsKey = "app/migration/events"

// Report is the live-migration receipt.
type Report struct {
	SourceSHA256 string   `json:"source_sha256"`
	Library      int      `json:"library"`
	Jobs         int      `json:"jobs"`
	Tokens       int      `json:"tokens"`
	Events       int      `json:"events"`
	LibraryKeys  []string `json:"library_keys"`
	JobIDs       []string `json:"job_ids"`
	TokenIDs     []string `json:"token_ids"`
	Replayed     bool     `json:"replayed"`
	Notes        []string `json:"notes"`
}

type libraryKey struct {
	RequestID string `json:"request_id"`
	RootID    string `json:"root_id"`
	Track     int    `json:"track"`
}

type storedReceipt struct {
	Report
	Files []libraryKey `json:"files"`
}

// Activate imports one Python profile into the open native engine.
// Missing sqlite is a skipped success. A matching receipt is a replay.
func Activate(engine *dataengine.Engine, profile string, flows *workflow.Repository, ledger *tokenledger.Ledger, now time.Time) (Report, error) {
	if engine == nil || flows == nil || ledger == nil || now.IsZero() {
		return Report{}, dataengine.ErrInvalidRequest
	}
	sqlitePath := filepath.Join(profile, "state", "musicops.sqlite")
	if _, err := os.Stat(sqlitePath); errors.Is(err, os.ErrNotExist) {
		return Report{Notes: []string{"没有旧版数据库"}}, nil
	} else if err != nil {
		return Report{}, err
	}
	sum, err := fileSHA(sqlitePath)
	if err != nil {
		return Report{}, err
	}
	if old, ok, err := readReceipt(engine); err != nil {
		return Report{}, err
	} else if ok && old.SourceSHA256 == sum {
		old.Replayed = true
		return old.Report, nil
	}
	if _, err = sqlitemigrate.Import(engine, sqlitePath); err != nil && !errors.Is(err, sqlitemigrate.ErrConflict) {
		return Report{}, err
	}
	if _, err = sqlitemigrate.Project(engine, 0); err != nil && !errors.Is(err, sqlitemigrate.ErrConflict) {
		return Report{}, err
	}
	db, cleanup, err := openCopy(sqlitePath)
	if err != nil {
		return Report{}, err
	}
	defer cleanup()
	libraryRoot, _ := libraryDir(db)
	report := Report{SourceSHA256: sum, Notes: []string{"音乐文件保持原路径，没有复制或改写", "冷归档保留在 sqlitemigrate 的域文档里"}}
	var files []libraryKey
	files, report.Library, err = importLibrary(engine, db, libraryRoot)
	if err != nil {
		return Report{}, err
	}
	report.LibraryKeys = make([]string, len(files))
	for i, file := range files {
		report.LibraryKeys[i] = file.RequestID
	}
	report.JobIDs, err = importJobs(flows, db, now)
	if err != nil {
		return Report{}, err
	}
	report.Jobs = len(report.JobIDs)
	report.TokenIDs, err = importTokens(ledger, db, now)
	if err != nil {
		return Report{}, err
	}
	report.Tokens = len(report.TokenIDs)
	report.Events, err = importEvents(engine, db)
	if err != nil {
		return Report{}, err
	}
	stored := storedReceipt{Report: report, Files: files}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return Report{}, err
	}
	ok, err := engine.CompareAndSwap(
		[]dataengine.Condition{{Key: []byte(receiptKey), Absent: true}},
		[]dataengine.Mutation{{Key: []byte(receiptKey), Value: encoded}},
	)
	if err != nil {
		return Report{}, err
	}
	if !ok {
		if old, found, err := readReceipt(engine); err != nil {
			return Report{}, err
		} else if found && old.SourceSHA256 == sum {
			old.Replayed = true
			return old.Report, nil
		}
		return Report{}, errors.New("迁移回执已存在且来源不同，拒绝覆盖")
	}
	return report, nil
}

// Rollback removes the live index, held jobs and migrated token rows
// recorded by the receipt. Audio files and the cold archive stay.
func Rollback(engine *dataengine.Engine, flows *workflow.Repository, ledger *tokenledger.Ledger) error {
	stored, ok, err := readReceipt(engine)
	if err != nil || !ok {
		return err
	}
	for _, file := range stored.Files {
		if err := audiorelease.RemoveIndexed(engine, file.RequestID, file.RootID, file.Track); err != nil {
			return err
		}
	}
	for _, id := range stored.JobIDs {
		if err := flows.RemoveHeld(id); err != nil {
			return err
		}
	}
	bySite := map[string][]string{}
	for _, id := range stored.TokenIDs {
		site, request, ok := strings.Cut(id, "/")
		if ok {
			bySite[site] = append(bySite[site], request)
		}
	}
	for site, ids := range bySite {
		if err := ledger.RemoveImported(site, ids); err != nil {
			return err
		}
	}
	raw, found, err := func() ([]byte, bool, error) {
		var raw []byte
		var found bool
		err := engine.View(func(v *dataengine.View) error {
			var err error
			raw, found, err = v.Get([]byte(receiptKey))
			return err
		})
		return raw, found, err
	}()
	if err != nil || !found {
		return err
	}
	_, err = engine.CompareAndSwap(
		[]dataengine.Condition{{Key: []byte(receiptKey), Value: raw}},
		[]dataengine.Mutation{
			{Key: []byte(receiptKey), Delete: true},
			{Key: []byte(eventsKey), Delete: true},
		},
	)
	return err
}

func importLibrary(engine *dataengine.Engine, db *sql.DB, libraryRoot string) ([]libraryKey, int, error) {
	if !tableExists(db, "library_files") {
		return nil, 0, nil
	}
	rows, err := db.Query(`SELECT payload FROM library_files`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []libraryKey
	seenRoot := map[string]int{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, 0, err
		}
		var row map[string]any
		if json.Unmarshal([]byte(payload), &row) != nil {
			continue
		}
		if str(row, "location_state") != "" && str(row, "location_state") != "library" {
			continue
		}
		path := str(row, "path")
		if path == "" || !filepath.IsAbs(path) {
			continue
		}
		artist := firstNonEmpty(str(row, "albumartist"), str(row, "artist"), "未知艺人")
		album := firstNonEmpty(str(row, "album"), "未知专辑")
		root := safeID("alb-", artist+"-"+album, 80)
		track := intFrom(row["tracknumber"])
		if track < 1 {
			track = intFrom(row["track"])
		}
		if track < 1 {
			seenRoot[root]++
			track = seenRoot[root]
		}
		if track > 999 {
			continue
		}
		request := safeID("file-", path, 80)
		format := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
		var ok bool
		for attempt := 0; attempt < 20 && track <= 999; attempt++ {
			ok, err = audiorelease.IndexExisting(engine, audiorelease.Existing{
				RequestID: request, RootID: root, Track: track, Path: path, Library: libraryRoot, Format: format, SourceName: "migrated",
			})
			if err != nil {
				return nil, 0, err
			}
			if ok {
				break
			}
			track++
		}
		if ok {
			out = append(out, libraryKey{RequestID: request, RootID: root, Track: track})
		}
	}
	return out, len(out), rows.Err()
}

func importJobs(flows *workflow.Repository, db *sql.DB, now time.Time) ([]string, error) {
	if !tableExists(db, "evidence") {
		return nil, nil
	}
	rows, err := db.Query(`SELECT id, payload FROM evidence WHERE id LIKE 'workflow:%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, err
		}
		var row map[string]any
		if json.Unmarshal([]byte(payload), &row) != nil {
			continue
		}
		if hidden(row["hidden"]) {
			continue
		}
		stage := str(row, "stage")
		switch stage {
		case "completed", "cancelled", "skipped", "failed", "restored":
			continue
		}
		rawID := strings.TrimPrefix(id, "workflow:")
		if rawID == "" {
			rawID = str(row, "job_id")
		}
		jobID := fitID(rawID, 120)
		scene := str(row, "scene")
		switch scene {
		case "playlist", "complete", "upgrade", "fill", "expand":
		default:
			scene = "complete"
		}
		label := firstNonEmpty(str(row, "label"), str(row, "query"), jobID)
		if len(label) > 500 {
			label = label[:500]
		}
		message := "从旧版迁入，原阶段 " + firstNonEmpty(stage, "未知") + "。尚未继续下载或扣费，请核对后再处理。"
		if old := str(row, "message"); old != "" && !strings.Contains(strings.ToLower(old), "cookie") {
			message += " 旧说明：" + trim(old, 240)
		}
		job := workflow.Job{
			ID: jobID, BatchID: jobID, RootID: jobID, Scene: scene, Kind: "album",
			TaskType: "album", TaskGroup: "process", Label: label, Message: message,
			ReviewKind: "interrupted", ErrorClass: "migrated",
			Revision: 1, CreatedNS: now.UnixNano(), UpdatedNS: now.UnixNano(), StageStartedNS: now.UnixNano(),
			Paused: hidden(row["paused"]),
		}
		created, err := flows.ImportHeld(job)
		if err != nil {
			return nil, err
		}
		if created {
			ids = append(ids, jobID)
		}
	}
	return ids, rows.Err()
}

func importTokens(ledger *tokenledger.Ledger, db *sql.DB, now time.Time) ([]string, error) {
	if !tableExists(db, "token_reservations") {
		return nil, nil
	}
	rows, err := db.Query(`SELECT site, resource_id, cost, state, id FROM token_reservations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var site, resource, state, id string
		var cost int
		if err := rows.Scan(&site, &resource, &cost, &state, &id); err != nil {
			return nil, err
		}
		if site != "dicmusic" && site != "orpheus" {
			continue
		}
		request := fitID(id, 60)
		resource = fitID(resource, 60)
		result, err := ledger.ImportHistorical(now, site, resource, request, strings.ToUpper(state), cost)
		if err != nil {
			return nil, err
		}
		if result.LedgerRecorded && (result.State == tokenledger.StateRequestUnknown || result.State == tokenledger.StateConfirmed) {
			ids = append(ids, site+"/"+request)
		}
	}
	return ids, rows.Err()
}

func importEvents(engine *dataengine.Engine, db *sql.DB) (int, error) {
	if !tableExists(db, "events") {
		return 0, nil
	}
	rows, err := db.Query(`SELECT at, phase, COALESCE(task_id,''), payload FROM events ORDER BY seq DESC LIMIT 200`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type event struct {
		At      string `json:"at"`
		Phase   string `json:"phase"`
		TaskID  string `json:"task_id"`
		Message string `json:"message"`
	}
	var events []event
	for rows.Next() {
		var item event
		var payload string
		if err := rows.Scan(&item.At, &item.Phase, &item.TaskID, &payload); err != nil {
			return 0, err
		}
		item.Message = trim(scrub(payload), 240)
		events = append(events, item)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		return 0, err
	}
	_, err = engine.CompareAndSwap(
		[]dataengine.Condition{{Key: []byte(eventsKey), Absent: true}},
		[]dataengine.Mutation{{Key: []byte(eventsKey), Value: encoded}},
	)
	return len(events), err
}

func libraryDir(db *sql.DB) (string, error) {
	if !tableExists(db, "config_versions") {
		return "", nil
	}
	var payload string
	err := db.QueryRow(`SELECT payload FROM config_versions ORDER BY CAST(id AS INTEGER) DESC LIMIT 1`).Scan(&payload)
	if err != nil {
		return "", nil
	}
	var doc struct {
		Values struct {
			Paths struct {
				Library string `json:"library"`
			} `json:"paths"`
		} `json:"values"`
	}
	if json.Unmarshal([]byte(payload), &doc) != nil {
		return "", nil
	}
	return doc.Values.Paths.Library, nil
}

func readReceipt(engine *dataengine.Engine) (storedReceipt, bool, error) {
	var raw []byte
	var found bool
	err := engine.View(func(v *dataengine.View) error {
		var err error
		raw, found, err = v.Get([]byte(receiptKey))
		return err
	})
	if err != nil || !found {
		return storedReceipt{}, false, err
	}
	var stored storedReceipt
	if json.Unmarshal(raw, &stored) != nil {
		return storedReceipt{}, false, dataengine.ErrInvalidRequest
	}
	return stored, true, nil
}

// Read returns the receipt for the UI. found is false when nothing was migrated.
func Read(engine *dataengine.Engine) (Report, bool, error) {
	stored, found, err := readReceipt(engine)
	return stored.Report, found, err
}

func openCopy(source string) (*sql.DB, func(), error) {
	dir, err := os.MkdirTemp("", "musicops-live-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	copyPath := filepath.Join(dir, "source.sqlite")
	in, err := os.Open(source)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	out, err := os.OpenFile(copyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		in.Close()
		cleanup()
		return nil, nil, err
	}
	_, err = io.Copy(out, in)
	in.Close()
	out.Close()
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(copyPath)+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return db, func() { _ = db.Close(); cleanup() }, nil
}

// Events returns the scrubbed old events stored by Activate.
func Events(engine *dataengine.Engine) ([]map[string]string, error) {
	if engine == nil {
		return nil, dataengine.ErrInvalidRequest
	}
	var raw []byte
	var found bool
	err := engine.View(func(v *dataengine.View) error {
		var err error
		raw, found, err = v.Get([]byte(eventsKey))
		return err
	})
	if err != nil || !found {
		return nil, err
	}
	var events []map[string]string
	if json.Unmarshal(raw, &events) != nil {
		return nil, dataengine.ErrInvalidRequest
	}
	return events, nil
}

func tableExists(db *sql.DB, name string) bool {
	var found string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&found)
	return err == nil
}

func fileSHA(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func fitID(raw string, limit int) string {
	if raw != "" && len(raw) <= limit && safeID("", raw, limit) == raw {
		return raw
	}
	return safeID("m-", raw, limit)
}

func safeID(prefix, raw string, limit int) string {
	var b strings.Builder
	b.WriteString(prefix)
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == ':':
			b.WriteRune(r)
		}
	}
	if b.Len() <= len(prefix) {
		sum := sha256.Sum256([]byte(raw))
		b.WriteString(hex.EncodeToString(sum[:6]))
	}
	out := b.String()
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func str(row map[string]any, key string) string {
	value, ok := row[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return ""
	}
}

func intFrom(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(typed))
		return n
	default:
		return 0
	}
}

func hidden(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case float64:
		return typed != 0
	default:
		return false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func trim(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func scrub(value string) string {
	lower := strings.ToLower(value)
	for _, word := range []string{"cookie", "authkey", "torrent_pass", "password", "secret", "token"} {
		if strings.Contains(lower, word) {
			return "（已省略可能含凭据的内容）"
		}
	}
	return value
}
