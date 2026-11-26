package app

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/sqlitemigrate"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

var secretValue = regexp.MustCompile(`(?i)(authkey|torrent_pass|cookie|password|usetoken)=[^&\s"']*`)

func scrub(value string) string {
	return secretValue.ReplaceAllString(value, "$1=redacted")
}

var sensitiveKeys = map[string]bool{
	"cookie": true, "cookies": true, "password": true, "passwd": true,
	"authkey": true, "torrent_pass": true, "passkey": true, "secret": true,
	"api_key": true, "apikey": true, "authorization": true, "session": true,
	"credential": true, "credentials": true,
}

// publicTasks is the log view. Stored messages stay intact; the response
// must not repeat a cookie, authkey or torrent_pass that an executor put
// in a task message.
func publicTasks(tasks []taskrepo.Task) []taskrepo.Task {
	out := make([]taskrepo.Task, len(tasks))
	for i, task := range tasks {
		task.Message = scrub(task.Message)
		task.Phase = scrub(task.Phase)
		task.ErrorClass = scrub(task.ErrorClass)
		out[i] = task
	}
	return out
}

// publicRecord redacts an imported SQLite row before it leaves the process.
// The stored archive is unchanged. Text columns may be old task payloads.
func publicRecord(record sqlitemigrate.Record) sqlitemigrate.Record {
	for i := range record.Columns {
		record.Columns[i].Enc = scrubColumn(record.Columns[i].Enc)
	}
	return record
}

func scrubColumn(enc string) string {
	if len(enc) < 2 {
		return scrub(enc)
	}
	switch enc[0] {
	case 'T':
		return "T" + scrubText(enc[1:])
	default:
		return enc[:1] + scrub(enc[1:])
	}
}

func scrubText(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return scrub(value)
	}
	var parsed any
	if json.Unmarshal([]byte(value), &parsed) != nil {
		return scrub(value)
	}
	encoded, err := json.Marshal(redactJSON(parsed))
	if err != nil {
		return scrub(value)
	}
	return string(encoded)
}

func redactJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if sensitiveKeys[strings.ToLower(key)] {
				typed[key] = "redacted"
				continue
			}
			typed[key] = redactJSON(child)
		}
		return typed
	case []any:
		for i, child := range typed {
			typed[i] = redactJSON(child)
		}
		return typed
	case string:
		return scrub(typed)
	default:
		return value
	}
}

type jobView struct {
	ExecutionState   string          `json:"execution_state,omitempty"`
	AIReviewEligible bool            `json:"ai_review_eligible,omitempty"`
	ReviewJobIDs     []string        `json:"review_job_ids,omitempty"`
	ID               string          `json:"job_id"`
	RootID           string          `json:"root_id,omitempty"`
	ParentID         string          `json:"parent_job_id,omitempty"`
	Scene            string          `json:"scene"`
	Stage            string          `json:"stage"`
	Progress         string          `json:"progress"`
	Headline         string          `json:"headline"`
	Label            string          `json:"label,omitempty"`
	Message          string          `json:"message,omitempty"`
	ReviewKind       string          `json:"review_kind,omitempty"`
	ErrorClass       string          `json:"error_class,omitempty"`
	Paused           bool            `json:"paused,omitempty"`
	CancelRequested  bool            `json:"cancel_requested,omitempty"`
	Downloaded       bool            `json:"downloaded,omitempty"`
	LibraryPublished bool            `json:"library_published,omitempty"`
	Result           *jobResultView  `json:"result,omitempty"`
	UpdatedNS        int64           `json:"updated_ns"`
	RetryAfterNS     int64           `json:"retry_after_ns,omitempty"`
	RootStatus       *rootStatusView `json:"root_status,omitempty"`
}

