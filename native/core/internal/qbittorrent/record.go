package qbittorrent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// RecordState is the lifecycle state of a download task record.
type RecordState string

const (
	// StatePrecheck is a record that has not started an add yet.
	StatePrecheck RecordState = "PRECHECK"
	// StateAddIntent is a record whose creation intent is persisted but
	// whose add has not been confirmed by the client.
	StateAddIntent RecordState = "ADD_INTENT"
	// StateAddUnknown is a record whose add was sent but not confirmed.
	StateAddUnknown RecordState = "ADD_UNKNOWN"
	// StateStopped is a confirmed stopped torrent.
	StateStopped RecordState = "STOPPED"
	// StateStartRequested is a record with a start request in flight.
	StateStartRequested RecordState = "START_REQUESTED"
	// StateDownloading is a confirmed downloading torrent.
	StateDownloading RecordState = "DOWNLOADING"
	// StateComplete is a fully downloaded torrent.
	StateComplete RecordState = "COMPLETE"
)

// Record is one MusicOps download task as owned by qBittorrent. The caller
// owns persistence; this package only reads and mutates the in-memory copy
// passed to it.
type Record struct {
	TaskID           string         `json:"task_id"`
	ClientID         string         `json:"client_id"`
	InfoHashV1       string         `json:"infohash_v1,omitempty"`
	InfoHashV2       string         `json:"infohash_v2,omitempty"`
	SourcePath       string         `json:"source_path"`
	TaskTag          string         `json:"task_tag"`
	LegacyTaskTag    string         `json:"legacy_task_tag,omitempty"`
	Category         string         `json:"category"`
	MetainfoSHA256   string         `json:"metainfo_sha256,omitempty"`
	Files            []MetainfoFile `json:"files,omitempty"`
	CreationIntent   bool           `json:"creation_intent"`
	State            RecordState    `json:"state"`
	StartAttempts    int            `json:"start_attempts"`
	StartRequestedAt time.Time      `json:"start_requested_at,omitempty"`
	AddHTTPStatus    int            `json:"add_http_status,omitempty"`
}

var taskIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,120}$`)

func categoryOrDefault(category string) string {
	if category == "" {
		return DefaultCategory
	}
	return category
}

func taskTagOrDefault(tag string) string {
	if tag == "" {
		return ProjectTag
	}
	return tag
}

// NewRecord builds a record for taskID under sourceRoot. The source path is
// sourceRoot/taskID; creation intent starts false and must be set true by
// the caller once the intent is persisted, before AddStopped is called.
func NewRecord(taskID, sourceRoot, category string) (Record, error) {
	if !taskIDPattern.MatchString(taskID) {
		return Record{}, ErrInvalidRequest
	}
	if sourceRoot == "" || !filepath.IsAbs(sourceRoot) {
		return Record{}, ErrInvalidRequest
	}
	return Record{
		TaskID:     taskID,
		SourcePath: filepath.Join(sourceRoot, taskID),
		TaskTag:    ProjectTag,
		Category:   categoryOrDefault(category),
		State:      StatePrecheck,
	}, nil
}

// FreeEvidence is a confirmed-free download justification bound to one
// infohash.
type FreeEvidence struct {
	Confirmed bool   `json:"confirmed"`
	InfoHash  string `json:"infohash"`
	// Free is nil when the source did not state it; an explicit false means
	// the evidence is non-free.
	Free *bool `json:"free,omitempty"`
}

// FreePtr returns a *bool for a FreeEvidence literal.
func FreePtr(free bool) *bool { return &free }

// FreeEvidenceMatch reports whether evidence authorises a download of meta.
// The evidence must be confirmed and its infohash must be one of the
// metainfo hashes. A non-nil Free=false never matches here: the Go core has
// no non-free consent store (the Python client consults one), so non-free
// consent must be established by the caller through the consent flow
// outside this package and is honestly not claimed here.
func FreeEvidenceMatch(evidence FreeEvidence, meta Metainfo) bool {
	if !evidence.Confirmed {
		return false
	}
	hash := strings.ToLower(strings.TrimSpace(evidence.InfoHash))
	if hash == "" {
		return false
	}
	if hash != strings.ToLower(meta.InfoHashV1) && hash != strings.ToLower(meta.InfoHashV2) {
		return false
	}
	if evidence.Free != nil && !*evidence.Free {
		return false
	}
	return true
}

func parseTags(tags string) map[string]bool {
	out := map[string]bool{}
	for _, tag := range strings.Split(tags, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			out[tag] = true
		}
	}
	return out
}

func resolvePath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	// The path itself may not exist yet (a download path the client has
	// not created, for example): resolve the nearest existing ancestor
	// and append the remainder lexically, so a symlinked parent is
	// spelled the same way on both sides of the comparison. Returning
	// the unresolved spelling here used to fail ownership on macOS,
	// where the state directory sits under /var (a symlink to
	// /private/var) and the resolved base never matched it.
	ancestor := abs
	var suffix []string
	for {
		if _, err := os.Lstat(ancestor); err == nil {
			break
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return abs
		}
		suffix = append([]string{filepath.Base(ancestor)}, suffix...)
		ancestor = parent
	}
	if resolved, err := filepath.EvalSymlinks(ancestor); err == nil {
		for _, part := range suffix {
			resolved = filepath.Join(resolved, part)
		}
		return resolved
	}
	return abs
}

// boundPath reports whether path is absolute and equal to (exact) or inside
// expected.
func boundPath(path, expected string, exact bool) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	actual := resolvePath(path)
	base := resolvePath(expected)
	if exact {
		return actual == base
	}
	return actual == base || strings.HasPrefix(actual, base+string(filepath.Separator))
}

var stoppedStatesV5 = map[string]bool{"stoppedDL": true, "stoppedUP": true}
var stoppedStatesV4 = map[string]bool{"pausedDL": true, "pausedUP": true}

// CheckOwnership verifies that a client row belongs to a record. It returns
// "" on success and a plain reason string on failure, mirroring
// QbtClient._check_ownership. A zero Version accepts either stopped-state
// set, for callers that have not fetched the server version yet.
func CheckOwnership(row *TorrentRow, rec Record, requireStopped bool, ver Version) string {
	if row == nil {
		return "torrent not found in client"
	}
	if !rec.CreationIntent {
		return "no creation record for this task"
	}
	tags := parseTags(row.Tags)
	if !tags[taskTagOrDefault(rec.TaskTag)] && !tags[ProjectTag] {
		return "torrent has neither the task tag nor the MusicOps tag"
	}
	if row.Category != categoryOrDefault(rec.Category) {
		return "torrent category does not match the task category"
	}
	if !boundPath(row.SavePath, rec.SourcePath, true) {
		return "save path does not equal the task source path"
	}
	if row.AutoTMM == nil || *row.AutoTMM {
		return "auto_tmm is unknown or enabled"
	}
	if row.ContentPath != "" && !boundPath(row.ContentPath, rec.SourcePath, false) {
		return "content path escapes the task source path"
	}
	if row.DownloadPath != "" && !boundPath(row.DownloadPath, rec.SourcePath, false) {
		return "download path escapes the task source path"
	}
	if row.DownloadPath == "" {
		return "download path not returned by client"
	}
	if requireStopped {
		stopped := stoppedStatesV5
		if !ver.IsZero() && !ver.AtLeast(5, 0, 0) {
			stopped = stoppedStatesV4
		} else if ver.IsZero() {
			// Unknown version: accept either stopped set rather than
			// rejecting a torrent the client did stop.
			if stoppedStatesV5[row.State] || stoppedStatesV4[row.State] {
				return ""
			}
			return "torrent is not in a stopped state"
		}
		if !stopped[row.State] {
			return "torrent is not in a stopped state"
		}
	}
	return ""
}

// CheckOwnership verifies that a client row belongs to a record, using the
// server version this client has fetched (or either stopped-state set when
// the version is not known yet).
func (c *Client) CheckOwnership(row *TorrentRow, rec Record, requireStopped bool) string {
	return CheckOwnership(row, rec, requireStopped, c.versionSnapshot())
}

// NormalizeTags renames a task's generated tag to the project tag. It adds
// the "MusicOps" tag to an owned task, removes the legacy per-task tag, and
// deletes a tag globally only after confirming no torrent still uses it.
// It returns whether anything changed. The record is mutated in place; the
// caller owns persistence.
func (c *Client) NormalizeTags(ctx context.Context, rec *Record) (bool, error) {
	if rec == nil || rec.ClientID == "" {
		return false, ErrInvalidRequest
	}
	row, err := c.Query(ctx, *rec)
	if err != nil {
		return false, err
	}
	if row == nil {
		return false, &OwnershipError{Reason: "torrent not found in client"}
	}
	if reason := c.CheckOwnership(row, *rec, false); reason != "" {
		return false, &OwnershipError{Reason: reason}
	}
	taskTag := taskTagOrDefault(rec.TaskTag)
	old := rec.LegacyTaskTag
	if old == "" {
		old = taskTag
	}
	changed := false
	if taskTag != ProjectTag {
		if err := c.AddTags(ctx, rec.ClientID, ProjectTag); err != nil {
			return false, err
		}
		row, err = c.Query(ctx, *rec)
		if err != nil {
			return false, err
		}
		if row == nil {
			return false, &OwnershipError{Reason: "torrent not found in client"}
		}
		proposed := *rec
		proposed.TaskTag = ProjectTag
		if reason := c.CheckOwnership(row, proposed, false); reason != "" {
			return false, &OwnershipError{Reason: reason}
		}
		rec.TaskTag = ProjectTag
		rec.LegacyTaskTag = old
		changed = true
	}
	if old != ProjectTag {
		if parseTags(row.Tags)[old] {
			if err := c.RemoveTags(ctx, rec.ClientID, old); err != nil {
				return changed, err
			}
		}
		// A tag is deleted globally only after it is confirmed unused.
		rows, err := c.InfoByTag(ctx, old)
		if err != nil {
			return changed, err
		}
		if len(rows) == 0 {
			if err := c.DeleteTags(ctx, old); err != nil {
				return changed, err
			}
		}
		rec.LegacyTaskTag = ""
		changed = true
	}
	return changed, nil
}

// PollOutcome is the classification of one poll of a download task.
type PollOutcome string

const (
	// OutcomeWaiting covers checking / moving states.
	OutcomeWaiting PollOutcome = "waiting"
	// OutcomeDownloading covers active, queued and stalled downloads, and
	// finished states whose file manifest is not yet fully verified.
	OutcomeDownloading PollOutcome = "downloading"
	// OutcomeStopped is a stopped or paused torrent with progress < 1.
	OutcomeStopped PollOutcome = "stopped"
	// OutcomeComplete is a finished torrent whose manifest is fully verified.
	OutcomeComplete PollOutcome = "complete"
	// OutcomeBlocked covers error / missingFiles / unknown states.
	OutcomeBlocked PollOutcome = "blocked"
)

// PollResult is the outcome of one Poll call.
type PollResult struct {
	Outcome     PollOutcome
	ClientState string
	Complete    bool
	Reason      string
}

var waitingStates = map[string]bool{
	"checkingUP": true, "checkingDL": true, "checkingResumeData": true, "moving": true,
}

var blockedStates = map[string]bool{
	"error": true, "missingFiles": true, "unknown": true,
}

var finishedStates = map[string]bool{
	"uploading": true, "stalledUP": true, "queuedUP": true,
	"forcedUP": true, "stoppedUP": true, "pausedUP": true,
}

// ClassifyState maps a raw client state (and progress) to a PollOutcome
// without touching the file manifest. Completion is deliberately not
// decided here: Poll upgrades a finished state to complete only after the
// manifest check.
func ClassifyState(state string, progress float64) PollOutcome {
	switch {
	case waitingStates[state]:
		return OutcomeWaiting
	case blockedStates[state]:
		return OutcomeBlocked
	case state == "stoppedDL" || state == "pausedDL":
		if progress < 1 {
			return OutcomeStopped
		}
		return OutcomeDownloading
	default:
		return OutcomeDownloading
	}
}

// Poll classifies one download task. Completion requires a finished state
// (uploading/stalledUP/queuedUP/forcedUP/stoppedUP/pausedUP) AND progress==1
// AND the file manifest covering every expected non-padding file with
// progress==1 and priority!=0. expectedFiles overrides the record's file
// list when non-nil. The record state is updated in place.
func (c *Client) Poll(ctx context.Context, rec *Record, expectedFiles []string) (PollResult, error) {
	if rec == nil || rec.ClientID == "" {
		return PollResult{}, ErrInvalidRequest
	}
	if _, err := c.Capabilities(ctx); err != nil {
		return PollResult{}, err
	}
	row, err := c.Query(ctx, *rec)
	if err != nil {
		return PollResult{}, err
	}
	if row == nil {
		return PollResult{}, &OwnershipError{Reason: "torrent not found in client"}
	}
	if reason := c.CheckOwnership(row, *rec, false); reason != "" {
		return PollResult{}, &OwnershipError{Reason: reason}
	}
	if rec.TaskTag != ProjectTag || rec.LegacyTaskTag != "" {
		// Tag maintenance never retries an add/start or changes seeding.
		_, _ = c.NormalizeTags(ctx, rec)
	}
	state := row.State
	switch ClassifyState(state, row.Progress) {
	case OutcomeWaiting:
		return PollResult{Outcome: OutcomeWaiting, ClientState: state}, nil
	case OutcomeBlocked:
		return PollResult{Outcome: OutcomeBlocked, ClientState: state, Reason: "client reports an error or missing files"}, nil
	case OutcomeStopped:
		return PollResult{Outcome: OutcomeStopped, ClientState: state, Reason: "client task stopped; resume it to continue"}, nil
	}
	if !finishedStates[state] || row.Progress != 1 {
		rec.State = StateDownloading
		rec.StartAttempts = 0
		return PollResult{Outcome: OutcomeDownloading, ClientState: state}, nil
	}
	files, err := c.Files(ctx, rec.ClientID)
	if err != nil {
		return PollResult{}, err
	}
	expected := expectedFiles
	if expected == nil {
		for _, f := range rec.Files {
			if !f.Padding {
				expected = append(expected, f.Path)
			}
		}
	}
	if len(expected) == 0 {
		return PollResult{Outcome: OutcomeDownloading, ClientState: state, Reason: "no expected files to verify"}, nil
	}
	actual := map[string]TorrentFile{}
	for _, file := range files {
		if _, dup := actual[file.Name]; dup {
			return PollResult{Outcome: OutcomeDownloading, ClientState: state, Reason: "duplicate file in client manifest"}, nil
		}
		actual[file.Name] = file
	}
	for _, path := range expected {
		entry, ok := actual[path]
		if !ok || entry.Priority == 0 || entry.Progress != 1 {
			return PollResult{Outcome: OutcomeDownloading, ClientState: state, Reason: "expected file missing, skipped or incomplete"}, nil
		}
	}
	rec.State = StateComplete
	return PollResult{Outcome: OutcomeComplete, ClientState: state, Complete: true}, nil
}

// ReconcileAdd confirms an add that was sent but not yet verified. It
// returns the record state and a nil error for known outcomes (waiting,
// stopped, downloading, add-unknown); ownership and transport failures are
// returned as errors.
func (c *Client) ReconcileAdd(ctx context.Context, rec *Record) (RecordState, error) {
	if rec == nil || rec.ClientID == "" {
		return "", ErrInvalidRequest
	}
	if _, err := c.Capabilities(ctx); err != nil {
		return "", err
	}
	row, err := c.Query(ctx, *rec)
	if err != nil {
		return "", err
	}
	if row == nil {
		return StateAddUnknown, nil
	}
	waiting := waitingStates[row.State]
	requireStopped := !waiting && rec.StartRequestedAt.IsZero() &&
		rec.State != StateStartRequested && rec.State != StateDownloading && rec.State != StateComplete
	if reason := c.CheckOwnership(row, *rec, requireStopped); reason != "" {
		return "", &OwnershipError{Reason: reason}
	}
	if rec.TaskTag != ProjectTag || rec.LegacyTaskTag != "" {
		_, _ = c.NormalizeTags(ctx, rec)
	}
	state := row.State
	switch {
	case waitingStates[state]:
		return rec.State, nil
	case blockedStates[state]:
		return "", errors.New("qbittorrent: client reports a file or storage error")
	case stoppedStatesV5[state] || stoppedStatesV4[state]:
		rec.State = StateStopped
		return StateStopped, nil
	case state == "downloading" || state == "stalledDL" || state == "queuedDL" ||
		state == "forcedDL" || state == "metaDL" || state == "forcedMetaDL" ||
		state == "uploading" || state == "stalledUP" || state == "queuedUP" || state == "forcedUP":
		rec.State = StateDownloading
		rec.StartAttempts = 0
		return StateDownloading, nil
	default:
		return rec.State, nil
	}
}

// AddStopped validates free evidence, records creation intent and adds the
// torrent stopped, then reconciles the add. The record is mutated in place;
// the caller owns persistence. It returns the reconciled record state.
func (c *Client) AddStopped(ctx context.Context, rec *Record, torrent []byte, evidence FreeEvidence) (RecordState, error) {
	if rec == nil {
		return "", ErrInvalidRequest
	}
	if _, err := c.Capabilities(ctx); err != nil {
		return "", err
	}
	if !taskIDPattern.MatchString(rec.TaskID) {
		return "", ErrInvalidRequest
	}
	meta, err := ParseMetainfo(torrent)
	if err != nil {
		return "", err
	}
	if !FreeEvidenceMatch(evidence, meta) {
		return "", ErrFreeEvidenceMismatch
	}
	if rec.MetainfoSHA256 != "" && rec.MetainfoSHA256 != meta.MetainfoSHA256 {
		return "", ErrRecordConflict
	}
	if rec.MetainfoSHA256 == "" {
		rec.InfoHashV1 = meta.InfoHashV1
		rec.InfoHashV2 = meta.InfoHashV2
		if meta.InfoHashV1 != "" {
			rec.ClientID = meta.InfoHashV1
		} else {
			rec.ClientID = meta.InfoHashV2[:40]
		}
		rec.MetainfoSHA256 = meta.MetainfoSHA256
		rec.Files = meta.Files
	}
	existing, err := c.Query(ctx, *rec)
	if err != nil {
		return "", err
	}
	if existing != nil {
		return "", ErrAlreadyExists
	}
	rec.CreationIntent = true
	rec.State = StateAddIntent
	if err := c.Add(ctx, AddRequest{
		Torrent:  torrent,
		SavePath: rec.SourcePath,
		Category: categoryOrDefault(rec.Category),
		Tags:     taskTagOrDefault(rec.TaskTag),
	}); err != nil {
		rec.State = StateAddUnknown
		return "", err
	}
	rec.State = StateAddUnknown
	return c.ReconcileAdd(ctx, rec)
}

// StartDownload starts a stopped task after re-checking ownership, stopped
// state and free evidence. It allows three attempts and enforces a ten
// second cooldown between attempts.
func (c *Client) StartDownload(ctx context.Context, rec *Record, evidence FreeEvidence) error {
	if rec == nil {
		return ErrInvalidRequest
	}
	if _, err := c.Capabilities(ctx); err != nil {
		return err
	}
	if rec.State != StateStopped {
		return errors.New("qbittorrent: task is not stopped")
	}
	meta := Metainfo{InfoHashV1: rec.InfoHashV1, InfoHashV2: rec.InfoHashV2}
	if !FreeEvidenceMatch(evidence, meta) {
		return ErrFreeEvidenceMismatch
	}
	row, err := c.Query(ctx, *rec)
	if err != nil {
		return err
	}
	if row == nil {
		return &OwnershipError{Reason: "torrent not found in client"}
	}
	if reason := c.CheckOwnership(row, *rec, true); reason != "" {
		return &OwnershipError{Reason: reason}
	}
	if rec.StartAttempts >= 3 {
		return ErrStartAttemptsExhausted
	}
	if rec.StartAttempts > 0 && time.Since(rec.StartRequestedAt) < 10*time.Second {
		return ErrStartCooldown
	}
	rec.State = StateStartRequested
	rec.StartAttempts++
	rec.StartRequestedAt = time.Now()
	if err := c.Start(ctx, rec.ClientID); err != nil {
		return err
	}
	return nil
}
