package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

// researchAlias asks the configured model for one alternate search string.
// The result is only a new query. It cannot confirm identity or spend a token.
func (a *App) researchAlias(ctx context.Context, job workflow.Job, plan SearchPlan, decision searchdecision.Decision) (string, bool) {
	if job.AIResearchCount != 0 {
		return "", false
	}
	switch decision.Action {
	case searchdecision.ActionQueueRequest, searchdecision.ActionFallback, searchdecision.ActionRetry, searchdecision.ActionResearch:
		return "", false
	}
	settings, err := a.loadAISettings()
	if err != nil || !settings.Enabled || strings.TrimSpace(settings.BaseURL) == "" {
		return "", false
	}
	payload, err := json.Marshal(map[string]string{
		"task": "alias-search", "artist": plan.ExpectedArtist, "album": plan.ExpectedTitle, "query": plan.Query,
	})
	if err != nil {
		return "", false
	}
	endpoint := strings.TrimRight(settings.BaseURL, "/") + "/responses"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", false
	}
	var decoded struct {
		Query   string   `json:"query"`
		Aliases []string `json:"aliases"`
		Artist  string   `json:"artist"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&decoded) != nil {
		return "", false
	}
	candidates := []string{decoded.Query}
	candidates = append(candidates, decoded.Aliases...)
	if strings.TrimSpace(decoded.Artist) != "" {
		candidates = append(candidates, decoded.Artist+" "+plan.ExpectedTitle)
	}
	original := strings.TrimSpace(plan.Query)
	for _, candidate := range candidates {
		query := strings.TrimSpace(candidate)
		if query == "" || query == original || len([]rune(query)) > 300 {
			continue
		}
		return query, true
	}
	return "", false
}
