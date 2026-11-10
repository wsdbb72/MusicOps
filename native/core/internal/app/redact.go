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
	ID               string `json:"job_id"`
	RootID           string `json:"root_id,omitempty"`
	ParentID         string `json:"parent_job_id,omitempty"`
	Scene            string `json:"scene"`
	Stage            string `json:"stage"`
	Progress         string `json:"progress"`
	Headline         string `json:"headline"`
	Label            string `json:"label,omitempty"`
	Message          string `json:"message,omitempty"`
	ReviewKind       string `json:"review_kind,omitempty"`
	ErrorClass       string `json:"error_class,omitempty"`
	Paused           bool   `json:"paused,omitempty"`
	CancelRequested  bool   `json:"cancel_requested,omitempty"`
	Downloaded       bool   `json:"downloaded,omitempty"`
	LibraryPublished bool   `json:"library_published,omitempty"`
	UpdatedNS        int64  `json:"updated_ns"`
}

func publicJob(job workflow.Job) jobView {
	return jobView{
		ID: job.ID, RootID: job.RootID, ParentID: job.ParentJobID, Scene: job.Scene, Stage: job.Stage,
		Label: scrub(job.Label), Message: scrub(job.Message), ReviewKind: job.ReviewKind,
		ErrorClass: job.ErrorClass, Paused: job.Paused, CancelRequested: job.CancelRequested, UpdatedNS: job.UpdatedNS,
	}
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
			job.Progress = "已入库"
			job.Headline = "音乐已校验并放入库中（本地导入，未经站点下载）。"
			job.LibraryPublished = true
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
