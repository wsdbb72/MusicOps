package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestNeteaseTrackIDsPageThroughSongDetail(t *testing.T) {
	var detailCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/playlist/detail", func(w http.ResponseWriter, r *http.Request) {
		ids := make([]string, 101)
		for i := range ids {
			ids[i] = fmt.Sprintf(`{"id":%d}`, i+1)
		}
		_, _ = io.WriteString(w, `{"code":200,"playlist":{"id":1001,"name":"只有编号","trackCount":101,"trackIds":[`+strings.Join(ids, ",")+`]}}`)
	})
	mux.HandleFunc("/api/v3/song/detail", func(w http.ResponseWriter, r *http.Request) {
		detailCalls.Add(1)
		raw, err := url.QueryUnescape(r.URL.Query().Get("c"))
		if err != nil {
			http.Error(w, "query", http.StatusBadRequest)
			return
		}
		var rows []struct {
			ID int `json:"id"`
		}
		if json.Unmarshal([]byte(raw), &rows) != nil || len(rows) == 0 {
			http.Error(w, "rows", http.StatusBadRequest)
			return
		}
		songs := make([]string, 0, len(rows))
		for _, row := range rows {
			songs = append(songs, fmt.Sprintf(`{"id":%d,"name":"歌%d","ar":[{"name":"甲"}],"al":{"name":"专"},"dt":200000}`, row.ID, row.ID))
		}
		_, _ = io.WriteString(w, `{"code":200,"songs":[`+strings.Join(songs, ",")+`]}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	application := newApp(t)
	fetched := post(t, application, "/v1/playlists/fetch", map[string]any{
		"platform": "netease", "playlist_id": "1001", "base_url": server.URL,
	})
	if fetched["downloaded"] != false || fetched["track_count"] != float64(101) || fetched["name"] != "只有编号" || detailCalls.Load() != 2 {
		t.Fatal(fetched, detailCalls.Load())
	}
	if strings.Contains(fmt.Sprint(fetched["note"]), "已入库") {
		t.Fatal(fetched["note"])
	}
}

func TestKugouCollectionAndSharePage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/get_other_list_file", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("global_collection_id") != "collection_full" || len(r.URL.Query().Get("signature")) != 32 {
			http.Error(w, "query", http.StatusBadRequest)
			return
		}
		title := "第一"
		if r.URL.Query().Get("page") != "1" {
			title = "第二"
		}
		fmt.Fprintf(w, `{"status":1,"data":{"count":2,"name":"完整","info":[{"songname":"%s","singername":"甲","duration":180}]}}`, title)
	})
	mux.HandleFunc("/songlist/gcid_preview/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<html><script>window.$output = {"encode_gic":"collection_full","info":{"listinfo":{"name":"预览"},"songs":[{"songname":"预览","singername":"甲"}]}}</script></html>`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	application := newApp(t)
	collection := post(t, application, "/v1/playlists/fetch", map[string]any{
		"platform": "kugou", "playlist_id": "collection_full", "base_url": server.URL,
	})
	if collection["downloaded"] != false || collection["track_count"] != float64(2) || collection["truncated"] != false || collection["name"] != "完整" {
		t.Fatal(collection)
	}
	shared := post(t, application, "/v1/playlists/fetch", map[string]any{
		"platform": "kugou", "playlist_id": "gcid_preview", "base_url": server.URL,
	})
	if shared["downloaded"] != false || shared["track_count"] != float64(2) || shared["name"] != "完整" {
		t.Fatal(shared)
	}
}

func TestSpotifyWithoutTokenReadsThePublicEmbed(t *testing.T) {
	id := "0123456789012345678901"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/embed/playlist/"+id {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `<html><script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{"state":{"data":{"entity":{"uri":"spotify:playlist:%s","name":"公开","trackList":[{"title":"歌","subtitle":"甲","duration_ms":200000}]}}}}}}</script></html>`, id)
	}))
	defer server.Close()
	application := newApp(t)
	fetched := post(t, application, "/v1/playlists/fetch", map[string]any{
		"platform": "spotify", "playlist_id": id, "base_url": server.URL,
	})
	note, _ := fetched["note"].(string)
	if fetched["downloaded"] != false || fetched["imported"] != false || fetched["preview"] != true || fetched["track_count"] != float64(1) || !strings.Contains(note, "公开嵌入") || !strings.Contains(note, "不保证完整") {
		t.Fatal(fetched)
	}
}