// rootStatusView is the aggregate state of a root and every durable child.
// The root's own task often finishes after identification while a later
// search or download descendant is still running, blocked or failed. Showing
// only the root task state made those albums look completed and healthy.
type rootStatusView struct {
	JobID            string         `json:"job_id"`
	Stage            string         `json:"stage"`
	ExecutionState   string         `json:"execution_state"`
	Progress         string         `json:"progress"`
	Headline         string         `json:"headline"`
	Message          string         `json:"message,omitempty"`
	ReviewKind       string         `json:"review_kind,omitempty"`
	ErrorClass       string         `json:"error_class,omitempty"`
	Paused           bool           `json:"paused,omitempty"`
	CancelRequested  bool           `json:"cancel_requested,omitempty"`
	Downloaded       bool           `json:"downloaded,omitempty"`
	LibraryPublished bool           `json:"library_published,omitempty"`
	Result           *jobResultView `json:"result,omitempty"`
	UpdatedNS        int64          `json:"updated_ns"`
	RetryAfterNS     int64          `json:"retry_after_ns,omitempty"`
}

// jobResultView is the sanitized failure result shown to the UI and returned
// by the job API. Go has no SQL statement or traceback to expose here; the
// durable executor phase, class, task id and cause are the actionable
// diagnostics. Credentials are removed by scrub before serialization.
type jobResultView struct {
	OK         bool   `json:"ok"`
	TaskID     string `json:"task_id,omitempty"`
	Stage      string `json:"stage"`
	State      string `json:"state"`
	ErrorClass string `json:"error_class,omitempty"`
	Phase      string `json:"phase,omitempty"`
	Message    string `json:"message,omitempty"`
}

func publicJob(job workflow.Job) jobView {
	return jobView{
		ID: job.ID, RootID: job.RootID, ParentID: job.ParentJobID, Scene: job.Scene, Stage: job.Stage,
		Label: scrub(job.Label), Message: scrub(job.Message), ReviewKind: job.ReviewKind,
		ErrorClass: job.ErrorClass, Paused: job.Paused, CancelRequested: job.CancelRequested, UpdatedNS: job.UpdatedNS,
		RetryAfterNS: job.RetryAfterNS,
	}
}

func (a *App) publicJobWithResult(job workflow.Job) jobView {
	view := publicJob(job)
	if a == nil || a.tasks == nil || job.ActiveTaskID == "" {
		return view
	}
	task, err := a.tasks.Get(job.ActiveTaskID)
	if err != nil || task == nil || (job.Stage == "completed" && task.State == "completed") {
		return view
	}
	view.Result = &jobResultView{
		OK: task.State == "completed", TaskID: task.ID, Stage: job.Stage, State: task.State,
		ErrorClass: scrub(task.ErrorClass), Phase: scrub(task.Phase), Message: scrub(firstNonEmpty(task.Message, job.Message)),
	}
	return view
}

// presentJobs gives every record in one album the same current progress.
// A handed-off record keeps its own stage, and that stage is not shown as the album being finished.
func presentJobs(jobs []jobView) []jobView {
	groups := map[string][]jobView{}
	for _, job := range jobs {
		root := job.RootID
		if root == "" {
			root = job.ID
		}
		groups[root] = append(groups[root], job)
	}
	out := make([]jobView, len(jobs))
	for i, job := range jobs {
		root := job.RootID
		if root == "" {
			root = job.ID
		}
		job.Progress, job.Headline = albumProgress(groups[root])
		out[i] = job
	}
	return out
}

// markReleased shows library ingest only after a verified audio file is in the library.
// A local import is not a download: progress says ingested, not finished downloading,
// and Downloaded stays false. An internal handoff stage stays untouched when that record is absent.
func markReleased(jobs []jobView, albums map[string]bool) []jobView {
	if len(albums) == 0 {
		return jobs
	}
	out := make([]jobView, len(jobs))
	for i, job := range jobs {
		root := job.RootID
		if root == "" {
			root = job.ID
		}
		if albums[root] {
			job.LibraryPublished = true
			switch job.Progress {
			case "未下载":
				job.Progress = "已入库"
				job.Headline = "音乐已校验并放入库中（本地导入，未经站点下载）。"
			case "待核对", "未能继续", "已取消", "已跳过":
				job.Progress = "部分入库"
				if !strings.Contains(job.Headline, "部分入库") && !strings.Contains(job.Headline, "仍缺") {
					job.Headline = "已入库可用曲目，但仍有未完成项目：" + job.Headline
				}
			}
		}
		out[i] = job
	}
	return out
}

