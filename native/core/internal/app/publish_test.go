package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

func startSearchJob(t *testing.T, application *App, jobID string) {
	t.Helper()
	post(t, application, "/v1/search", searchBody(jobID, "http://127.0.0.1:9", []string{}, false))
	waitJob(t, application, "search:"+jobID, "needs_review")
}

func TestPublishControlledAudio(t *testing.T) {
	application := newApp(t)
	startSearchJob(t, application, "pub-ctl")
	wav := pcmWAV(24)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(wav)
	}))
	defer server.Close()
	posted := postCode(t, application, "/v1/audio/publish", map[string]any{
		"job_id": "pub-ctl", "source_url": server.URL, "artist": "Artist", "album": "Album", "title": "Title", "track": 1,
	})
	if posted.Code != http.StatusOK || !strings.Contains(posted.Body.String(), `"publish_task_id":"stage:pub-ctl:publishing"`) {
		t.Fatal(posted.Code, posted.Body.String())
	}
	waitJob(t, application, "pub-ctl", "completed")
	published, err := os.ReadFile(filepath.Join(application.Dir, "library-audio", "Artist", "Album", "01 Title.wav"))
	if err != nil || !bytes.Equal(wav, published) {
		t.Fatal(err)
	}
	// The placed file must also be visible in the library catalogue: a
	// controlled publish that only lands on disk is invisible to the
	// library view, dedup scan and artist-folder planning.
	library := getAuth(t, application, "/v1/library")
	if !strings.Contains(library.Body.String(), `"file":"Artist/Album/01 Title.wav"`) ||
		!strings.Contains(library.Body.String(), `"origin":"controlled_audio"`) {
		t.Fatal(library.Body.String())
	}
	detail := getAuth(t, application, "/v1/jobs/pub-ctl")
	if !strings.Contains(detail.Body.String(), "controlled-audio") {
		t.Fatal(detail.Body.String())
	}
}

func TestPublishRejectsBadShapes(t *testing.T) {
	application := newApp(t)
	startSearchJob(t, application, "pub-bad")
	for _, body := range []map[string]any{
		{"job_id": "pub-bad", "source_url": "http://example.com/x.wav", "artist": "Artist", "album": "Album", "title": "Title", "track": 1},
		{"job_id": "pub-bad", "source_url": "http://127.0.0.1:9/x.wav", "artist": "Artist", "album": "Album", "title": "Title", "track": 1, "receipts": []map[string]any{{"request_id": "r1", "sha256": "00"}}},
		{"job_id": "pub-bad"},
		{"job_id": "pub-bad", "source_url": "http://127.0.0.1:9/x.wav", "artist": "Artist", "album": "Album", "title": "Title", "track": 0},
	} {
		posted := postCode(t, application, "/v1/audio/publish", body)
		if posted.Code != http.StatusBadRequest {
			t.Fatal(posted.Code, posted.Body.String())
		}
	}
}

