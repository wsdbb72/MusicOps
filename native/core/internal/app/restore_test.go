package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func TestReindexListsGapsAndDoesNotDeleteAudio(t *testing.T) {
	application := newApp(t)
	library := filepath.Join(t.TempDir(), "music")
	if err := os.MkdirAll(library, 0o700); err != nil {
		t.Fatal(err)
	}
	first := []byte("track-one")
	second := []byte("track-three")
	if err := os.WriteFile(filepath.Join(library, "01 first.flac"), first, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(library, "03 third.flac"), second, 0o600); err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	indexed := post(t, application, "/v1/library/reindex", map[string]any{})
	if indexed["added"] != float64(2) || indexed["deleted_files"] != float64(0) {
		t.Fatal(indexed)
	}
	again := post(t, application, "/v1/library/reindex", map[string]any{})
	if again["added"] != float64(0) || again["kept"] != float64(2) {
		t.Fatal(again)
	}
	gaps := migrationGet(t, application, "/v1/library/gaps")
	albums, _ := gaps["albums"].([]any)
	if len(albums) != 1 {
		t.Fatal(gaps)
	}
	missing, _ := albums[0].(map[string]any)["missing"].([]any)
	if len(missing) != 1 || missing[0] != float64(2) {
		t.Fatal(gaps)
	}
	got, err := os.ReadFile(filepath.Join(library, "01 first.flac"))
	if err != nil || string(got) != string(first) {
		t.Fatal("reindex changed the audio file", err)
	}
	path := filepath.Join(library, "01 first.flac")
	revised := []byte("track-one with updated artwork")
	if err := os.WriteFile(path, revised, 0o600); err != nil {
		t.Fatal(err)
	}
	if audiorelease.PresenceUnchanged(application.engine, libraryFileID(path)) {
		t.Fatal("an externally changed file still matched the old snapshot")
	}
	refreshed := post(t, application, "/v1/library/reindex", map[string]any{})
	if refreshed["added"] != float64(0) || refreshed["kept"] != float64(2) || !audiorelease.PresenceUnchanged(application.engine, libraryFileID(path)) {
		t.Fatal("reindex did not refresh an existing local snapshot", refreshed)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, revised) {
		t.Fatal("reindex modified the file", err)
	}
	started := post(t, application, "/v1/library/workflow", map[string]any{"scene": "fill", "label": "补缺"})
	if started["downloaded"] != false || started["started"] != false {
		t.Fatal(started)
	}
}

