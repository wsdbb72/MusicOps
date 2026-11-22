// Batch re-scrape (F10). One call identifies every selected album once and
// writes the catalogue's own fields into each track's FLAC tags. It is the
// batch counterpart of the single-file completion and deliberately narrower:
// tags only, never lyrics or covers. Artwork is a per-file decision and stays
// on the single-file path.
package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
	"github.com/wsdbb72/MusicOps/native/core/internal/musicbrainz"
)

const (
	// scrapeBatchMaxFiles bounds one call: every album costs one or two
	// MusicBrainz requests at 1 req/s, so a bigger batch belongs in a
	// background job, not in one HTTP request.
	scrapeBatchMaxFiles = 200
	// scrapeBatchMaxAlbums bounds the lookups independently: 200 files from
	// 200 albums would take minutes, 200 files from 12 albums takes seconds.
	scrapeBatchMaxAlbums = 12
	// scrapeArtworkMaxFiles bounds a batch that also fetches lyrics and
	// covers: two extra reads per file, each with its own deadline.
	scrapeArtworkMaxFiles = 30
)

type scrapeBatchRequest struct {
	Paths          []string `json:"paths"`
	CatalogBaseURL string   `json:"catalog_base_url"`
	Overwrite      bool     `json:"overwrite"`
	DryRun         bool     `json:"dry_run"`
	// WithArtwork also fetches lyrics and the cover from the saved lyrics
	// service. It is off by default and capped: each file costs two extra
	// HTTP reads, so a batch that turns it on is deliberately smaller.
	WithArtwork bool `json:"with_artwork"`
}

// scrapeFileResult is one file's outcome. Status is one of written, planned,
// unchanged, skipped, failed.
type scrapeFileResult struct {
	Path            string        `json:"path"`
	Status          string        `json:"status"`
	Code            string        `json:"code,omitempty"`
	Note            string        `json:"note,omitempty"`
	WrittenFields   []string      `json:"written_fields,omitempty"`
	KeptFields      []tagConflict `json:"kept_fields,omitempty"`
	UnchangedFields []string      `json:"unchanged_fields,omitempty"`
	LockedFields    []string      `json:"locked_fields,omitempty"`
	Lyrics          bool          `json:"lyrics,omitempty"`
	LyricsSidecar   bool          `json:"lyrics_sidecar,omitempty"`
	Cover           bool          `json:"cover,omitempty"`
	ArtworkPlanned  bool          `json:"artwork_planned,omitempty"`
}

type scrapeEntry struct {
	apiPath  string
	full     string
	tags     *flactags.Tags
	duration float64
	artist   string
	album    string
	title    string
}

type scrapeAlbumGroup struct {
	artist, album string
	entries       []scrapeEntry
}