func TestEmbeddedCoverModeSkipsTheSidecar(t *testing.T) {
	application := newApp(t)
	library := filepath.Join(t.TempDir(), "music")
	if err := os.MkdirAll(library, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(library, "01 opening.flac")
	if err := os.WriteFile(path, syntheticFLAC(t, false), 0o600); err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	catalog := newFakeMusicBrainz(t, catalogSearchJSON, catalogDetailJSON, http.StatusOK)
	coverBody := append([]byte{0xff, 0xd8}, []byte(strings.Repeat("a", 64))...)
	cover := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(coverBody)
	}))
	defer cover.Close()
	written := post(t, application, "/v1/library/metadata", map[string]any{
		"path": path, "artist": "Some Artist", "album": "First Album", "title": "Opening",
		"catalog_base_url": catalog.server.URL, "cover_base_url": cover.URL, "cover_mode": "embedded",
	})
	if written["written"] != true || written["cover_mode"] != "embedded" {
		t.Fatal(written)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytesEqual(flactags.FrontCover(after), coverBody) {
		t.Fatal(err)
	}
	if _, err := os.Stat(strings.TrimSuffix(path, ".flac") + ".jpg"); !os.IsNotExist(err) {
		t.Fatal("embedded mode left a jpg", err)
	}
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func TestWorkflowPagesAndConfirmsBeforeStarting(t *testing.T) {
	application := newApp(t)
	library := filepath.Join(t.TempDir(), "music")
	if err := os.MkdirAll(library, 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(library, "01 opening.flac")
	second := filepath.Join(library, "02 second.flac")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("local-audio"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	request := httptest.NewRequest(http.MethodGet, "/v1/library/files?offset=1&limit=1", nil)
	request.SetBasicAuth("musicops", "test-password-16b")
	recorder := httptest.NewRecorder()
	application.Handler().ServeHTTP(recorder, request)
	var page map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil || page["total"] != float64(2) || len(page["files"].([]any)) != 1 {
		t.Fatal(recorder.Body.String(), err)
	}
	preview := post(t, application, "/v1/library/workflow", map[string]any{"scene": "upgrade", "paths": []string{first, second}})
	if preview["started"] != false || preview["downloaded"] != false {
		t.Fatal(preview)
	}
	empty := postCode(t, application, "/v1/library/workflow", map[string]any{"scene": "fill", "confirmed": true})
	if empty.Code != http.StatusBadRequest || !strings.Contains(empty.Body.String(), "逐首确认") {
		t.Fatal(empty.Code, empty.Body.String())
	}
	started := post(t, application, "/v1/library/workflow", map[string]any{
		"scene": "fill", "label": "补缺", "paths": []string{first, second}, "confirmed": true,
	})
	note, _ := started["note"].(string)
	if started["downloaded"] != false || started["started"] != true || !strings.Contains(note, "令牌规则") || !strings.Contains(note, "尚未下载") {
		t.Fatal(started)
	}
}

func TestAliasSkipsAnAliasThatRepeatsTheQuery(t *testing.T) {
	application := newApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"query":"old query","aliases":["old query","Fresh Alias"]}`)
	}))
	defer server.Close()
	post(t, application, "/v1/settings/ai", map[string]any{"enabled": true, "base_url": server.URL, "model": "alias"})
	query, ok := application.researchAlias(context.Background(), workflow.Job{}, SearchPlan{Query: "old query", ExpectedTitle: "Album"}, searchdecision.Decision{Action: searchdecision.ActionReviewSearch})
	if !ok || query != "Fresh Alias" {
		t.Fatal(query, ok)
	}
}

func TestRoonEnvelopeRejectsBareJSONAndUnauthorized(t *testing.T) {
	if name, _, auth := parseMooInfo(`{"core_id":"c1","display_name":"Living Room","display_version":"2.0"}`); name != "" || auth {
		t.Fatal(name, auth)
	}
	denied := "MOO/1 COMPLETE Unauthorized\nRequest-Id: 0\n\n"
	if name, _, auth := parseMooInfo(denied); name != "" || !auth {
		t.Fatal(name, auth)
	}
}