func ingestTrack(t *testing.T, application *App, root string, track int, frames int) audiorelease.Result {
	t.Helper()
	incoming, library := filepath.Join(application.Dir, "incoming"), filepath.Join(application.Dir, "library-audio")
	if err := os.MkdirAll(incoming, 0o700); err != nil || os.MkdirAll(library, 0o700) != nil {
		t.Fatal(err)
	}
	name := "song" + strconv.Itoa(track) + ".wav"
	if err := os.WriteFile(filepath.Join(incoming, name), pcmWAV(frames), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("album-audio/" + root + "/track/" + strconv.Itoa(track)))
	result, err := audiorelease.Release(context.Background(), application.engine, audiorelease.Request{
		RequestID: "audio-" + hex.EncodeToString(sum[:16]), RootID: root, Artist: "Artist", Album: "Album",
		Title: "Title " + strconv.Itoa(track), Track: track, SourceName: name, Origin: "local_import",
		Incoming: incoming, Library: library, Decoder: audiorelease.SystemDecoder(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPublishMultiReceipt(t *testing.T) {
	application := newApp(t)
	startSearchJob(t, application, "pub-multi")
	first := ingestTrack(t, application, "pub-multi", 1, 24)
	second := ingestTrack(t, application, "pub-multi", 2, 48)
	posted := postCode(t, application, "/v1/audio/publish", map[string]any{
		"job_id": "pub-multi",
		"receipts": []map[string]any{
			{"track_id": "1", "request_id": first.RequestID, "sha256": first.SHA256, "format": first.Format, "origin": first.Origin, "source_name": first.SourceName, "decode_state": first.DecodeState},
			{"track_id": "2", "request_id": second.RequestID, "sha256": second.SHA256, "format": second.Format, "origin": second.Origin, "source_name": second.SourceName, "decode_state": second.DecodeState},
		},
	})
	if posted.Code != http.StatusOK || !strings.Contains(posted.Body.String(), `"publish_task_id":"stage:pub-multi:publishing"`) {
		t.Fatal(posted.Code, posted.Body.String())
	}
	waitJob(t, application, "pub-multi", "completed")
	for _, name := range []string{"01 Title 1.wav", "02 Title 2.wav"} {
		if _, err := os.Stat(filepath.Join(application.Dir, "library-audio", "Artist", "Album", name)); err != nil {
			t.Fatal(name, err)
		}
	}
}

func TestPublishRejectsTamperedReceipt(t *testing.T) {
	application := newApp(t)
	startSearchJob(t, application, "pub-tamper")
	first := ingestTrack(t, application, "pub-tamper", 1, 24)
	posted := postCode(t, application, "/v1/audio/publish", map[string]any{
		"job_id": "pub-tamper",
		"receipts": []map[string]any{
			{"track_id": "1", "request_id": first.RequestID, "sha256": strings.Repeat("0", 64), "format": first.Format, "origin": first.Origin, "source_name": first.SourceName, "decode_state": first.DecodeState},
		},
	})
	if posted.Code != http.StatusOK {
		t.Fatal(posted.Code, posted.Body.String())
	}
	waitTaskState(t, application, "stage:pub-tamper:publishing", "failed")
	// A publish verification failure fails the job (stage "failed", retryable
	// from review) instead of leaving it in publishing with a stuck task.
	held := inspect(t, application, "pub-tamper")
	if held.Job.Stage != "failed" {
		t.Fatal("tampered publish did not fail the job", held.Job.Stage)
	}
}

func TestPublishMissingTracksQueuesIncompleteAlbumExpansion(t *testing.T) {
	application := newApp(t)
	type followup struct {
		dirs  map[string][]string
		ctxOK bool
	}
	calls := make(chan followup, 1)
	application.missingAlbumFollowup = func(ctx context.Context, dirs map[string][]string) int {
		copied := make(map[string][]string, len(dirs))
		for dir, paths := range dirs {
			copied[dir] = append([]string(nil), paths...)
		}
		calls <- followup{dirs: copied, ctxOK: ctx.Err() == nil}
		return 1
	}
	startSearchJob(t, application, "pub-partial")
	first := ingestTrack(t, application, "pub-partial", 1, 24)
	posted := postCode(t, application, "/v1/audio/publish", map[string]any{
		"job_id": "pub-partial",
		"receipts": []map[string]any{
			{"track_id": "1", "request_id": first.RequestID, "sha256": first.SHA256, "format": first.Format, "origin": first.Origin, "source_name": first.SourceName, "decode_state": first.DecodeState},
		},
		"missing_tracks": []string{"03 Missing Track：source unavailable"},
	})
	if posted.Code != http.StatusOK {
		t.Fatal(posted.Code, posted.Body.String())
	}
	job := waitJob(t, application, "pub-partial", "completed")
	if !strings.Contains(job.Message, "已立即创建 1 个整专补全任务") {
		t.Fatalf("partial completion did not record follow-up: %q", job.Message)
	}
	select {
	case got := <-calls:
		if !got.ctxOK {
			t.Fatal("follow-up received a cancelled context")
		}
		albumDir := filepath.Join(application.Dir, "library-audio", "Artist", "Album")
		paths := got.dirs[albumDir]
		if len(paths) != 1 || filepath.Base(paths[0]) != "01 Title 1.wav" {
			t.Fatalf("follow-up paths = %#v", got.dirs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("missing tracks did not trigger an immediate album expansion")
	}
}

func TestPublishCompleteAlbumSkipsExpansionFollowup(t *testing.T) {
	application := newApp(t)
	called := make(chan struct{}, 1)
	application.missingAlbumFollowup = func(context.Context, map[string][]string) int {
		called <- struct{}{}
		return 1
	}
	startSearchJob(t, application, "pub-complete")
	first := ingestTrack(t, application, "pub-complete", 1, 24)
	posted := postCode(t, application, "/v1/audio/publish", map[string]any{
		"job_id": "pub-complete",
		"receipts": []map[string]any{
			{"track_id": "1", "request_id": first.RequestID, "sha256": first.SHA256, "format": first.Format, "origin": first.Origin, "source_name": first.SourceName, "decode_state": first.DecodeState},
		},
	})
	if posted.Code != http.StatusOK {
		t.Fatal(posted.Code, posted.Body.String())
	}
	waitJob(t, application, "pub-complete", "completed")
	select {
	case <-called:
		t.Fatal("complete album unexpectedly triggered expansion")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestPublishReceiptRequestIDIncludesMissingTracks(t *testing.T) {
	receipts := []publishReceipt{{TrackID: "1", RequestID: "rel-1", SHA256: "digest"}}
	complete, err := publishReceiptRequestID("root", receipts, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstMissing, err := publishReceiptRequestID("root", receipts, []string{"02 Missing"})
	if err != nil {
		t.Fatal(err)
	}
	secondMissing, err := publishReceiptRequestID("root", receipts, []string{"03 Missing"})
	if err != nil {
		t.Fatal(err)
	}
	if complete == firstMissing || firstMissing == secondMissing {
		t.Fatalf("request ids did not include missing tracks: %q %q %q", complete, firstMissing, secondMissing)
	}
}

func TestPublishRecoversPrunedRecordAfterFileRestore(t *testing.T) {
	application := newApp(t)
	startSearchJob(t, application, "pub-recover")
	ingested := ingestTrack(t, application, "pub-recover", 1, 96)
	path, found, err := audiorelease.PathFor(application.engine, ingested.RequestID)
	if err != nil || !found {
		t.Fatal(path, found, err)
	}
	// Reproduce the legacy damaged state directly: the file still exists,
	// but both catalogue keys were removed by the old index-prune bug.
	for _, key := range []string{
		"library/audio/" + ingested.RequestID,
		"library/album-audio/" + ingested.RootID + "/1",
	} {
		var raw []byte
		if err := application.engine.View(func(v *dataengine.View) error {
			value, ok, err := v.Get([]byte(key))
			if err == nil && ok {
				raw = append([]byte(nil), value...)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(raw) == 0 {
			t.Fatalf("catalogue key missing before simulation: %s", key)
		}
		ok, err := application.engine.CompareAndSwap(
			[]dataengine.Condition{{Key: []byte(key), Value: raw}},
			[]dataengine.Mutation{{Key: []byte(key), Delete: true}},
		)
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	if _, found, err := audiorelease.PathFor(application.engine, ingested.RequestID); err != nil || found {
		t.Fatal("legacy damaged receipt was not removed", found, err)
	}
	posted := postCode(t, application, "/v1/audio/publish", map[string]any{
		"job_id": "pub-recover",
		"receipts": []map[string]any{{
			"track_id": "1", "request_id": ingested.RequestID, "sha256": ingested.SHA256,
			"format": ingested.Format, "origin": ingested.Origin, "source_name": ingested.SourceName,
			"decode_state": ingested.DecodeState,
		}},
	})
	if posted.Code != http.StatusOK {
		t.Fatal(posted.Code, posted.Body.String())
	}
	waitJob(t, application, "pub-recover", "completed")
	if _, err := audiorelease.Verify(application.engine, ingested.RequestID); err != nil {
		t.Fatal("recovered receipt did not verify", err)
	}
}