// scrapeLibrary is POST /v1/library/scrape.
func (a *App) scrapeLibrary(w http.ResponseWriter, r *http.Request) {
	var body scrapeBatchRequest
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	paths := []string{}
	seen := map[string]bool{}
	for _, raw := range body.Paths {
		raw = strings.TrimSpace(raw)
		if raw == "" || seen[raw] {
			continue
		}
		seen[raw] = true
		paths = append(paths, raw)
	}
	if len(paths) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "请先勾选要刮削的曲目"})
		return
	}
	if len(paths) > scrapeBatchMaxFiles {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "BATCH_TOO_LARGE", "error": "一次最多刮削 200 首，请分批勾选"})
		return
	}
	if body.WithArtwork && len(paths) > scrapeArtworkMaxFiles {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "ARTWORK_BATCH_TOO_LARGE", "error": "同时取歌词和封面时一次最多 30 首，请分批勾选"})
		return
	}
	client := catalogClient
	if strings.TrimSpace(body.CatalogBaseURL) != "" {
		if !loopbackURL(body.CatalogBaseURL) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "目录地址只接受本机"})
			return
		}
		client = musicbrainz.NewClient()
		client.BaseURL = body.CatalogBaseURL
	}
	results := map[string]*scrapeFileResult{}
	genericPaths := []string{}
	genericRaw := map[string]string{}
	ordered := make([]string, 0, len(paths))
	groups := []*scrapeAlbumGroup{}
	index := map[string]*scrapeAlbumGroup{}
	for _, raw := range paths {
		ordered = append(ordered, raw)
		result := &scrapeFileResult{Path: raw}
		results[raw] = result
		full, err := a.resolveLibraryPath(raw)
		if err != nil {
			result.Status, result.Code, result.Note = "skipped", "UNSUPPORTED", "只处理音乐库里的音频文件"
			continue
		}
		if !strings.EqualFold(filepath.Ext(full), ".flac") {
			genericPaths = append(genericPaths, full)
			genericRaw[full] = raw
			continue
		}
		tags, err := flactags.ReadFile(full)
		if err != nil {
			result.Status, result.Code, result.Note = "skipped", "TAG_READ_FAILED", "标签读不出来"
			continue
		}
		duration, ok := flacDuration(tags.StreamInfo)
		if !ok {
			result.Status, result.Code, result.Note = "skipped", "AUDIO_UNREADABLE", "读不到音频时长，无法与目录核对"
			continue
		}
		artist := strings.TrimSpace(firstNonEmpty(tags.Artist, strings.Join(tags.Artists, "")))
		album := strings.TrimSpace(tags.Album)
		title := strings.TrimSpace(tags.Title)
		inferredArtist, inferredAlbum, inferredTitle := scrapeIdentityFromPath(full)
		artist = firstNonEmpty(artist, inferredArtist)
		album = firstNonEmpty(album, inferredAlbum)
		title = firstNonEmpty(title, inferredTitle)
		if artist == "" || album == "" || title == "" {
			result.Status, result.Code, result.Note = "skipped", "NO_TAGS", "缺艺人、专辑或曲名，且路径不足以推断"
			continue
		}
		key := foldTagText(artist) + "\x00" + foldTagText(album)
		group := index[key]
		if group == nil {
			group = &scrapeAlbumGroup{artist: artist, album: album}
			index[key] = group
			groups = append(groups, group)
		}
		group.entries = append(group.entries, scrapeEntry{
			apiPath: raw, full: full, tags: tags, duration: duration, artist: artist, album: album, title: title,
		})
	}
	if len(groups) > scrapeBatchMaxAlbums {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "BATCH_TOO_LARGE", "error": "一次最多刮削 12 张专辑，请分批勾选"})
		return
	}
	summary := []map[string]any{}
	genreWritten := 0
	lyricsWritten, coverWritten := 0, 0
	lyricsSettings, _ := a.loadLyricsSettings()
	coverMode := coverWriteMode("", lyricsSettings)
	for _, group := range groups {
		tracks := make([]catalogTrackRequest, 0, len(group.entries))
		for _, entry := range group.entries {
			duration := entry.duration
			tracks = append(tracks, catalogTrackRequest{Title: entry.title, Artists: []string{entry.artist}, Duration: &duration})
		}
		lookup, err := identifyCatalogLookup(r.Context(), client, a.engine, catalogIdentifyRequest{
			Artist: group.artist, Album: group.album, Tracks: tracks,
		}, catalogBudget, catalogTTL)
		if err != nil {
			if delay, backoff := catalogBackoffDelay(err); backoff {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{
					"ok": false, "code": "CATALOG_TEMPORARILY_UNAVAILABLE",
					"retry_after_sec": int(delay / time.Second), "error": "音乐目录服务暂时不可用，未写标签",
				})
				return
			}
			for _, entry := range group.entries {
				results[entry.apiPath].Status, results[entry.apiPath].Code = "failed", "CATALOG_UNAVAILABLE"
				results[entry.apiPath].Note = "目录查询失败，文件没有改"
			}
			summary = append(summary, map[string]any{"artist": group.artist, "album": group.album, "confirmed": false, "note": "目录查询失败"})
			continue
		}
		view := lookup.MusicBrainz
		confirmed := view != nil
		if !confirmed {
			for _, entry := range group.entries {
				results[entry.apiPath].Status, results[entry.apiPath].Code = "failed", "CATALOG_UNCONFIRMED"
				results[entry.apiPath].Note = "目录没有确认这张专辑，文件没有改"
			}
			summary = append(summary, map[string]any{"artist": group.artist, "album": group.album, "confirmed": false, "note": "目录没有确认"})
			continue
		}
		albumRow := map[string]any{
			"artist": group.artist, "album": group.album, "confirmed": true,
			"release_id": view.ReleaseID, "genres": view.Genres,
		}
		for i, entry := range group.entries {
			result := results[entry.apiPath]
			confirmedTrack := i >= len(lookup.Results) || lookup.Results[i].IdentityConfirmed
			track, found := matchCatalogTrackForResult(view, entry.title, entry.duration, confirmedTrack)
			if !found {
				result.Status, result.Code, result.Note = "failed", "TRACK_NOT_FOUND", "目录里没有找到对应曲目"
				continue
			}
			updates, completion := planTagWrite(entry.tags, desiredTags(view, track), body.Overwrite)
			var locked []string
			updates, locked = a.applyFieldLocks(filepath.Dir(entry.full), updates)
			result.LockedFields = locked
			if len(locked) > 0 {
				isLocked := map[string]bool{}
				for _, field := range locked {
					isLocked[field] = true
				}
				written := completion.Written[:0]
				for _, name := range completion.Written {
					if !isLocked[name] {
						written = append(written, name)
					}
				}
				completion.Written = written
			}
			// Lyrics and cover are per-file reads against the saved service.
			// A dry run never fetches them: the point of a dry run is to see
			// the plan without spending requests.
			var cover []byte
			// lyricsText is the text that actually goes into the file; it is
			// written both as the LYRICS tag and as "<track>.lrc".
			lyricsText := ""
			if body.WithArtwork {
				if body.DryRun {
					result.ArtworkPlanned = true
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					fetchedLyrics, fetchedCover, artErr := a.fetchArtworkContext(ctx, "", "", entry.artist, entry.album, entry.title)
					cancel()
					if artErr != nil {
						result.Status, result.Code, result.Note = "failed", "ARTWORK_UNAVAILABLE", "歌词或封面没有读到，文件没有改"
						continue
					}
					cover = fetchedCover
					if fetchedLyrics != "" && ((len(entry.tags.Raw["LYRICS"]) == 0 && !hasLyricsSidecar(entry.full)) || body.Overwrite) {
						updates["LYRICS"] = []string{fetchedLyrics}
						lyricsText = fetchedLyrics
						completion.Written = append(completion.Written, "lyrics")
						// A locked lyrics field wins over the fetch, exactly as
						// it wins over the catalogue.
						var extra []string
						updates, extra = a.applyFieldLocks(filepath.Dir(entry.full), updates)
						for _, field := range extra {
							result.LockedFields = append(result.LockedFields, field)
							if field == "lyrics" {
								// The tag and sidecar are one user-visible field. A
								// lock on LYRICS must suppress both writes.
								lyricsText = ""
							}
						}
						if len(extra) > 0 {
							written := completion.Written[:0]
							for _, name := range completion.Written {
								if name != "lyrics" {
									written = append(written, name)
								}
							}
							completion.Written = written
						}
					}
				}
			}
			if body.DryRun {
				result.Status = "planned"
				result.WrittenFields = completion.Written
				result.KeptFields = completion.Kept
				result.UnchangedFields = completion.Unchanged
				if len(completion.Written) == 0 {
					result.Status = "unchanged"
				}
				continue
			}
			if len(updates) == 0 && cover == nil {
				result.Status, result.UnchangedFields = "unchanged", completion.Unchanged
				continue
			}
			// One backup covers both writes: tags first, then the embedded
			// cover, so a failed cover leaves the original file in place.
			backup := ""
			if cover != nil || lyricsText != "" {
				if backup, err = backupFile(entry.full); err != nil {
					result.Status, result.Code, result.Note = "failed", "TAG_UNAVAILABLE", "备份失败，文件没有改"
					continue
				}
			}
			sidecar := strings.TrimSuffix(entry.full, filepath.Ext(entry.full)) + ".jpg"
			sidecarPaths := []string{}
			if lyricsText != "" {
				sidecarPaths = append(sidecarPaths, lyricsSidecarPath(entry.full), entry.full+".lrc")
			}
			if cover != nil {
				sidecarPaths = append(sidecarPaths, sidecar)
			}
			sidecars, snapshotErr := snapshotFiles(sidecarPaths...)
			if snapshotErr != nil {
				if backup != "" {
					_ = os.Remove(backup)
				}
				result.Status, result.Code, result.Note = "failed", "TAG_UNAVAILABLE", "sidecar 备份失败，文件没有改"
				continue
			}
			rollback := func(cause error) (bool, error) {
				restored := true
				resultErr := cause
				if backup != "" {
					restored, resultErr = restoreBackupOnFailure(entry.full, backup, cause)
				}
				if sidecarErr := restoreFileSnapshots(sidecars); sidecarErr != nil {
					return false, errors.Join(resultErr, sidecarErr)
				}
				return restored, resultErr
			}
			fail := func(code string, cause error) {
				restored, resultErr := rollback(cause)
				result.Status, result.Code = "failed", code
				if restored {
					result.Note = "操作失败，原文件已恢复：" + resultErr.Error()
				} else {
					result.Note = "操作失败且恢复未完成；原文件备份已保留：" + resultErr.Error()
				}
			}
			if len(updates) > 0 {
				if err := writeFLACTags(entry.full, updates); err != nil {
					fail("TAG_WRITE_FAILED", err)
					continue
				}
			}
			if cover != nil {
				updated, readErr := os.ReadFile(entry.full)
				if readErr != nil {
					fail("COVER_WRITE_FAILED", readErr)
					continue
				}
				embedded, embedErr := flactags.EmbedFrontCover(updated, cover)
				if embedErr != nil || !bytes.Equal(flactags.FrontCover(embedded), cover) {
					if embedErr == nil {
						embedErr = errors.New("front cover readback mismatch")
					}
					fail("COVER_WRITE_FAILED", embedErr)
					continue
				}
				if err := replaceFileAtomically(entry.full, embedded, 0); err != nil {
					fail("COVER_WRITE_FAILED", err)
					continue
				}
			}
			if lyricsText != "" {
				if err := writeLyricsSidecar(entry.full, lyricsText); err != nil {
					fail("LYRICS_WRITE_FAILED", err)
					continue
				}
			}
			// Apply the cover sidecar only after every operation that can fail
			// on the audio. Atomic sidecar writes preserve an existing jpg when
			// the final rename cannot commit.
			if cover != nil {
				if coverMode == "sidecar" {
					if err := replaceFileAtomically(sidecar, cover, 0o600); err != nil {
						fail("COVER_WRITE_FAILED", err)
						continue
					}
				} else {
					if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
						fail("COVER_WRITE_FAILED", err)
						continue
					}
				}
				result.Cover = true
				coverWritten++
			}
			if backup != "" {
				_ = os.Remove(backup)
			}
			_ = commitFileSnapshots(sidecars)
			result.Status = "written"
			result.WrittenFields = completion.Written
			result.KeptFields = completion.Kept
			for _, field := range completion.Written {
				if field == "genre" {
					genreWritten++
				}
				if field == "lyrics" {
					result.Lyrics = true
					result.LyricsSidecar = true
					lyricsWritten++
				}
			}
		}
		summary = append(summary, albumRow)
	}
	for path, result := range a.scrapeGenericFiles(r.Context(), genericPaths, client, body.Overwrite, body.DryRun, body.WithArtwork) {
		raw := genericRaw[path]
		result.Path = raw
		results[raw] = result
		for _, field := range result.WrittenFields {
			if field == "genre" {
				genreWritten++
			}
		}
		if result.Lyrics {
			lyricsWritten++
		}
		if result.Cover {
			coverWritten++
		}
	}

	files := make([]scrapeFileResult, 0, len(ordered))
	counts := map[string]int{"written": 0, "planned": 0, "unchanged": 0, "skipped": 0, "failed": 0}
	for _, raw := range ordered {
		result := results[raw]
		counts[result.Status]++
		files = append(files, *result)
	}
	note := "批量刮削只写标签；勾选歌词封面后才会从设置里的歌词服务取，一次最多 30 首，歌词同时写成同名 lrc（Roon / Plex / Navidrome 读文件）。"
	if body.DryRun {
		note = "预演：只报告会写哪些字段，文件没有改，也不取歌词封面。"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "dry_run": body.DryRun, "files": files, "albums": summary,
		"written": counts["written"], "planned": counts["planned"], "unchanged": counts["unchanged"],
		"skipped": counts["skipped"], "failed": counts["failed"], "genre_written": genreWritten,
		"with_artwork": body.WithArtwork, "lyrics_written": lyricsWritten, "cover_written": coverWritten,
		"note": note,
	})
}

func scrapeIdentityFromPath(path string) (artist, album, title string) {
	dir := filepath.Dir(path)
	album = strings.TrimSpace(filepath.Base(dir))
	artist = strings.TrimSpace(filepath.Base(filepath.Dir(dir)))
	title = strings.TrimSpace(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	if fields := strings.Fields(title); len(fields) > 1 {
		head := strings.TrimRight(fields[0], ".-_")
		if _, err := strconv.Atoi(head); err == nil {
			title = strings.TrimSpace(strings.Join(fields[1:], " "))
		}
	}
	title = strings.TrimLeft(title, "-_. ")
	return artist, album, title
}
