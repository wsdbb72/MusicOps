package app

import (
	"regexp"

	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

var secretValue = regexp.MustCompile(`(?i)(authkey|torrent_pass|cookie|password|usetoken)=[^&\s"']*`)

func scrub(value string) string {
	return secretValue.ReplaceAllString(value, "$1=redacted")
}

type jobView struct {
	ID         string `json:"job_id"`
	ParentID   string `json:"parent_job_id,omitempty"`
	Scene      string `json:"scene"`
	Stage      string `json:"stage"`
	Label      string `json:"label,omitempty"`
	Message    string `json:"message,omitempty"`
	ReviewKind string `json:"review_kind,omitempty"`
	ErrorClass string `json:"error_class,omitempty"`
	Paused     bool   `json:"paused,omitempty"`
	UpdatedNS  int64  `json:"updated_ns"`
}

func publicJob(job workflow.Job) jobView {
	return jobView{
		ID: job.ID, ParentID: job.ParentJobID, Scene: job.Scene, Stage: job.Stage,
		Label: scrub(job.Label), Message: scrub(job.Message), ReviewKind: job.ReviewKind,
		ErrorClass: job.ErrorClass, Paused: job.Paused, UpdatedNS: job.UpdatedNS,
	}
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
