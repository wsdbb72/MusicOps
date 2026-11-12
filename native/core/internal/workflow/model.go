// Package workflow is the durable album-workflow ledger.
// Stage changes, evidence, task completion and successor queue edges commit together.
// It does not search, download, publish or read a music library.
package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

var (
	ErrStale     = taskrepo.ErrStale
	ErrNotFound  = taskrepo.ErrNotFound
	ErrConflict  = taskrepo.ErrRequestConflict
	ErrIntegrity = taskrepo.ErrIntegrity
)

const (
	edgeQueueSearch  = "queue_search"
	edgeQueueProcess = "queue_process"
	edgeDefer        = "defer_search"
	edgeAdvance      = "advance"
	edgeTerminal     = "terminal"

	cmdPrepare  = "prepare_batch"
	cmdIdentify = "identify_release"
	cmdSnapshot = "snapshot_album"
	cmdSearch   = "search_release"
	cmdRepair   = "repair_album"
	cmdProcess  = "process_release"
	cmdPublish  = "publish_album"
	cmdDownload = "download_release"

	EdgeQueueSearch  = edgeQueueSearch
	EdgeQueueProcess = edgeQueueProcess
	EdgeDefer        = edgeDefer
	EdgeAdvance      = edgeAdvance
	EdgeTerminal     = edgeTerminal

	CommandIdentify = cmdIdentify
	CommandSnapshot = cmdSnapshot
	CommandSearch   = cmdSearch
	CommandProcess  = cmdProcess
	// CommandPublish is the only command whose effect exists outside the
	// search/acquire chain: the publish_album executor verifies a library
	// record before completing the job.
	CommandPublish = cmdPublish
	// CommandDownload runs the qBittorrent download for a process job whose
	// torrent receipt is already stored. It is created only by StartDownload,
	// never by the search/acquire chain.
	CommandDownload = cmdDownload
)

// Commands are durable queue names. The Go application registers identify,
// search, publish and download. Repair stays unregistered until that effect
// exists.
var Commands = []string{cmdPrepare, cmdIdentify, cmdSnapshot, cmdSearch, cmdRepair, cmdProcess, cmdPublish, cmdDownload}

