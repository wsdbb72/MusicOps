package app

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/wavflac"
)

func TestWAVTagsAreReadBeforeAndAfterImport(t *testing.T) {
	application := newApp(t)
	post(t, application, "/v1/search", searchBody("board-wav", "http://127.0.0.1:9", []string{}, false))
	if job := waitJob(t, application, "search:board-wav", "needs_review"); job.Stage != "needs_review" {
		t.Fatal(job.Stage)
	}
	incoming := filepath.Join(application.Dir, "incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(incoming, "notags.wav")
	withID3 := filepath.Join(incoming, "id3.wav")
	withInfo := filepath.Join(incoming, "info.wav")
	if err := os.WriteFile(bare, pcmWAV(32), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(withID3, wavWithID3(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(withInfo, wavWithINFO(), 0o600); err != nil {
		t.Fatal(err)
	}
	turnedOff := post(t, application, "/v1/settings/wav", map[string]any{"auto_convert_wav": false, "wav_disposal": "keep"})
	note, _ := turnedOff["note"].(string)
	if turnedOff["ok"] != true || !strings.Contains(note, "当前已关闭自动转换") {
		t.Fatal(turnedOff)
	}

	bareBefore := mustCompare(t, application, "notags.wav")
	id3Before := mustCompare(t, application, "id3.wav")
	infoBefore := mustCompare(t, application, "info.wav")
	if bareBefore.Converted || !bareBefore.Before.NeedsReview || !bareBefore.After.NeedsReview || bareBefore.Before.FromFile || bareBefore.After.Format != "wav" {
		t.Fatalf("untagged wav was treated as identified: %+v", bareBefore)
	}
	if id3Before.Converted || id3Before.Before.NeedsReview || id3Before.After.NeedsReview || id3Before.Before.TagSource != "id3" || id3Before.Before.Artist != "Ada" || id3Before.Before.Album != "First" || id3Before.Before.Title != "Opening" || id3Before.Before.IdentityConfirmed {
		t.Fatalf("id3 wav was sent to review before conversion: %+v", id3Before)
	}
	if infoBefore.Converted || infoBefore.Before.NeedsReview || infoBefore.After.NeedsReview || infoBefore.Before.TagSource != "info" || infoBefore.Before.Artist != "Ada" || infoBefore.Before.Album != "First" || infoBefore.Before.Title != "Opening" {
		t.Fatalf("info wav was sent to review before conversion: %+v", infoBefore)
	}
	tags := getAuth(t, application, "/v1/library/tags?path="+withID3)
	if tags.Code != http.StatusOK || !strings.Contains(tags.Body.String(), `"tag_source":"id3"`) || !strings.Contains(tags.Body.String(), `"identity_confirmed":false`) || strings.Contains(tags.Body.String(), `"identity_confirmed":true`) {
		t.Fatal(tags.Code, tags.Body.String())
	}

	saved := post(t, application, "/v1/settings/wav", map[string]any{"auto_convert_wav": true, "wav_disposal": "keep"})
	if saved["ok"] != true {
		t.Fatal(saved)
	}
	bareAfter := mustCompare(t, application, "notags.wav")
	id3After := mustCompare(t, application, "id3.wav")
	infoAfter := mustCompare(t, application, "info.wav")
	if !bareAfter.Converted || bareAfter.After.Format != "flac" || !bareAfter.After.NeedsReview || bareAfter.After.FromFile || bareAfter.After.IdentityConfirmed {
		t.Fatalf("tagless wav was confirmed after conversion: %+v", bareAfter)
	}
	if !id3After.Converted || id3After.After.Format != "flac" || id3After.After.NeedsReview || id3After.After.Artist != "Ada" || id3After.After.TagSource == "" || id3After.After.IdentityConfirmed {
		t.Fatalf("id3 tags did not survive conversion: %+v", id3After)
	}
	if !infoAfter.Converted || infoAfter.After.NeedsReview || infoAfter.After.Album != "First" || infoAfter.After.Title != "Opening" {
		t.Fatalf("info tags did not survive conversion: %+v", infoAfter)
	}
	for _, name := range []string{"notags", "id3", "info"} {
		wavPath := filepath.Join(incoming, name+".wav")
		flacPath := filepath.Join(incoming, name+".flac")
		wavHash, err := wavflac.PCMHash(wavPath)
		if err != nil {
			t.Fatal(err)
		}
		flacHash, err := wavflac.PCMHash(flacPath)
		if err != nil || wavHash != flacHash {
			t.Fatal(name, err, wavHash, flacHash)
		}
		if _, err := os.Stat(wavPath); err != nil {
			t.Fatal("source wav was removed", name)
		}
	}

	refused := postCode(t, application, "/v1/audio/release", map[string]any{
		"job_id": "board-wav", "artist": "", "album": "", "title": "", "track": 1, "source_name": "notags.wav",
	})
	if refused.Code != http.StatusUnprocessableEntity || !strings.Contains(refused.Body.String(), `"needs_review":true`) || !strings.Contains(refused.Body.String(), `"library_published":false`) || strings.Contains(refused.Body.String(), `"downloaded":true`) {
		t.Fatal(refused.Code, refused.Body.String())
	}
	if _, err := os.Stat(filepath.Join(application.libraryDir(), "Ada")); !os.IsNotExist(err) {
		t.Fatal("tagless wav was imported", err)
	}
	released := postCode(t, application, "/v1/audio/release", map[string]any{
		"job_id": "board-wav", "artist": "", "album": "", "title": "", "track": 1, "source_name": "id3.wav",
	})
	if released.Code != http.StatusOK || !strings.Contains(released.Body.String(), `"format":"flac"`) || !strings.Contains(released.Body.String(), `"needs_review":false`) || !strings.Contains(released.Body.String(), `"identity_confirmed":false`) || !strings.Contains(released.Body.String(), `"library_published":true`) || strings.Contains(released.Body.String(), `"downloaded":true`) {
		t.Fatal(released.Code, released.Body.String())
	}
	published := filepath.Join(application.libraryDir(), "Ada", "First", "01 Opening.flac")
	if _, err := os.Stat(published); err != nil {
		t.Fatal(err)
	}
}

func mustCompare(t *testing.T, application *App, name string) identityCompare {
	t.Helper()
	got, err := application.compareIncomingIdentity(filepath.Join(application.Dir, "incoming"), name)
	if err != nil {
		t.Fatal(name, err)
	}
	return got
}

func wavWithID3() []byte {
	frames := id3Frames([][2]string{{"TPE1", "Ada"}, {"TALB", "First"}, {"TIT2", "Opening"}})
	tag := make([]byte, 10+len(frames))
	copy(tag, "ID3")
	tag[3] = 3
	size := len(frames)
	tag[6] = byte(size >> 21)
	tag[7] = byte(size >> 14)
	tag[8] = byte(size >> 7)
	tag[9] = byte(size)
	copy(tag[10:], frames)
	return appendWAVChunk(pcmWAV(32), "id3 ", tag)
}

func wavWithINFO() []byte {
	info := []byte("INFO")
	info = appendINFO(info, "IART", "Ada")
	info = appendINFO(info, "IPRD", "First")
	info = appendINFO(info, "INAM", "Opening")
	return appendWAVChunk(pcmWAV(32), "LIST", info)
}

func appendINFO(body []byte, id, value string) []byte {
	raw := append([]byte(value), 0)
	body = append(body, []byte(id)...)
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(raw)))
	body = append(body, size[:]...)
	body = append(body, raw...)
	if len(raw)%2 == 1 {
		body = append(body, 0)
	}
	return body
}

func id3Frames(fields [][2]string) []byte {
	var body []byte
	for _, field := range fields {
		payload := append([]byte{0}, []byte(field[1])...)
		frame := make([]byte, 10+len(payload))
		copy(frame, field[0])
		binary.BigEndian.PutUint32(frame[4:], uint32(len(payload)))
		copy(frame[10:], payload)
		body = append(body, frame...)
	}
	return body
}

func appendWAVChunk(wav []byte, id string, payload []byte) []byte {
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(payload)))
	wav = append(wav, []byte(id)...)
	wav = append(wav, size[:]...)
	wav = append(wav, payload...)
	if len(payload)%2 == 1 {
		wav = append(wav, 0)
	}
	return wav
}

