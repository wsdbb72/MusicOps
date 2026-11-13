package app

// Lyrics sidecar backfill: older builds wrote "<audio>.flac.lrc" (a name Roon,
// Plex and Navidrome do not read) and some files only carry the LYRICS tag.
// This pass fixes both across an existing library:
//
//	standard .lrc present  -> leave it alone (it may be the user's own file)
//	old "<audio>.flac.lrc" -> rename to "<track>.lrc", content untouched
//	LYRICS tag only        -> write "<track>.lrc" from the tag
//
// Nothing is deleted and no audio file is written. A dry run only counts.
//
// A real library takes minutes to walk (one stat per file, plus a FLAC tag
// read for every track without a sidecar), so the pass runs in the background
// and the page polls progress. One pass at a time: a second POST while one is
// running returns the running state instead of starting a competing walk.

import (
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
	"github.com/wsdbb72/MusicOps/native/core/internal/lrc"
)

// lyricsBackfillMaxFiles bounds one pass; a library larger than this is
// processed by starting another pass.
const lyricsBackfillMaxFiles = 200000

type lyricsBackfillReport struct {
	Scanned  int      `json:"scanned"`
	Renamed  int      `json:"renamed"`
	Written  int      `json:"written"`
	Kept     int      `json:"kept"`
	Missing  int      `json:"missing"`
	Skipped  int      `json:"skipped"`
	DryRun   bool     `json:"dry_run"`
	Failed   []string `json:"failed,omitempty"`
	Changed  []string `json:"changed,omitempty"`
	Started  string   `json:"started,omitempty"`
	Finished string   `json:"finished,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// lyricsBackfillRun is the single running (or last finished) pass.
type lyricsBackfillRun struct {
	mu      sync.Mutex
	running bool
	report  lyricsBackfillReport
	started time.Time
}

var lyricsBackfill = &lyricsBackfillRun{}

func (run *lyricsBackfillRun) snapshot() (bool, lyricsBackfillReport) {
	run.mu.Lock()
	defer run.mu.Unlock()
	report := run.report
	if run.started.IsZero() {
		report.Started = ""
	} else {
		report.Started = run.started.Format(time.RFC3339)
	}
	return run.running, report
}

// backfillLyricsSidecars walks the library and updates the shared report as it
// goes, so the page can show progress instead of a request that never answers.
func (a *App) backfillLyricsSidecars(root string, dryRun bool) {
	counted := 0
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		lyricsBackfill.mu.Lock()
		lyricsBackfill.report.Error = "音乐库目录不可用"
		lyricsBackfill.report.Finished = time.Now().Format(time.RFC3339)
		lyricsBackfill.mu.Unlock()
		return
	}
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
		if !audioExtensions[ext] {
			return nil
		}
		counted++
		if counted > lyricsBackfillMaxFiles {
			return fs.SkipAll
		}
		standard := lrc.SidecarPath(path)
		kept := false
		if data, readErr := os.ReadFile(standard); readErr == nil && len(strings.TrimSpace(string(data))) > 0 {
			kept = true
		}
		legacy := path + ".lrc"
		renamed, written, missing, skipped := false, false, false, false
		if !kept {
			if data, readErr := os.ReadFile(legacy); readErr == nil && len(strings.TrimSpace(string(data))) > 0 {
				if dryRun {
					renamed = true
				} else if renameErr := os.Rename(legacy, standard); renameErr != nil {
					skipped = true
					lyricsBackfill.mu.Lock()
					lyricsBackfill.report.Failed = append(lyricsBackfill.report.Failed, legacy+": "+renameErr.Error())
					lyricsBackfill.mu.Unlock()
				} else {
					renamed = true
				}
			} else {
				// An empty .lrc is left alone but does not count as lyrics:
				// otherwise it would block the write forever.
				text := ""
				if ext == "flac" {
					text = lyricsTagOf(path)
				}
				switch {
				case strings.TrimSpace(text) == "":
					missing = true
				case dryRun:
					written = true
				default:
					if writeErr := writeLyricsSidecar(path, text); writeErr != nil {
						skipped = true
						lyricsBackfill.mu.Lock()
						lyricsBackfill.report.Failed = append(lyricsBackfill.report.Failed, path+": "+writeErr.Error())
						lyricsBackfill.mu.Unlock()
					} else {
						written = true
					}
				}
			}
		}
		lyricsBackfill.mu.Lock()
		lyricsBackfill.report.Scanned++
		if kept {
			lyricsBackfill.report.Kept++
		}
		if renamed {
			lyricsBackfill.report.Renamed++
			lyricsBackfill.report.remember(legacy + " -> " + standard)
		}
		if written {
			lyricsBackfill.report.Written++
			lyricsBackfill.report.remember(standard + "（按标签补写）")
		}
		if missing {
			lyricsBackfill.report.Missing++
		}
		if skipped {
			lyricsBackfill.report.Skipped++
		}
		lyricsBackfill.mu.Unlock()
		return nil
	})
	lyricsBackfill.mu.Lock()
	lyricsBackfill.report.Finished = time.Now().Format(time.RFC3339)
	if walkErr != nil {
		lyricsBackfill.report.Error = walkErr.Error()
	}
	lyricsBackfill.mu.Unlock()
}

// lyricsTagOf reads the LYRICS tag without loading the audio. FLAC metadata
// sits at the start of the file, so a 1 MiB prefix is enough for the usual
// case (a truncated prefix that cuts a metadata block fails to parse, and the
// full read is the fallback). On a library of 30 MB tracks this is the
// difference between minutes and an hour.
func lyricsTagOf(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	prefix := make([]byte, 1<<20)
	n, _ := io.ReadFull(file, prefix)
	if tags, parseErr := flactags.ReadBytes(prefix[:n]); parseErr == nil {
		return firstTagValue(tags.Raw, "LYRICS")
	}
	if tags, readErr := flactags.ReadFile(path); readErr == nil {
		return firstTagValue(tags.Raw, "LYRICS")
	}
	return ""
}

func (report *lyricsBackfillReport) remember(line string) {
	if len(report.Changed) < 10 {
		report.Changed = append(report.Changed, line)
	}
}

// startLyricsBackfill is POST /v1/library/lyrics/backfill {dry_run}.
func (a *App) startLyricsBackfill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DryRun bool `json:"dry_run"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	lyricsBackfill.mu.Lock()
	if lyricsBackfill.running {
		report := lyricsBackfill.report
		lyricsBackfill.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": true, "report": report, "note": "已经有一轮在跑"})
		return
	}
	lyricsBackfill.running = true
	lyricsBackfill.started = time.Now()
	lyricsBackfill.report = lyricsBackfillReport{DryRun: body.DryRun}
	lyricsBackfill.mu.Unlock()

	root := a.libraryDir()
	go func() {
		a.backfillLyricsSidecars(root, body.DryRun)
		lyricsBackfill.mu.Lock()
		lyricsBackfill.running = false
		lyricsBackfill.mu.Unlock()
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "running": true, "report": lyricsBackfillReport{DryRun: body.DryRun}})
}

// lyricsBackfillStatus is GET /v1/library/lyrics/backfill.
func (a *App) lyricsBackfillStatus(w http.ResponseWriter, r *http.Request) {
	running, report := lyricsBackfill.snapshot()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": running, "report": report})
}