func albumProgress(tree []jobView) (string, string) {
	rank := map[string]int{
		"preparing": 1, "identifying": 1, "snapshotting": 1, "metadata": 1,
		"searching": 2, "fallback": 3, "requesting": 4, "validating": 4,
		"downloading": 5, "publishing": 5, "notifying": 5,
	}
	labels := map[string]string{
		"preparing": "准备中", "identifying": "识别中", "snapshotting": "识别中", "metadata": "整理资料",
		"searching": "检索中", "fallback": "已排队流媒体", "requesting": "获取中", "validating": "正在核对文件",
		"downloading": "获取中", "publishing": "正在入库", "notifying": "正在收尾",
	}
	var active, review, failed, cancelled, skipped *jobView
	best := 0
	for i := range tree {
		job := &tree[i]
		if job.Stage == "completed" && job.Result != nil && job.Result.State == "failed" {
			// A sibling can finish the album after this task failed. The root
			// stage alone would then say "completed" and hide the active task's
			// actionable verification error.
			failedView := *job
			failedView.Stage = "failed"
			failedView.ExecutionState = "failed"
			failedView.Progress = firstNonEmpty(job.Progress, "未能继续")
			failedView.Headline = firstNonEmpty(job.Result.Message, job.Headline, "任务失败")
			failedView.Message = firstNonEmpty(job.Result.Message, job.Message)
			failedView.ErrorClass = firstNonEmpty(job.Result.ErrorClass, job.ErrorClass)
			job = &failedView
		}
		switch job.Stage {
		case "needs_review":
			if review == nil || job.UpdatedNS >= review.UpdatedNS {
				review = job
			}
		case "failed":
			if failed == nil || job.UpdatedNS >= failed.UpdatedNS {
				failed = job
			}
		case "cancelled":
			if cancelled == nil || job.UpdatedNS >= cancelled.UpdatedNS {
				cancelled = job
			}
		case "skipped":
			if skipped == nil || job.UpdatedNS >= skipped.UpdatedNS {
				skipped = job
			}
		default:
			if rank[job.Stage] > best || (rank[job.Stage] == best && rank[job.Stage] > 0 && (active == nil || job.UpdatedNS >= active.UpdatedNS)) {
				if rank[job.Stage] > 0 {
					best = rank[job.Stage]
					active = job
				}
			}
		}
	}
	switch {
	case active != nil:
		return labels[active.Stage], withUndownloaded(active.Message, labels[active.Stage])
	case review != nil:
		return "待核对", withUndownloaded(review.Message, "待核对")
	case failed != nil:
		return "未能继续", withUndownloaded(failed.Message, "未能继续")
	case cancelled != nil:
		return "已取消", withUndownloaded(cancelled.Message, "已取消")
	case skipped != nil:
		return "已跳过", withUndownloaded(skipped.Message, "已跳过")
	default:
		return "未下载", "音乐还没有下载到库里。"
	}
}

