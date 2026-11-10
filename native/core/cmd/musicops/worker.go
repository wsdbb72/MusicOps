package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// workerRoute is an old worker command that already has an HTTP handler.
// Commands without a route return a clear error and do not pretend to run.
type workerRoute struct {
	method string
	path   string
	scene  string
}

var workerRoutes = map[string]workerRoute{
	"index_library":            {method: http.MethodPost, path: "/v1/library/reindex"},
	"list_library_files":       {method: http.MethodGet, path: "/v1/library/files"},
	"list_library_albums":      {method: http.MethodGet, path: "/v1/library"},
	"get_status":               {method: http.MethodGet, path: "/v1/jobs"},
	"list_tasks":               {method: http.MethodGet, path: "/v1/jobs"},
	"get_task":                 {method: http.MethodGet, path: "/v1/jobs/{id}"},
	"search":                   {method: http.MethodPost, path: "/v1/search"},
	"manual_album_search":      {method: http.MethodPost, path: "/v1/search"},
	"identify_metadata":        {method: http.MethodPost, path: "/v1/catalog/identify"},
	"resolve_metadata":         {method: http.MethodPost, path: "/v1/library/metadata"},
	"accept_metadata":          {method: http.MethodPost, path: "/v1/library/metadata"},
	"refresh_playlist":         {method: http.MethodPost, path: "/v1/playlists/fetch"},
	"parse_playlist":           {method: http.MethodPost, path: "/v1/playlists/import"},
	"locate_playlist":          {method: http.MethodPost, path: "/v1/playlists/locate"},
	"list_subscriptions":       {method: http.MethodGet, path: "/v1/subscriptions"},
	"check_subscription":       {method: http.MethodPost, path: "/v1/subscriptions/check"},
	"check_subscription_now":   {method: http.MethodPost, path: "/v1/subscriptions/check"},
	"preview_compilation":      {method: http.MethodGet, path: "/v1/library/compilations"},
	"merge_compilation":        {method: http.MethodPost, path: "/v1/library/compilations/merge"},
	"scan_duplicates":          {method: http.MethodGet, path: "/v1/library/duplicates"},
	"merge_duplicates":         {method: http.MethodPost, path: "/v1/library/duplicates/merge"},
	"restore_duplicates":       {method: http.MethodPost, path: "/v1/library/duplicates/restore"},
	"normalize_qbt_tags":       {method: http.MethodPost, path: "/v1/settings/qbittorrent/tags"},
	"roon_preflight":           {method: http.MethodPost, path: "/v1/settings/servers/roon/test"},
	"qbt_preflight":            {method: http.MethodPost, path: "/v1/settings/qbittorrent/test"},
	"test_qbt_connection":      {method: http.MethodPost, path: "/v1/settings/qbittorrent/test"},
	"browser_solver_preflight": {method: http.MethodPost, path: "/v1/settings/solver/test"},
	"test_browser_solver":      {method: http.MethodPost, path: "/v1/settings/solver/test"},
	"pt_preflight":             {method: http.MethodPost, path: "/v1/sites/preflight"},
	"engine_control":           {method: http.MethodPost, path: "/v1/control"},
	"events":                   {method: http.MethodGet, path: "/v1/logs"},
	"request_rollback":         {method: http.MethodPost, path: "/v1/migration/rollback"},
	"prepare_fill":             {method: http.MethodPost, path: "/v1/library/workflow", scene: "fill"},
	"validate_selected_file":   {method: http.MethodPost, path: "/v1/library/workflow", scene: "upgrade"},
	"submit_download":          {method: http.MethodPost, path: "/v1/jobs/{id}/download"},
	"lyrics_preflight":         {method: http.MethodPost, path: "/v1/settings/lyrics/test"},
	"ai_test":                  {method: http.MethodPost, path: "/v1/settings/ai/test"},
	"list_workflows":           {method: http.MethodGet, path: "/v1/jobs"},
	"get_workflow":             {method: http.MethodGet, path: "/v1/jobs/{id}"},
	"transcode":                {method: http.MethodPost, path: "/v1/library/transcode"},
	"transcode_batch":          {method: http.MethodPost, path: "/v1/library/transcode/batch"},
	"transcode_status":         {method: http.MethodGet, path: "/v1/library/transcode/batch"},
	"transcode_control":        {method: http.MethodPost, path: "/v1/library/transcode/batch/control"},
	"wav_settings":             {method: http.MethodPost, path: "/v1/settings/wav"},
}

// unwiredWorker lists 0.4.31 commands that have no HTTP equivalent. They fail
// closed instead of reporting a fake success.
var unwiredWorker = map[string]bool{
	"demo": true, "compare": true, "resources": true, "prepare_resource": true,
	"prepare_repair": true, "prepare_dedup": true, "submit_plan": true, "recover": true,
	"ai_analyze": true, "ai_plan": true, "assist_workflows": true, "start_workflow": true,
	"advance_workflows": true, "restore_workflow": true, "archive_events": true,
	"compilation_albums": true, "duplicate_history": true, "attach": true,
	"get_playlist_targets": true, "queue_ai_reviews": true, "export_report": true,
}

func workerCommand(cmd string) bool {
	_, ok := workerRoutes[cmd]
	return ok
}

// workerRequest maps a worker command onto an existing HTTP call.
func workerRequest(cmd string, payload map[string]any) (string, string, any, error) {
	if unwiredWorker[cmd] {
		return "", "", nil, fmt.Errorf("%s 尚未接入，没有执行", cmd)
	}
	route, ok := workerRoutes[cmd]
	if !ok {
		return "", "", nil, fmt.Errorf("未知命令: %s", cmd)
	}
	fields := map[string]any{}
	for key, value := range payload {
		fields[key] = value
	}
	path := route.path
	if strings.Contains(path, "{id}") {
		id := firstString(fields, "job_id", "task_id", "id")
		if id == "" {
			return "", "", nil, errors.New("需要任务 ID，没有执行")
		}
		path = strings.ReplaceAll(path, "{id}", url.PathEscape(id))
	}
	if route.method == http.MethodGet {
		query := url.Values{}
		if offset, ok := fields["offset"]; ok {
			query.Set("offset", fmt.Sprint(offset))
		}
		if id := firstString(fields, "batch_id"); id != "" {
			query.Set("batch_id", id)
		}
		if encoded := query.Encode(); encoded != "" {
			path += "?" + encoded
		}
		return route.method, path, nil, nil
	}
	if route.scene != "" {
		if _, ok := fields["scene"]; !ok {
			fields["scene"] = route.scene
		}
	}
	if len(fields) == 0 {
		return route.method, path, map[string]any{}, nil
	}
	return route.method, path, fields, nil
}

func firstString(fields map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := fields[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func runMappedWorker(ctx context.Context, cmd, profile string, flags *cliFlags) (int, bool) {
	if unwiredWorker[cmd] {
		return printError(fmt.Errorf("%s 尚未接入，没有执行", cmd)), true
	}
	route, ok := workerRoutes[cmd]
	if !ok {
		return 0, false
	}
	_ = route
	method, path, body, err := workerRequest(cmd, flags.payload)
	if err != nil {
		return printError(err), true
	}
	client, err := attachCore(profile, flags.coreBinary)
	if err != nil {
		return printError(err), true
	}
	raw, err := client.do(ctx, method, path, body)
	if err != nil {
		return printError(err), true
	}
	printRawJSON(raw)
	return 0, true
}
