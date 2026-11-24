package app

import (
	"context"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/aiclient"
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
	if err != nil || !settings.Enabled || strings.TrimSpace(settings.BaseURL) == "" || strings.TrimSpace(settings.Model) == "" || a.vault == nil {
		return "", false
	}
	_, host, err := cleanURL(settings.BaseURL, false)
	if err != nil || host == "" {
		return "", false
	}
	key, found, err := a.vault.Get(aiKeyRef, host)
	if err != nil || !found || strings.TrimSpace(key) == "" {
		return "", false
	}
	client, err := aiclient.New(settings.BaseURL, settings.Model, key)
	if err != nil {
		return "", false
	}
	a.useAIHTTPClient(client)
	client.Timeout = time.Duration(settings.Timeout) * time.Second
	client.MaxOutputTokens = settings.MaxOutputTokens
	client.Temperature = settings.Temperature
	suggestion, err := client.SuggestAlias(ctx, aiclient.AliasRequest{
		Artist: plan.ExpectedArtist, AlbumArtist: plan.AlbumArtist, Title: plan.ExpectedTitle, Query: plan.Query,
	})
	if err != nil {
		return "", false
	}
	candidates := []string{suggestion.Query}
	candidates = append(candidates, suggestion.Aliases...)
	if strings.TrimSpace(suggestion.Artist) != "" && strings.TrimSpace(plan.ExpectedTitle) != "" {
		candidates = append(candidates, suggestion.Artist+" "+plan.ExpectedTitle)
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