func TestReindexKeepsMultipleUnnumberedFilesAndDoesNotConvertWAV(t *testing.T) {
	application := newApp(t)
	library := t.TempDir()
	files := map[string][]byte{"first.wav": pcmWAV(100), "second.wav": pcmWAV(200)}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(library, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	reply := post(t, application, "/v1/library/reindex", map[string]any{})
	if reply["added"] != float64(2) {
		t.Fatal(reply)
	}
	for name, want := range files {
		if got, err := os.ReadFile(filepath.Join(library, name)); err != nil || !bytes.Equal(got, want) {
			t.Fatal(name, err)
		}
	}
	if entries, err := os.ReadDir(library); err != nil || len(entries) != len(files) {
		t.Fatal(entries, err)
	}
	replay := post(t, application, "/v1/library/reindex", map[string]any{})
	if replay["added"] != float64(0) || replay["kept"] != float64(2) {
		t.Fatal(replay)
	}
}

func TestCompilationPreviewDoesNotMoveUntilMerge(t *testing.T) {
	application := newApp(t)
	library := filepath.Join(t.TempDir(), "music")
	folder := filepath.Join(library, "合辑")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(folder, "01 song.flac")
	if err := os.WriteFile(source, []byte("compilation"), 0o600); err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	preview := migrationGet(t, application, "/v1/library/compilations")
	if preview["applied"] != false {
		t.Fatal(preview)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal(err)
	}
	merged := post(t, application, "/v1/library/compilations/merge", map[string]any{})
	if merged["applied"] != true {
		t.Fatal(merged)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatal("source still present", err)
	}
	dest := filepath.Join(library, "Compilations", "合辑", "01 song.flac")
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "compilation" {
		t.Fatal(err, string(got))
	}
}

func TestCompilationMergeRelocatesIndexedRecordAndLyrics(t *testing.T) {
	application := newApp(t)
	library := filepath.Join(t.TempDir(), "music")
	incoming := filepath.Join(t.TempDir(), "incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	writeToneWAV(t, filepath.Join(incoming, "compilation.wav"), 440)
	result, err := audiorelease.Release(context.Background(), application.engine, audiorelease.Request{
		RequestID: "compilation-record", RootID: "compilation-root", Artist: "Artist", Album: "合辑",
		Title: "Song", Track: 1, SourceName: "compilation.wav", Incoming: incoming, Library: library,
	})
	if err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	source, found, err := audiorelease.PathFor(application.engine, result.RequestID)
	if err != nil || !found {
		t.Fatal(source, found, err)
	}
	lyrics := filepath.Join(filepath.Dir(source), "01 Song.lrc")
	if err := os.WriteFile(lyrics, []byte("[00:00.00]lyrics\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyLyrics := source + ".lrc"
	if err := os.WriteFile(legacyLyrics, []byte("[00:00.00]legacy lyrics\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	compilationDir := filepath.Join(library, "Compilations", "合辑")
	if err := os.MkdirAll(compilationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	conflictingLyric := filepath.Join(compilationDir, filepath.Base(lyrics))
	if err := os.WriteFile(conflictingLyric, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if moved, err := application.compilationMoves(true); len(moved) != 0 || !os.IsExist(err) {
		t.Fatalf("conflicting lyrics: %v %v", moved, err)
	}
	if body, err := os.ReadFile(conflictingLyric); err != nil || string(body) != "existing" {
		t.Fatalf("conflicting lyrics overwritten: %q %v", body, err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(conflictingLyric); err != nil {
		t.Fatal(err)
	}
	merged := post(t, application, "/v1/library/compilations/merge", map[string]any{})
	if merged["applied"] != true {
		t.Fatal(merged)
	}
	dest, found, err := audiorelease.PathFor(application.engine, result.RequestID)
	if err != nil || !found || !strings.Contains(dest, filepath.Join("Compilations", "合辑")) {
		t.Fatalf("dest=%q found=%v err=%v", dest, found, err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "01 Song.lrc")); err != nil {
		t.Fatal("lyrics sidecar was not moved with the audio:", err)
	}
	if _, err := os.Stat(dest + ".lrc"); err != nil {
		t.Fatal("legacy lyrics sidecar was not moved with the audio:", err)
	}
}
func TestXLSXPlaylistReadsRowsWithoutMacros(t *testing.T) {
	tracks, _, err := parsePlaylistDocument(xlsxSheet(t), "")
	if err != nil || len(tracks) != 1 || tracks[0].Title != "夜曲" || tracks[0].Artist != "周杰伦" {
		t.Fatal(tracks, err)
	}
	macro := xlsxSheet(t)
	reader, err := zip.NewReader(bytes.NewReader(macro), int64(len(macro)))
	if err != nil {
		t.Fatal(err)
	}
	buf := new(bytes.Buffer)
	writer := zip.NewWriter(buf)
	for _, file := range reader.File {
		entry, err := writer.Create(file.Name)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := readZip(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = entry.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	extra, err := writer.Create("xl/vbaProject.bin")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = extra.Write([]byte("macro"))
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err = parsePlaylistDocument(buf.Bytes(), ""); err == nil {
		t.Fatal("macro workbook was accepted")
	}
}

func TestPipelinePauseBlocksUntilResume(t *testing.T) {
	application := newApp(t)
	scanCtx, started := application.beginDuplicateScan(context.Background())
	if !started {
		t.Fatal("duplicate scan state did not start")
	}
	paused := post(t, application, "/v1/control", map[string]any{"action": "pause"})
	select {
	case <-scanCtx.Done():
	default:
		t.Fatal("pause did not cancel the active duplicate scan")
	}
	if paused["paused"] != true {
		t.Fatal(paused)
	}
	application.finishDuplicateScan(nil, scanCtx.Err())
	scanRecorder := httptest.NewRecorder()
	application.duplicateScan(scanRecorder, httptest.NewRequest(http.MethodGet, "/v1/library/duplicates", nil))
	if scanRecorder.Code != http.StatusConflict || !strings.Contains(scanRecorder.Body.String(), "LIBRARY_SCAN_CANCELED") {
		t.Fatalf("paused duplicate scan status=%d body=%s", scanRecorder.Code, scanRecorder.Body.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := application.waitIfPaused(ctx); err == nil {
		t.Fatal("paused pipeline returned")
	}
	resumed := post(t, application, "/v1/control", map[string]any{"action": "resume"})
	if resumed["paused"] != false {
		t.Fatal(resumed)
	}
	if err := application.waitIfPaused(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRoonPreflightDoesNotWrite(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	got := make(chan []byte, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			got <- nil
			return
		}
		defer conn.Close()
		buf := make([]byte, 512)
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _ := conn.Read(buf)
		got <- append([]byte(nil), buf[:n]...)
	}()
	application := newApp(t)
	_, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	body := post(t, application, "/v1/settings/servers/roon/test", map[string]any{"host": "127.0.0.1", "port": port})
	seen := <-got
	if body["reachable"] != true || body["registered"] != false || !bytes.HasPrefix(seen, []byte("GET /api")) || bytes.Contains(seen, []byte("register\n")) {
		t.Fatal(body, string(seen))
	}
	closed := post(t, application, "/v1/settings/servers/roon/test", map[string]any{"host": "127.0.0.1", "port": 1})
	if closed["reachable"] != false || closed["registered"] != false {
		t.Fatal(closed)
	}
}

func TestDiscogsSearchStaysUnconfirmed(t *testing.T) {
	vaultKey(t)
	application := newApp(t)
	post(t, application, "/v1/settings/discogs", map[string]any{"token": "discogs-token-123"})
	var sawAuth string
	discogs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Some Artist - First Album","id":42,"type":"release"}]}`))
	}))
	defer discogs.Close()
	fake := newFakeMusicBrainz(t, catalogSearchJSON, catalogDetailJSON, http.StatusOK)
	clock := &catalogFakeClock{t: time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)}
	withCatalogClient(t, newCatalogTestClient(t, fake, clock.Now))
	body := catalogBody(catalogTrack("Opening", 200))
	body["discogs_base_url"] = discogs.URL
	status, decoded := callIdentify(t, application, body)
	if status != http.StatusOK || sawAuth != "Discogs token=discogs-token-123" {
		t.Fatal(status, sawAuth, decoded)
	}
	hits, _ := decoded["discogs"].([]any)
	if len(hits) != 1 || hits[0].(map[string]any)["identity_confirmed"] != false {
		t.Fatal(decoded["discogs"])
	}
	source := sourceByName(t, decoded, "discogs")
	if source["available"] != true {
		t.Fatal(source)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil || bytes.Contains(encoded, []byte("discogs-token-123")) {
		t.Fatal("token leaked into the identify response", err)
	}
}

func TestAliasResearchReturnsAQueryOnly(t *testing.T) {
	application := newApp(t)
	configureAliasAI(t, application, `{"query":"Canonical Artist Album"}`)
	query, ok := application.researchAlias(context.Background(), workflow.Job{}, SearchPlan{Query: "old query", ExpectedTitle: "Album", ExpectedArtist: "Artist"}, searchdecision.Decision{Action: searchdecision.ActionReviewSearch})
	if !ok || query != "Canonical Artist Album" {
		t.Fatal(query, ok)
	}
	_, ok = application.researchAlias(context.Background(), workflow.Job{AIResearchCount: 1}, SearchPlan{Query: "old query"}, searchdecision.Decision{Action: searchdecision.ActionReviewSearch})
	if ok {
		t.Fatal("second research was allowed")
	}
}

func TestMetadataWriteRequiresAConfirmedMatch(t *testing.T) {
	application := newApp(t)
	library := filepath.Join(t.TempDir(), "music")
	if err := os.MkdirAll(library, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(library, "01 opening.flac")
	original := syntheticFLAC(t, false)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	post(t, application, "/v1/settings/library", map[string]any{"library_dir": library})
	refused := postCode(t, application, "/v1/library/metadata", map[string]any{
		"path": path, "artist": "Some Artist", "album": "First Album", "title": "Opening",
		"catalog_base_url": "https://musicbrainz.org",
	})
	if refused.Code != http.StatusBadRequest {
		t.Fatal(refused.Code, refused.Body.String())
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("public catalog attempt changed the file", err)
	}
	fake := newFakeMusicBrainz(t, catalogSearchJSON, catalogDetailJSON, http.StatusOK)
	written := post(t, application, "/v1/library/metadata", map[string]any{
		"path": path, "artist": "Some Artist", "album": "First Album", "title": "Opening",
		"catalog_base_url": fake.server.URL,
	})
	if written["written"] != true {
		t.Fatal(written)
	}
	after, err := os.ReadFile(path)
	if err != nil || bytes.Equal(after, original) {
		t.Fatal("confirmed match did not write tags", err)
	}
}

func TestRestoredControlsAreInThePage(t *testing.T) {
	for _, id := range []string{
		"pipeline-pause", "pipeline-resume", "reindex-library", "list-gaps", "fill-gaps",
		"save-auto-maintenance", "run-auto-maintenance", "auto-maintenance-enabled", "auto-maintenance-scrape",
		"upgrade-album", "expand-album", "preview-compilations", "merge-compilations",
		"plan-artist-folders", "apply-artist-folders", "decrypt-ncm", "unify-qbt-tags", "fetch-playlist",
		"review-download", "review-more", "scrape-preview", "scrape-apply",
		"guard-save", "guard-clear", "guard-dir", "guard-suppress",
		"save-netease", "netease-cookie", "scrape-artwork", "playlist-enqueue",
		"roon-connect", "roon-disconnect", "roon-zones", "backfill-lrc", "backfill-lrc-preview",
		"lookup-sources", "sources-result",
	} {
		if !bytes.Contains([]byte(appJS), []byte(id)) {
			t.Fatal("missing control", id)
		}
	}
	if !bytes.Contains([]byte(appJS), []byte(".xlsx")) {
		t.Fatal("xlsx is not offered")
	}
	if !bytes.Contains([]byte(appJS), []byte("/v1/jobs?review=1")) {
		t.Fatal("the review view does not ask the server for review work")
	}
	if !bytes.Contains([]byte(appJS), []byte("review_job_ids")) || !bytes.Contains([]byte(appJS), []byte("actionJobIDs")) || !bytes.Contains([]byte(appJS), []byte("start += 50")) {
		t.Fatal("review batch actions do not target the actionable child job")
	}
}

func xlsxSheet(t *testing.T) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	writer := zip.NewWriter(buf)
	shared, err := writer.Create("xl/sharedStrings.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = shared.Write([]byte(`<sst><si><t>歌名</t></si><si><t>歌手</t></si><si><t>夜曲</t></si><si><t>周杰伦</t></si></sst>`))
	sheet, err := writer.Create("xl/worksheets/sheet1.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sheet.Write([]byte(`<worksheet><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row><row r="2"><c r="A2" t="s"><v>2</v></c><c r="B2" t="s"><v>3</v></c></row></sheetData></worksheet>`))
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
