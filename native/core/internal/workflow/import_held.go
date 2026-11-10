package workflow

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

// ImportHeld stores one migrated job as needs_review and does not queue a
// task. A second call with the same id does nothing. Nothing is downloaded.
func (r *Repository) ImportHeld(job Job) (bool, error) {
	if r == nil || r.engine == nil {
		return false, dataengine.ErrInvalidRequest
	}
	job.Stage = "needs_review"
	if job.ReviewKind == "" {
		job.ReviewKind = "interrupted"
	}
	if job.Kind == "" {
		job.Kind = "album"
	}
	if job.TaskType == "" {
		job.TaskType = "album"
	}
	if job.TaskGroup == "" {
		job.TaskGroup = "process"
	}
	if job.RootID == "" {
		job.RootID = job.ID
	}
	if job.BatchID == "" {
		job.BatchID = job.ID
	}
	if job.Revision == 0 {
		job.Revision = 1
	}
	if job.Scene == "playlist" {
		if job.SourceTrackCount == 0 {
			job.SourceTrackCount = 1
			job.MatchedCount = 0
		}
		if len(job.SourceTrackDigest) != 64 {
			sum := sha256.Sum256([]byte(job.ID))
			job.SourceTrackDigest = hex.EncodeToString(sum[:])
		}
	}
	if !validJob(job) {
		return false, dataengine.ErrInvalidRequest
	}
	var existing *Job
	err := r.engine.View(func(v *dataengine.View) error {
		found, _, err := readJob(v, job.ID)
		existing = found
		return err
	})
	if err != nil {
		return false, err
	}
	if existing != nil {
		return existing.ErrorClass == "migrated", nil
	}
	condition, writes, err := putJob(nil, job)
	if err != nil {
		return false, err
	}
	ok, err := r.engine.CompareAndSwap([]dataengine.Condition{condition}, writes)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// RemoveHeld deletes one migrated needs_review job. It refuses a job that
// is not marked with the migration error class.
func (r *Repository) RemoveHeld(id string) error {
	if r == nil || r.engine == nil || !validID(id) {
		return dataengine.ErrInvalidRequest
	}
	var raw []byte
	var job *Job
	err := r.engine.View(func(v *dataengine.View) error {
		var err error
		job, raw, err = readJob(v, id)
		return err
	})
	if err != nil || job == nil {
		return err
	}
	if job.ErrorClass != "migrated" {
		return dataengine.ErrInvalidRequest
	}
	ok, err := r.engine.CompareAndSwap(
		[]dataengine.Condition{{Key: jobKey(id), Value: raw}},
		[]dataengine.Mutation{
			{Key: jobKey(id), Delete: true},
			{Key: rootIndex(job.RootID, job.ID), Delete: true},
		},
	)
	if err != nil {
		return err
	}
	if !ok {
		return dataengine.ErrInvalidRequest
	}
	return nil
}
