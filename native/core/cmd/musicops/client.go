package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// coreClient talks to a running Go core over its local HTTP API with the
// same Basic authentication the core itself enforces
// (musicops:<password>). It carries no desktop dependency, so the wire
// behaviour is unit-testable against a fake core.
type coreClient struct {
	base     string
	password string
	http     *http.Client // nil means http.DefaultClient
}

func (c *coreClient) client() *http.Client {
	if c.http != nil {
		return c.http
	}
	return http.DefaultClient
}

// do performs one authenticated request. A non-2xx response is an error
// carrying the core's code (and message when it sent one).
func (c *coreClient) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.base, "/")+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("musicops:"+c.password)))
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, coreError(resp.StatusCode, raw)
	}
	return json.RawMessage(raw), nil
}

func coreError(status int, raw []byte) error {
	var decoded struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &decoded); err == nil && decoded.Code != "" {
		if decoded.Error != "" {
			return fmt.Errorf("%s: %s", decoded.Code, decoded.Error)
		}
		return errors.New(decoded.Code)
	}
	text := strings.TrimSpace(string(raw))
	if len(text) > 200 {
		text = text[:200]
	}
	return fmt.Errorf("核心返回 HTTP %d: %s", status, text)
}

// getJobs returns the core's job list as raw job views.
func (c *coreClient) getJobs(ctx context.Context) ([]json.RawMessage, error) {
	raw, err := c.do(ctx, http.MethodGet, "/v1/jobs", nil)
	if err != nil {
		return nil, err
	}
	var decoded struct {
		Jobs []json.RawMessage `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("解析任务列表失败: %w", err)
	}
	if decoded.Jobs == nil {
		decoded.Jobs = []json.RawMessage{}
	}
	return decoded.Jobs, nil
}

// getJob returns the raw GET /v1/jobs/{id} response.
func (c *coreClient) getJob(ctx context.Context, id string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/jobs/"+id, nil)
}

// control posts one job control action.
func (c *coreClient) control(ctx context.Context, id, action string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/jobs/"+id+"/control", map[string]string{"action": action})
}

// jobState is the subset of the job responses the CLI reasons about.
type jobState struct {
	Inspection *struct {
		Job *struct {
			Stage string `json:"stage"`
		} `json:"job"`
		Task *struct {
			State string `json:"state"`
		} `json:"task"`
	} `json:"inspection"`
	Job *struct {
		Stage string `json:"stage"`
	} `json:"job"`
}

// extractState returns the polled state of a job response. The task state
// wins when present: it is the one that carries queued/running/paused and
// the terminal task states (failed/blocked/cancelled/recovery_required),
// while the job stage only carries workflow stages.
func extractState(raw json.RawMessage) string {
	var s jobState
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	if s.Inspection != nil {
		if s.Inspection.Task != nil && s.Inspection.Task.State != "" {
			return s.Inspection.Task.State
		}
		if s.Inspection.Job != nil && s.Inspection.Job.Stage != "" {
			return s.Inspection.Job.Stage
		}
	}
	if s.Job != nil {
		return s.Job.Stage
	}
	return ""
}

// terminalState mirrors the Python CLI exit rule: these states exit 2.
func terminalState(state string) bool {
	switch state {
	case "failed", "blocked", "cancelled", "recovery_required":
		return true
	default:
		return false
	}
}

// activeState mirrors the Python CLI --wait rule: these states keep polling.
func activeState(state string) bool {
	switch state {
	case "queued", "running", "paused":
		return true
	default:
		return false
	}
}

// exitCodeForState is 2 for a terminally failed job state, else 0.
func exitCodeForState(state string) int {
	if terminalState(state) {
		return 2
	}
	return 0
}

// waitSettled polls GET /v1/jobs/{id} every 200ms until the state leaves
// queued/running/paused. It returns the last fetched response and whether
// the wait timed out. timeoutSec 0 means no deadline.
func (c *coreClient) waitSettled(ctx context.Context, id string, timeoutSec float64) (json.RawMessage, string, bool, error) {
	var deadline time.Time
	if timeoutSec > 0 {
		deadline = time.Now().Add(time.Duration(timeoutSec * float64(time.Second)))
	}
	for {
		raw, err := c.getJob(ctx, id)
		if err != nil {
			return nil, "", false, err
		}
		state := extractState(raw)
		if !activeState(state) {
			return raw, state, false, nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return raw, state, true, nil
		}
		select {
		case <-ctx.Done():
			return raw, state, false, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