var (
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,199}$`)
	sourcePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`)
	trackPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._@+-]{0,199}$`)
)

var scenes = map[string]bool{"playlist": true, "complete": true, "upgrade": true, "fill": true, "expand": true}
var stages = map[string]bool{
	"preparing": true, "identifying": true, "snapshotting": true, "metadata": true, "searching": true,
	"requesting": true, "downloading": true, "fallback": true, "validating": true, "publishing": true, "notifying": true,
	"needs_review": true, "completed": true, "failed": true, "cancelled": true, "skipped": true,
}
var reviewKinds = map[string]bool{
	"metadata": true, "candidate": true, "token": true, "error": true, "album_scope": true,
	"cover": true, "resources": true, "numbering": true, "interrupted": true, "search": true, "existing_album": true,
}
var evidenceKinds = map[string]bool{"api": true, "website": true, "catalog": true, "ai": true, "synthetic_fixture": true}

type Job struct {
	ID                  string `json:"job_id"`
	Kind                string `json:"job_kind"`
	Scene               string `json:"scene"`
	Stage               string `json:"stage"`
	TaskType            string `json:"task_type"`
	TaskGroup           string `json:"task_group"`
	BatchID             string `json:"batch_id"`
	RootID              string `json:"root_id"`
	ParentJobID         string `json:"parent_job_id,omitempty"`
	IdentificationJobID string `json:"identification_job_id,omitempty"`
	SearchJobID         string `json:"search_job_id,omitempty"`
	ProcessingJobID     string `json:"processing_job_id,omitempty"`
	ActiveTaskID        string `json:"active_task_id,omitempty"`
	Paused              bool   `json:"paused,omitempty"`
	CancelRequested     bool   `json:"cancel_requested,omitempty"`
	SourceTrackCount    uint64 `json:"source_track_count,omitempty"`
	SourceTrackDigest   string `json:"source_track_digest,omitempty"`
	LibraryFileCount    uint64 `json:"library_file_count,omitempty"`
	MatchedCount        uint64 `json:"matched_count,omitempty"`
	Label               string `json:"label,omitempty"`
	Message             string `json:"message,omitempty"`
	ReviewKind          string `json:"review_kind,omitempty"`
	ErrorClass          string `json:"error_class,omitempty"`
	SearchAction        string `json:"search_action,omitempty"`
	SearchRetryCount    uint8  `json:"search_retry_count,omitempty"`
	AIResearchCount     uint8  `json:"ai_research_count,omitempty"`
	SearchRetryAfterSec int    `json:"search_retry_after_sec,omitempty"`
	Revision            uint64 `json:"revision"`
	CreatedNS           int64  `json:"created_ns"`
	UpdatedNS           int64  `json:"updated_ns"`
	StageStartedNS      int64  `json:"stage_started_ns"`
}

type Evidence struct {
	ID                 string `json:"evidence_id"`
	JobID              string `json:"job_id"`
	Kind               string `json:"kind"`
	Source             string `json:"source"`
	SubjectID          string `json:"subject_id,omitempty"`
	AliasLanguage      string `json:"alias_language,omitempty"`
	IdentityConfirmed  bool   `json:"identity_confirmed,omitempty"`
	TracklistVerified  bool   `json:"tracklist_verified,omitempty"`
	ProductionVerified bool   `json:"production_verified,omitempty"`
	PayloadHash        string `json:"payload_hash"`
}

type EvidenceInput struct {
	ID                 string `json:"evidence_id"`
	Kind               string `json:"kind"`
	Source             string `json:"source"`
	SubjectID          string `json:"subject_id,omitempty"`
	AliasLanguage      string `json:"alias_language,omitempty"`
	IdentityConfirmed  bool   `json:"identity_confirmed,omitempty"`
	TracklistVerified  bool   `json:"tracklist_verified,omitempty"`
	ProductionVerified bool   `json:"production_verified,omitempty"`
	Payload            []byte `json:"payload,omitempty"`
}

type Start struct {
	JobID            string          `json:"job_id"`
	BatchID          string          `json:"batch_id"`
	Scene            string          `json:"scene"`
	Stage            string          `json:"stage"`
	Label            string          `json:"label,omitempty"`
	SourceTrackIDs   []string        `json:"source_track_ids,omitempty"`
	LibraryFileCount uint64          `json:"library_file_count,omitempty"`
	MatchedCount     uint64          `json:"matched_count,omitempty"`
	Payload          []byte          `json:"payload,omitempty"`
	Evidence         []EvidenceInput `json:"evidence,omitempty"`
	NowNS            int64           `json:"now_ns"`
}

type Complete struct {
	JobID      string                 `json:"job_id"`
	Edge       string                 `json:"edge"`
	NextStage  string                 `json:"next_stage,omitempty"`
	ReviewKind string                 `json:"review_kind,omitempty"`
	Message    string                 `json:"message,omitempty"`
	TaskState  string                 `json:"task_state"`
	ErrorClass string                 `json:"error_class,omitempty"`
	Result     []byte                 `json:"result,omitempty"`
	Evidence   []EvidenceInput        `json:"evidence,omitempty"`
	Search     *searchdecision.Report `json:"search,omitempty"`
	NowNS      int64                  `json:"now_ns"`
}

type Result struct {
	Job       *Job                     `json:"job,omitempty"`
	Evidence  []Evidence               `json:"evidence,omitempty"`
	Children  []Job                    `json:"children,omitempty"`
	Search    *searchdecision.Decision `json:"search,omitempty"`
	Task      *taskrepo.Task           `json:"task,omitempty"`
	Followups []taskrepo.Task          `json:"followups,omitempty"`
	Replayed  bool                     `json:"replayed"`
	Changed   bool                     `json:"changed"`
}

type Status struct {
	State  string  `json:"state"`
	Code   string  `json:"code,omitempty"`
	Result *Result `json:"result,omitempty"`
}

type JobPage struct {
	Jobs []Job  `json:"jobs"`
	Next []byte `json:"next,omitempty"`
}

type EvidencePage struct {
	Evidence []Evidence `json:"evidence"`
	Next     []byte     `json:"next,omitempty"`
}

type Inspection struct {
	Job        Job             `json:"job"`
	Task       *taskrepo.Task  `json:"task,omitempty"`
	Children   []Job           `json:"children,omitempty"`
	ChildTasks []taskrepo.Task `json:"child_tasks,omitempty"`
	Evidence   []Evidence      `json:"evidence,omitempty"`
}

func validID(id string) bool { return idPattern.MatchString(id) }

// ValidID reports whether a string is a legal job, batch or evidence id. It
// exists so the HTTP layer can name the offending field instead of turning a
// malformed request into a bare 409 from the repository (P1-2).
func ValidID(id string) bool { return validID(id) }

// ValidateEvidenceInput checks one caller-supplied evidence row without
// writing it: the id, kind and source must be legal and an identity claim must
// carry a verified tracklist from a catalogue/api/website source. The
// repository runs the same check again on commit; this is for the error
// message, not a substitute for it.
func ValidateEvidenceInput(in EvidenceInput) error {
	item := Evidence{
		ID: in.ID, JobID: "placeholder", Kind: in.Kind, Source: in.Source, SubjectID: in.SubjectID,
		AliasLanguage: in.AliasLanguage, IdentityConfirmed: in.IdentityConfirmed,
		TracklistVerified: in.TracklistVerified, ProductionVerified: in.ProductionVerified,
		PayloadHash: strings.Repeat("0", 64),
	}
	return validateEvidence(item)
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func trackDigest(ids []string) (uint64, string, error) {
	if len(ids) > 50000 {
		return 0, "", dataengine.ErrInvalidRequest
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !trackPattern.MatchString(id) || seen[id] {
			return 0, "", dataengine.ErrInvalidRequest
		}
		seen[id] = true
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	if len(sorted) == 0 {
		return 0, "", nil
	}
	canonical := make([]byte, 0)
	for i, id := range sorted {
		if i > 0 {
			canonical = append(canonical, '\n')
		}
		canonical = append(canonical, id...)
	}
	return uint64(len(sorted)), digestBytes(canonical), nil
}

func validateEvidence(item Evidence) error {
	if !validID(item.ID) || !validID(item.JobID) || !evidenceKinds[item.Kind] || !sourcePattern.MatchString(item.Source) || len(item.PayloadHash) != 64 {
		return dataengine.ErrInvalidRequest
	}
	if item.SubjectID != "" && !validID(item.SubjectID) {
		return dataengine.ErrInvalidRequest
	}
	switch item.AliasLanguage {
	case "", "ja", "zh", "en":
	default:
		return dataengine.ErrInvalidRequest
	}
	if item.Kind == "synthetic_fixture" && (item.ProductionVerified || item.IdentityConfirmed) {
		return dataengine.ErrInvalidRequest
	}
	// An alias or an AI result can be stored. Neither one confirms identity.
	if item.IdentityConfirmed {
		if !item.TracklistVerified || (item.Kind != "api" && item.Kind != "website" && item.Kind != "catalog") {
			return dataengine.ErrInvalidRequest
		}
	}
	return nil
}

func confirmedIdentity(items []Evidence) bool {
	for _, item := range items {
		if item.IdentityConfirmed && item.TracklistVerified && item.Kind != "ai" && item.Kind != "synthetic_fixture" {
			return true
		}
	}
	return false
}

func observedSource(items []Evidence) bool {
	for _, item := range items {
		if item.Kind == "api" || item.Kind == "website" || item.Kind == "catalog" {
			return true
		}
	}
	return false
}

func terminalStage(stage string) bool {
	switch stage {
	case "completed", "failed", "needs_review", "cancelled", "skipped":
		return true
	default:
		return false
	}
}

func validJob(job Job) bool {
	if !validID(job.ID) || !validID(job.BatchID) || !validID(job.RootID) || !scenes[job.Scene] || !stages[job.Stage] {
		return false
	}
	if job.Revision == 0 || job.CreatedNS <= 0 || job.UpdatedNS < job.CreatedNS || job.StageStartedNS <= 0 {
		return false
	}
	if job.ParentJobID != "" && !validID(job.ParentJobID) || job.ActiveTaskID != "" && !validID(job.ActiveTaskID) {
		return false
	}
	if len(job.Label) > 500 || len(job.Message) > 4000 || len(job.ErrorClass) > 80 {
		return false
	}
	if job.ReviewKind != "" && !reviewKinds[job.ReviewKind] {
		return false
	}
	switch job.SearchAction {
	case "", searchdecision.ActionQueueRequest, searchdecision.ActionRetry, searchdecision.ActionResearch, searchdecision.ActionReviewSearch, searchdecision.ActionReviewCandidate, searchdecision.ActionFallback, searchdecision.ActionSkip:
	default:
		return false
	}
	if job.SearchRetryCount > 3 || job.AIResearchCount > 1 {
		return false
	}
	switch job.SearchRetryAfterSec {
	case 0, 600, 1800, 3600:
	default:
		return false
	}
	switch job.Kind {
	case "album", "batch":
	default:
		return false
	}
	switch job.TaskType {
	case "preparation", "identification", "search", "album":
	default:
		return false
	}
	switch job.TaskGroup {
	case "identify", "search", "process", "repair":
	default:
		return false
	}
	if job.Scene == "playlist" {
		if job.SourceTrackCount == 0 || len(job.SourceTrackDigest) != 64 || job.MatchedCount > job.SourceTrackCount {
			return false
		}
	} else if job.SourceTrackCount != 0 || job.SourceTrackDigest != "" || job.MatchedCount != 0 {
		return false
	}
	return true
}
