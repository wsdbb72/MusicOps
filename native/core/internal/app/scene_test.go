package app

import (
	"net/http"
	"strings"
	"testing"
)

// "补全" was offered by the search page but the entrance always started at
// "identifying", which planStart refuses for scene complete, so the user got a
// bare 409 and no job. The scene now says why it cannot run (P1-1).
func TestCompleteSceneIsRefusedWithAReason(t *testing.T) {
	application := newApp(t)
	body := searchBody("album-complete", "http://127.0.0.1:9", []string{"dicmusic"}, false)
	body["scene"] = "complete"
	body["source_track_ids"] = []string{}
	recorder := postRaw(t, application, "/v1/search", body)
	if recorder.Code != http.StatusUnprocessableEntity || !containsAll(recorder.Body.String(), "SCENE_UNSUPPORTED", "尚未接入") {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	if job := inspect(t, application, "album-complete"); job.Job.ID != "" {
		t.Fatal("a refused scene created a job")
	}
}

func containsAll(text string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(text, needle) {
			return false
		}
	}
	return true
}
