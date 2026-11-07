package sqlitemigrate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

const domainManifestKey = "domain/meta/manifest"
const domainCursorKey = "domain/meta/cursor"

// DomainReport is the typed projection of a verified archive. It is not a live profile switch.
type DomainReport struct {
	SourceSHA256   string         `json:"source_sha256"`
	Tables         map[string]int `json:"tables"`
	Rows           int            `json:"rows"`
	Decoded        int            `json:"decoded"`
	Opaque         int            `json:"opaque"`
	CredentialRefs int            `json:"credential_refs"`
	State          string         `json:"state"`
	Resumed        bool           `json:"resumed"`
	Replayed       bool           `json:"replayed"`
	Verified       bool           `json:"verified"`
}

// Document is one historical record in the native domain. CredentialRefs are references, not secret values.
type Document struct {
	Table          string          `json:"table"`
	PK             string          `json:"pk"`
	Kind           string          `json:"kind"`
	SourceSHA256   string          `json:"source_sha256"`
	Decoded        bool            `json:"decoded"`
	ID             string          `json:"id,omitempty"`
	UpdatedAt      string          `json:"updated_at,omitempty"`
	Site           string          `json:"site,omitempty"`
	ResourceID     string          `json:"resource_id,omitempty"`
	State          string          `json:"state,omitempty"`
	Phase          string          `json:"phase,omitempty"`
	TaskID         string          `json:"task_id,omitempty"`
	TxID           string          `json:"tx_id,omitempty"`
	RootKey        string          `json:"root_key,omitempty"`
	AlbumID        string          `json:"album_id,omitempty"`
	Cost           *int            `json:"cost,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	CredentialRefs []string        `json:"credential_refs,omitempty"`
	Opaque         string          `json:"opaque,omitempty"`
	SHA256         string          `json:"sha256"`
}

type domainManifest struct {
	State          string         `json:"state"`
	SourceSHA256   string         `json:"source_sha256"`
	Tables         map[string]int `json:"tables"`
	Rows           int            `json:"rows"`
	Decoded        int            `json:"decoded"`
	Opaque         int            `json:"opaque"`
	CredentialRefs int            `json:"credential_refs"`
}

type domainCursor struct {
	Key            string         `json:"key"`
	Rows           int            `json:"rows"`
	Tables         map[string]int `json:"tables"`
	Decoded        int            `json:"decoded"`
	Opaque         int            `json:"opaque"`
	CredentialRefs int            `json:"credential_refs"`
}

// Project turns a verified raw archive into typed task, event, token, library and history documents.
// stopAfter is zero outside tests. It does not write the SQLite file.
func Project(engine *dataengine.Engine, stopAfter int) (DomainReport, error) {
	if engine == nil || stopAfter < 0 {
		return DomainReport{}, dataengine.ErrInvalidRequest
	}
	archive, err := readManifest(engine)
	if err != nil {
		return DomainReport{}, err
	}
	if archive == nil || archive.State != "verified" {
		return DomainReport{}, ErrSchema
	}
	current, err := readDomainManifest(engine)
	if err != nil {
		return DomainReport{}, err
	}
	if current != nil && current.SourceSHA256 != archive.SourceSHA256 {
		return DomainReport{}, ErrConflict
	}
	if current != nil && current.State == "verified" {
		report, err := verifyDomain(engine, *current, archive.SourceSHA256)
		if err != nil {
			return DomainReport{}, err
		}
		report.Replayed = true
		return report, nil
	}
	cursor, err := readDomainCursor(engine)
	if err != nil {
		return DomainReport{}, err
	}
	resumed := cursor.Rows > 0
	if cursor.Tables == nil {
		cursor.Tables = map[string]int{}
	}
	written := 0
	for {
		var page dataengine.Page
		err = engine.View(func(v *dataengine.View) error {
			var scanErr error
			page, scanErr = v.Scan([]byte("archive/row/"), []byte(cursor.Key), 50)
			return scanErr
		})
		if err != nil {
			return DomainReport{}, err
		}
		if len(page.Items) == 0 {
			break
		}
		for _, item := range page.Items {
			var record Record
			if json.Unmarshal(item.Value, &record) != nil || record.Table == "" || record.PK == "" || record.SHA256 == "" {
				return DomainReport{}, ErrConflict
			}
			doc, err := projectRecord(record)
			if err != nil {
				return DomainReport{}, err
			}
			if err = putDocument(engine, doc, string(item.Key), cursor); err != nil {
				return DomainReport{}, err
			}
			cursor.Key = string(item.Key)
			cursor.Rows++
			cursor.Tables[record.Table]++
			if doc.Decoded {
				cursor.Decoded++
			} else {
				cursor.Opaque++
			}
			cursor.CredentialRefs += len(doc.CredentialRefs)
			written++
			if stopAfter > 0 && written >= stopAfter {
				return DomainReport{}, ErrPaused
			}
		}
		if len(page.Next) == 0 {
			break
		}
	}
	if cursor.Rows != archive.Rows || !sameCounts(cursor.Tables, archive.Tables) {
		return DomainReport{}, ErrConflict
	}
	manifest := domainManifest{
		State: "verified", SourceSHA256: archive.SourceSHA256, Tables: cursor.Tables, Rows: cursor.Rows,
		Decoded: cursor.Decoded, Opaque: cursor.Opaque, CredentialRefs: cursor.CredentialRefs,
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return DomainReport{}, err
	}
	ok, err := engine.CompareAndSwap([]dataengine.Condition{{Key: []byte(domainManifestKey), Absent: true}}, []dataengine.Mutation{{Key: []byte(domainManifestKey), Value: raw}})
	if err != nil {
		return DomainReport{}, err
	}
	if !ok {
		return Project(engine, 0)
	}
	report, err := verifyDomain(engine, manifest, archive.SourceSHA256)
	report.Resumed = resumed
	return report, err
}

// DomainTask returns one projected task by its historical id.
func DomainTask(engine *dataengine.Engine, id string) (Document, bool, error) {
	return domainByIndex(engine, "domain/task/"+id)
}

// DomainToken returns one projected token reservation.
func DomainToken(engine *dataengine.Engine, id string) (Document, bool, error) {
	return domainByIndex(engine, "domain/token/"+id)
}

// RollbackDomain deletes the typed projection and leaves the raw archive in place.
func RollbackDomain(engine *dataengine.Engine) (int, error) {
	if engine == nil {
		return 0, dataengine.ErrInvalidRequest
	}
	deleted := 0
	for _, prefix := range []string{"domain/doc/", "domain/task/", "domain/event/", "domain/token/", "domain/credential/", "domain/meta/"} {
		for {
			var page dataengine.Page
			err := engine.View(func(v *dataengine.View) error {
				var scanErr error
				page, scanErr = v.Scan([]byte(prefix), nil, 50)
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

func projectRecord(record Record) (Document, error) {
	fields := map[string]string{}
	for _, column := range record.Columns {
		value, ok := decodeEnc(column.Enc)
		if !ok {
			return Document{}, ErrSchema
		}
		fields[column.Name] = value
	}
	doc := Document{Table: record.Table, PK: record.PK, Kind: kindFor(record.Table), SourceSHA256: record.SHA256}
	doc.ID = first(fields, "id")
	doc.UpdatedAt = fields["updated_at"]
	doc.Site = fields["site"]
	doc.ResourceID = fields["resource_id"]
	doc.State = fields["state"]
	doc.Phase = fields["phase"]
	doc.TaskID = fields["task_id"]
	doc.TxID = fields["tx_id"]
	doc.RootKey = fields["root_key"]
	doc.AlbumID = fields["album_id"]
	if text, ok := fields["cost"]; ok && text != "" {
		cost, err := strconv.Atoi(text)
		if err != nil {
			return Document{}, ErrSchema
		}
		doc.Cost = &cost
	}
	if payload, ok := fields["payload"]; ok {
		compact := bytes.TrimSpace([]byte(payload))
		if json.Valid(compact) && (len(compact) == 0 || compact[0] == '{' || compact[0] == '[') {
			buf := bytes.Buffer{}
			if err := json.Compact(&buf, compact); err != nil {
				return Document{}, ErrSchema
			}
			doc.Payload = buf.Bytes()
			doc.Decoded = true
			var parsed any
			if json.Unmarshal(doc.Payload, &parsed) == nil {
				refs := []string{}
				collectRefs(parsed, &refs)
				sort.Strings(refs)
				doc.CredentialRefs = unique(refs)
			}
		} else {
			doc.Opaque = payload
		}
	} else if record.Table == "library_matches" {
		doc.Decoded = true
	}
	if doc.ID == "" {
		doc.ID = record.PK
	}
	doc.SHA256 = documentHash(doc)
	return doc, nil
}

func putDocument(engine *dataengine.Engine, doc Document, archiveKey string, cursor domainCursor) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	docKey := []byte("domain/doc/" + strings.TrimPrefix(archiveKey, "archive/row/"))
	conditions := []dataengine.Condition{{Key: docKey, Absent: true}}
	mutations := []dataengine.Mutation{{Key: docKey, Value: raw}}
	add := func(key string) {
		encoded := []byte(key)
		conditions = append(conditions, dataengine.Condition{Key: encoded, Absent: true})
		mutations = append(mutations, dataengine.Mutation{Key: encoded, Value: docKey})
	}
	switch doc.Kind {
	case "task":
		add("domain/task/" + doc.ID)
	case "event":
		add("domain/event/" + doc.PK)
	case "token":
		add("domain/token/" + doc.ID)
	}
	for _, ref := range doc.CredentialRefs {
		sum := sha256.Sum256([]byte(doc.Table + "\n" + doc.PK + "\n" + ref))
		refKey := "domain/credential/" + hex.EncodeToString(sum[:])
		body, err := json.Marshal(map[string]string{"ref": ref, "table": doc.Table, "pk": doc.PK})
		if err != nil {
			return err
		}
		encoded := []byte(refKey)
		conditions = append(conditions, dataengine.Condition{Key: encoded, Absent: true})
		mutations = append(mutations, dataengine.Mutation{Key: encoded, Value: body})
	}
	next := cursor
	next.Key = archiveKey
	next.Rows++
	if next.Tables == nil {
		next.Tables = map[string]int{}
	} else {
		copied := map[string]int{}
		for key, value := range next.Tables {
			copied[key] = value
		}
		next.Tables = copied
	}
	next.Tables[doc.Table]++
	if doc.Decoded {
		next.Decoded++
	} else {
		next.Opaque++
	}
	next.CredentialRefs += len(doc.CredentialRefs)
	cursorRaw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	previous, err := json.Marshal(cursor)
	if err != nil {
		return err
	}
	if cursor.Rows == 0 && cursor.Key == "" {
		conditions = append(conditions, dataengine.Condition{Key: []byte(domainCursorKey), Absent: true})
	} else {
		conditions = append(conditions, dataengine.Condition{Key: []byte(domainCursorKey), Value: previous})
	}
	mutations = append(mutations, dataengine.Mutation{Key: []byte(domainCursorKey), Value: cursorRaw})
	ok, err := engine.CompareAndSwap(conditions, mutations)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	var existing Document
	err = engine.View(func(v *dataengine.View) error {
		raw, found, err := v.Get(docKey)
		if err != nil || !found {
			if err == nil {
				err = ErrConflict
			}
			return err
		}
		if json.Unmarshal(raw, &existing) != nil {
			return ErrConflict
		}
		return nil
	})
	if err != nil {
		return err
	}
	if existing.SHA256 != doc.SHA256 {
		return ErrConflict
	}
	return nil
}

func verifyDomain(engine *dataengine.Engine, manifest domainManifest, source string) (DomainReport, error) {
	counts := map[string]int{}
	rows, decoded, opaque, refs := 0, 0, 0, 0
	after := []byte{}
	for {
		var page dataengine.Page
		err := engine.View(func(v *dataengine.View) error {
			var scanErr error
			page, scanErr = v.Scan([]byte("domain/doc/"), after, 50)
			return scanErr
		})
		if err != nil {
			return DomainReport{}, err
		}
		if len(page.Items) == 0 {
			break
		}
		for _, item := range page.Items {
			var doc Document
			if json.Unmarshal(item.Value, &doc) != nil || documentHash(doc) != doc.SHA256 {
				return DomainReport{}, ErrConflict
			}
			archiveKey := []byte("archive/row/" + strings.TrimPrefix(string(item.Key), "domain/doc/"))
			var record Record
			err = engine.View(func(v *dataengine.View) error {
				raw, found, err := v.Get(archiveKey)
				if err != nil || !found || json.Unmarshal(raw, &record) != nil {
					if err == nil {
						err = ErrConflict
					}
					return err
				}
				return nil
			})
			if err != nil || record.SHA256 != doc.SourceSHA256 || record.Table != doc.Table || record.PK != doc.PK {
				if err == nil {
					err = ErrConflict
				}
				return DomainReport{}, err
			}
			counts[doc.Table]++
			rows++
			if doc.Decoded {
				decoded++
			} else {
				opaque++
			}
			refs += len(doc.CredentialRefs)
		}
		if len(page.Next) == 0 {
			break
		}
		after = page.Next
	}
	if rows != manifest.Rows || decoded != manifest.Decoded || opaque != manifest.Opaque || refs != manifest.CredentialRefs || !sameCounts(counts, manifest.Tables) || manifest.SourceSHA256 != source {
		return DomainReport{}, ErrConflict
	}
	report := DomainReport{
		SourceSHA256: source, Tables: counts, Rows: rows, Decoded: decoded, Opaque: opaque, CredentialRefs: refs,
		State: "verified", Verified: true, Resumed: false,
	}
	return report, nil
}

func documentHash(doc Document) string {
	doc.SHA256 = ""
	raw, _ := json.Marshal(doc)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func kindFor(table string) string {
	switch table {
	case "tasks":
		return "task"
	case "events":
		return "event"
	case "token_reservations":
		return "token"
	case "transactions":
		return "transaction"
	case "download_tasks":
		return "download_task"
	case "library_albums":
		return "library_album"
	case "library_matches":
		return "library_match"
	case "evidence":
		return "evidence"
	default:
		return "record"
	}
}

func collectRefs(value any, refs *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if text, ok := child.(string); ok && text != "" && (strings.HasSuffix(key, "_ref") || strings.HasPrefix(text, "vault://")) {
				*refs = append(*refs, text)
			}
			collectRefs(child, refs)
		}
	case []any:
		for _, child := range typed {
			collectRefs(child, refs)
		}
	}
}

func decodeEnc(enc string) (string, bool) {
	if enc == "N" {
		return "", true
	}
	if len(enc) < 2 {
		return "", false
	}
	switch enc[0] {
	case 'T', 'I', 'F':
		return enc[1:], true
	default:
		return "", false
	}
}

func first(fields map[string]string, key string) string { return fields[key] }

func unique(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := []string{values[0]}
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func sameCounts(left, right map[string]int) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func domainByIndex(engine *dataengine.Engine, key string) (Document, bool, error) {
	var doc Document
	var found bool
	err := engine.View(func(v *dataengine.View) error {
		pointer, ok, err := v.Get([]byte(key))
		if err != nil || !ok {
			found = false
			return err
		}
		raw, ok, err := v.Get(pointer)
		if err != nil || !ok {
			found = false
			if err == nil {
				err = ErrConflict
			}
			return err
		}
		found = true
		if json.Unmarshal(raw, &doc) != nil || documentHash(doc) != doc.SHA256 {
			return ErrConflict
		}
		return nil
	})
	return doc, found, err
}

func readDomainManifest(engine *dataengine.Engine) (*domainManifest, error) {
	var manifest domainManifest
	var found bool
	err := engine.View(func(v *dataengine.View) error {
		raw, ok, err := v.Get([]byte(domainManifestKey))
		if err != nil || !ok {
			return err
		}
		found = true
		if json.Unmarshal(raw, &manifest) != nil {
			return ErrConflict
		}
		return nil
	})
	if err != nil || !found {
		return nil, err
	}
	return &manifest, nil
}

func readDomainCursor(engine *dataengine.Engine) (domainCursor, error) {
	cursor := domainCursor{Tables: map[string]int{}}
	err := engine.View(func(v *dataengine.View) error {
		raw, found, err := v.Get([]byte(domainCursorKey))
		if err != nil || !found {
			return err
		}
		if json.Unmarshal(raw, &cursor) != nil {
			return ErrConflict
		}
		return nil
	})
	if cursor.Tables == nil {
		cursor.Tables = map[string]int{}
	}
	return cursor, err
}