// summarizeRootJobs projects the state a person needs to see for an album.
// The representative descendant is selected with the same precedence as the
// progress label: live work first, then review, failure, cancellation and a
// terminal skip. The root itself is retained as the stable click target.
func summarizeRootJobs(tree []jobView) rootStatusView {
	if len(tree) == 0 {
		return rootStatusView{Progress: "未下载", Headline: "音乐还没有下载到库里。"}
	}
	var active, review, failed, cancelled, skipped *jobView
	best := 0
	for i := range tree {
		job := &tree[i]
		if job.Stage == "completed" && job.Result != nil && job.Result.State == "failed" {
			// A sibling can finish the album after this task failed. The root
			// stage alone would then say "completed" and hide the active task's
			// actionable verification error.
			failedView := *job
			failedView.Stage = "failed"
			failedView.ExecutionState = "failed"
			failedView.Progress = firstNonEmpty(job.Progress, "未能继续")
			failedView.Headline = firstNonEmpty(job.Result.Message, job.Headline, "任务失败")
			failedView.Message = firstNonEmpty(job.Result.Message, job.Message)
			failedView.ErrorClass = firstNonEmpty(job.Result.ErrorClass, job.ErrorClass)
			job = &failedView
		}
		switch job.Stage {
		case "needs_review":
			if review == nil || job.UpdatedNS >= review.UpdatedNS {
				review = job
			}
		case "failed":
			if failed == nil || job.UpdatedNS >= failed.UpdatedNS {
				failed = job
			}
		case "cancelled":
			if cancelled == nil || job.UpdatedNS >= cancelled.UpdatedNS {
				cancelled = job
			}
		case "skipped":
			if skipped == nil || job.UpdatedNS >= skipped.UpdatedNS {
				skipped = job
			}
		default:
			rank := albumStageRank(job.Stage)
			if rank > best || rank == best && rank > 0 && (active == nil || job.UpdatedNS >= active.UpdatedNS) {
				if rank > 0 {
					best = rank
					active = job
				}
			}
		}
	}
	rep := active
	if rep == nil {
		rep = review
	}
	if rep == nil {
		rep = failed
	}
	if rep == nil {
		rep = cancelled
	}
	if rep == nil {
		rep = skipped
	}
	if rep == nil {
		rep = &tree[0]
	}
	progress, headline := "", ""
	if rep.Progress != "" {
		progress, headline = rep.Progress, rep.Headline
	} else {
		progress, headline = albumProgress(tree)
	}
	state := rep.ExecutionState
	if state == "" {
		switch rep.Stage {
		case "needs_review":
			state = "blocked"
		case "failed":
			state = "failed"
		case "cancelled":
			state = "cancelled"
		case "skipped", "completed":
			state = "completed"
		default:
			state = "running"
		}
	}
	out := rootStatusView{
		JobID: rep.ID, Stage: rep.Stage, ExecutionState: state, Progress: progress, Headline: headline,
		Message: rep.Message, ReviewKind: rep.ReviewKind, ErrorClass: rep.ErrorClass,
		Paused: rep.Paused, CancelRequested: rep.CancelRequested, Downloaded: rep.Downloaded, LibraryPublished: rep.LibraryPublished, Result: rep.Result, UpdatedNS: rep.UpdatedNS,
		RetryAfterNS: rep.RetryAfterNS,
	}
	for _, job := range tree {
		out.Downloaded = out.Downloaded || job.Downloaded
		out.LibraryPublished = out.LibraryPublished || job.LibraryPublished
	}
	return out
}

func albumStageRank(stage string) int {
	switch stage {
	case "preparing", "identifying", "snapshotting", "metadata":
		return 1
	case "searching":
		return 2
	case "fallback":
		return 3
	case "requesting", "validating":
		return 4
	case "downloading", "publishing", "notifying":
		return 5
	default:
		return 0
	}
}
func withUndownloaded(message, fallback string) string {
	message = userFacing(strings.TrimSpace(message))
	if message == "" {
		message = fallback
	}
	if strings.Contains(message, "没有下载") || strings.Contains(message, "尚未下载") {
		return message
	}
	return message + "。音乐还没有下载。"
}

func userFacing(message string) string {
	switch {
	case strings.Contains(message, "检索需要核对"), strings.Contains(message, "候选需要核对"):
		return "需要核对。音乐还没有下载。"
	case strings.Contains(message, "对账"), strings.Contains(message, "结果还不确定"):
		return "这次请求的结果还不确定。音乐还没有下载。"
	case strings.Contains(message, "种子"):
		return "音乐还没有下载，需要核对。"
	case developerCopy(message):
		return ""
	default:
		return strings.ReplaceAll(message, "尚未下载、尚未入库", "音乐还没有下载。")
	}
}

func developerCopy(message string) bool {
	for _, word := range []string{"Python", "python", "执行器", "进程", "四站", "authkey", "Pebble", "usetoken"} {
		if strings.Contains(message, word) {
			return true
		}
	}
	return false
}

func scrubInspection(in workflow.Inspection) workflow.Inspection {
	in.Job = scrubStored(in.Job)
	for i := range in.Children {
		in.Children[i] = scrubStored(in.Children[i])
	}
	if in.Task != nil {
		task := *in.Task
		task.Message = scrub(task.Message)
		in.Task = &task
	}
	for i := range in.ChildTasks {
		in.ChildTasks[i].Message = scrub(in.ChildTasks[i].Message)
	}
	return in
}

func scrubStored(job workflow.Job) workflow.Job {
	job.Label = scrub(job.Label)
	job.Message = scrub(job.Message)
	return job
}