func TestWAVAutoConvertDefaultsOnAndKeepsAnExplicitOff(t *testing.T) {
	application := newApp(t)
	shown := getAuth(t, application, "/v1/settings/wav")
	if shown.Code != http.StatusOK || !strings.Contains(shown.Body.String(), `"auto_convert_wav":true`) || !strings.Contains(shown.Body.String(), `"wav_disposal":"keep"`) || !strings.Contains(shown.Body.String(), "默认开启") || !strings.Contains(shown.Body.String(), "原 WAV 默认保留") || !strings.Contains(shown.Body.String(), "当前已打开自动转换") {
		t.Fatal(shown.Code, shown.Body.String())
	}
	incoming := filepath.Join(application.Dir, "incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(incoming, "plain.wav")
	if err := os.WriteFile(source, pcmWAV(16), 0o600); err != nil {
		t.Fatal(err)
	}
	opened := mustCompare(t, application, "plain.wav")
	if !opened.Converted || opened.After.Format != "flac" {
		t.Fatalf("default did not convert before identify: %+v", opened)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("default disposal removed the wav", err)
	}
	if _, err := os.Stat(filepath.Join(incoming, "plain.flac")); err != nil {
		t.Fatal(err)
	}

	if err := application.casSaveJSON(wavSettingsKey(), []byte(`{"wav_disposal":"recycle"}`)); err != nil {
		t.Fatal(err)
	}
	if got := application.loadWAVSettings(); !got.Auto || got.Disposal != "recycle" {
		t.Fatalf("a document without the switch was not left on: %+v", got)
	}
	if err := application.casSaveJSON(wavSettingsKey(), []byte(`{"auto_convert_wav":false,"wav_disposal":"keep"}`)); err != nil {
		t.Fatal(err)
	}
	if got := application.loadWAVSettings(); got.Auto || got.Disposal != "keep" {
		t.Fatalf("an explicit off was turned back on: %+v", got)
	}
	held := filepath.Join(incoming, "held.wav")
	if err := os.WriteFile(held, pcmWAV(16), 0o600); err != nil {
		t.Fatal(err)
	}
	closed := mustCompare(t, application, "held.wav")
	if closed.Converted || closed.After.Format != "wav" {
		t.Fatalf("explicit off still converted: %+v", closed)
	}
	if _, err := os.Stat(filepath.Join(incoming, "held.flac")); !os.IsNotExist(err) {
		t.Fatal("explicit off wrote a flac", err)
	}
}

func TestWAVBatchReportsProgressAndDoesNotClaimDownload(t *testing.T) {
	application := newApp(t)
	dir := filepath.Join(application.Dir, "incoming", "batch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "one.wav"), pcmWAV(16), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "two.wav"), pcmWAV(24), 0o600); err != nil {
		t.Fatal(err)
	}
	started := postCode(t, application, "/v1/library/transcode/batch", map[string]any{"dir": dir, "request_id": "wavbatch"})
	if started.Code != http.StatusAccepted || !strings.Contains(started.Body.String(), `"count":2`) || !strings.Contains(started.Body.String(), `"downloaded":false`) {
		t.Fatal(started.Code, started.Body.String())
	}
	var accepted struct {
		BatchID string   `json:"batch_id"`
		TaskIDs []string `json:"task_ids"`
	}
	if err := json.Unmarshal(started.Body.Bytes(), &accepted); err != nil || accepted.BatchID == "" || len(accepted.TaskIDs) != 2 {
		t.Fatal(err, started.Body.String())
	}
	for _, id := range accepted.TaskIDs {
		waitTaskState(t, application, id, "completed")
	}
	status := getAuth(t, application, "/v1/library/transcode/batch?batch_id="+accepted.BatchID)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"downloaded":false`) || !strings.Contains(status.Body.String(), `"completed":2`) {
		t.Fatal(status.Code, status.Body.String())
	}
	paused := postCode(t, application, "/v1/library/transcode/batch/control", map[string]any{"batch_id": accepted.BatchID, "action": "pause"})
	if paused.Code != http.StatusOK || !strings.Contains(paused.Body.String(), "已完成的文件不会重做") || strings.Contains(paused.Body.String(), `"downloaded":true`) {
		t.Fatal(paused.Code, paused.Body.String())
	}
	for _, name := range []string{"one.flac", "two.flac"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, strings.TrimSuffix(name, ".flac")+".wav")); err != nil {
			t.Fatal("original removed", err)
		}
	}
}
