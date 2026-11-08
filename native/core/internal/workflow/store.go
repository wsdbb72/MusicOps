package workflow

import (
	"encoding/json"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

func jobKey(id string) []byte { return []byte("workflow-job/" + digestBytes([]byte(id))) }
func childIndex(parent, id string) []byte {
	return []byte("workflow-child/" + digestBytes([]byte(parent)) + "/" + digestBytes([]byte(id)))
}
func rootIndex(root, id string) []byte {
	return []byte("workflow-root/" + digestBytes([]byte(root)) + "/" + digestBytes([]byte(id)))
}
func evidenceKey(id string) []byte { return []byte("workflow-evidence/" + digestBytes([]byte(id))) }
func evidenceIndex(jobID, id string) []byte {
	return []byte("workflow-evidence-job/" + digestBytes([]byte(jobID)) + "/" + digestBytes([]byte(id)))
}

func readJob(v *dataengine.View, id string) (*Job, []byte, error) {
	if !validID(id) {
		return nil, nil, dataengine.ErrInvalidRequest
	}
	raw, found, err := v.Get(jobKey(id))
	if err != nil || !found {
		return nil, nil, err
	}
	var job Job
	if json.Unmarshal(raw, &job) != nil || job.ID != id || !validJob(job) {
		return nil, nil, ErrIntegrity
	}
	return &job, raw, nil
}

func readEvidence(v *dataengine.View, id string) (*Evidence, []byte, error) {
	if !validID(id) {
		return nil, nil, dataengine.ErrInvalidRequest
	}
	raw, found, err := v.Get(evidenceKey(id))
	if err != nil || !found {
		return nil, nil, err
	}
	var item Evidence
	if json.Unmarshal(raw, &item) != nil || item.ID != id || validateEvidence(item) != nil {
		return nil, nil, ErrIntegrity
	}
	return &item, raw, nil
}

func putJob(previous []byte, job Job) (dataengine.Condition, []dataengine.Mutation, error) {
	if !validJob(job) {
		return dataengine.Condition{}, nil, ErrIntegrity
	}
	raw, err := json.Marshal(job)
	if err != nil {
		return dataengine.Condition{}, nil, err
	}
	condition := dataengine.Condition{Key: jobKey(job.ID), Value: previous, Absent: previous == nil}
	writes := []dataengine.Mutation{{Key: jobKey(job.ID), Value: raw}, {Key: rootIndex(job.RootID, job.ID), Value: raw}}
	if job.ParentJobID != "" {
		writes = append(writes, dataengine.Mutation{Key: childIndex(job.ParentJobID, job.ID), Value: raw})
	}
	return condition, writes, nil
}

func writeEvidence(v *dataengine.View, items []Evidence, payloads [][]byte) ([]dataengine.Condition, []dataengine.Mutation, error) {
	var conditions []dataengine.Condition
	var writes []dataengine.Mutation
	for i, item := range items {
		raw, err := json.Marshal(item)
		if err != nil {
			return nil, nil, err
		}
		current, found, err := v.Get(evidenceKey(item.ID))
		if err != nil {
			return nil, nil, err
		}
		if found {
			if string(current) != string(raw) {
				return nil, nil, ErrStale
			}
			continue
		}
		hash, blobs, err := taskrepo.WriteBlob(v, payloads[i])
		if err != nil {
			return nil, nil, err
		}
		if hash != item.PayloadHash {
			return nil, nil, ErrIntegrity
		}
		conditions = append(conditions, dataengine.Condition{Key: evidenceKey(item.ID), Absent: true})
		writes = append(writes, blobs...)
		writes = append(writes, dataengine.Mutation{Key: evidenceKey(item.ID), Value: raw}, dataengine.Mutation{Key: evidenceIndex(item.JobID, item.ID), Value: raw})
	}
	return conditions, writes, nil
}

func later(now, current int64) int64 {
	if now < current {
		return current
	}
	return now
}
