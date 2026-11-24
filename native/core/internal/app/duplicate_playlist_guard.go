package app

// Duplicate cleanup must never make an already-present playlist row disappear.
// A recording can have both a clean "Song.flac" and a publisher-prefixed
// "Artist - Song.flac" with the same normalized title. Those files are valid
// quality duplicates, but only one of them may be the copy that satisfies a
// particular playlist row. This guard records the playlist rows each duplicate
// currently proves and refuses a quality merge when the bindings differ.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
)

type duplicatePlaylistGuard struct {
	index    playlistLibraryIndex
	rowsByID map[string][]playlistTrack
}

func (a *App) duplicatePlaylistGuard(entries []audiorelease.Entry) (duplicatePlaylistGuard, error) {
	rows, err := a.maintenancePlaylistRows()
	if err != nil {
		return duplicatePlaylistGuard{}, err
	}
	guard := duplicatePlaylistGuard{rowsByID: map[string][]playlistTrack{}}
	if len(rows) == 0 || len(entries) == 0 {
		return guard, nil
	}
	guard.index = indexPlaylistLibrary(a.engine, entries)
	for _, row := range rows {
		for _, id := range guard.index.playlistCandidateIDs(row) {
			guard.rowsByID[id] = append(guard.rowsByID[id], row)
		}
	}
	return guard, nil
}

// playlistKeys performs media checks only for files that are actually inside
// a duplicate group. Building an eager 4,000-row by 75,000-file matrix would
// add a second whole-library pass before dedupe even starts.
func (guard duplicatePlaylistGuard) playlistKeys(id string) []string {
	rows := guard.rowsByID[id]
	if len(rows) == 0 {
		return nil
	}
	keys := map[string]bool{}
	for _, row := range rows {
		if guard.index.playlistIDMatches(row, id) {
			keys[duplicatePlaylistRowKey(row)] = true
		}
	}
	return sortedStringSet(keys)
}

// maintenancePlaylistRows reads both the saved playlist library and the
// per-job manifests used by fallback. Reading every manifest is intentional:
// dedupe needs the complete set of active identities before it moves a file.
func (a *App) maintenancePlaylistRows() ([]playlistTrack, error) {
	dirs := []string{a.savedPlaylistsDir(), a.workPath("incoming")}
	if legacy := filepath.Join(a.Dir, "incoming"); !sameLibraryPath(legacy, dirs[1]) {
		dirs = append(dirs, legacy)
	}
	seenFiles := map[string]bool{}
	seenRows := map[string]bool{}
	rows := []playlistTrack{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(entry.Name()))
			if ext != ".m3u" && ext != ".m3u8" {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			canonical := strings.ToLower(filepath.Clean(path))
			if seenFiles[canonical] {
				continue
			}
			seenFiles[canonical] = true
			info, statErr := entry.Info()
			if statErr != nil {
				return nil, statErr
			}
			if info.Size() > maxPlaylistBytes {
				return nil, fmt.Errorf("歌单文件超过安全上限：%s", path)
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil, readErr
			}
			parsed, _, parseErr := parseM3U(data)
			if parseErr != nil {
				return nil, fmt.Errorf("歌单无法解析，已阻止去重：%s: %w", path, parseErr)
			}
			for _, row := range parsed {
				key := duplicatePlaylistRowKey(row)
				if seenRows[key] {
					continue
				}
				seenRows[key] = true
				rows = append(rows, row)
			}
		}
	}
	return rows, nil
}

func duplicatePlaylistRowKey(row playlistTrack) string {
	return strings.Join([]string{
		playlistMatchKey(row.Artist), playlistMatchKey(row.Album), playlistMatchKey(row.Title),
		strings.TrimSpace(row.Duration), strconv.FormatInt(row.NeteaseSongID, 10),
		strconv.FormatInt(row.NeteaseAlbumID, 10),
	}, "\x00")
}

// playlistCandidateIDs narrows a playlist row to files that can plausibly
// match by name or album. The final decision still checks bytes-on-disk state,
// embedded identity and duration.
func (index playlistLibraryIndex) playlistCandidateIDs(row playlistTrack) []string {
	seen := map[string]bool{}
	ids := []string{}
	add := func(values []string) {
		for _, id := range values {
			if id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	artist := playlistMatchKey(publishComponent(row.Artist))
	album := playlistMatchKey(publishComponent(row.Album))
	titleKeys := playlistTitleCandidateKeys(row.Title)
	for _, title := range titleKeys {
		add(index.titleCandidates[title])
		add(index.titleCandidates[playlistCompactTitleKey(title)])
		pathAlbum := firstNonEmpty(album, playlistMatchKey(row.Title))
		add(index.tracks[[3]string{artist, pathAlbum, title}])
	}
	if album != "" {
		add(index.albums[[2]string{artist, album}])
	}
	// Album members are a fallback for the migration compilation whose
	// uploader metadata and path titles often differ from the source row.
	// Eagerly attaching a multi-thousand-track album to every row would grow
	// with rows x files, so use it only when the title and exact album indexes
	// produced nothing.
	if len(ids) == 0 && isHotSlowCompilationAlbum(row.Album) {
		add(index.albumMembers[playlistMatchKey(hotSlowCompilationAlbum)])
	}
	sort.Strings(ids)
	return ids
}

func (index playlistLibraryIndex) playlistIDMatches(row playlistTrack, id string) bool {
	if !index.isUnchanged(id) {
		return false
	}
	entry, found := index.records[id]
	if found {
		pathArtist, pathAlbum, pathTitle := trackPathOf(entry.File)
		if playlistArtistFallbackMatches(row.Artist, pathArtist) &&
			(strings.TrimSpace(row.Album) == "" || playlistMatchKey(row.Album) == playlistMatchKey(pathAlbum)) &&
			neteaseAlternateTitleMatches(row.Title, pathTitle) {
			return true
		}
	}
	path, found, err := audiorelease.PathFor(index.engine, id)
	if err != nil || !found {
		return false
	}
	tags := index.recording(path)
	pathArtist, pathAlbum, pathTitle := trackPathOf(path)
	title := tags.title
	if strings.TrimSpace(title) == "" {
		title = pathTitle
	}
	if !neteaseAlternateTitleMatches(row.Title, title) {
		return false
	}
	artistMatches := playlistArtistTagMatches(row.Artist, tags.artists) || playlistArtistFallbackMatches(row.Artist, pathArtist)
	if !artistMatches {
		return false
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(row.Duration), 64)
	if err != nil || !(seconds > 0 && seconds <= 24*3600) || tags.duration <= 0 {
		return false
	}
	albumMatches := strings.TrimSpace(row.Album) == "" || playlistAlbumTagMatches(row.Album, tags.album) ||
		playlistMatchKey(row.Album) == playlistMatchKey(pathAlbum)
	tolerance := int64(1500)
	if albumMatches {
		tolerance = 3000
	}
	durationMS := int64(seconds * 1000)
	delta := int64(tags.duration*1000) - durationMS
	if delta < 0 {
		delta = -delta
	}
	return delta <= tolerance
}

func (guard duplicatePlaylistGuard) annotate(group *duplicateGroup) {
	if group == nil || group.State != "ready" || len(guard.rowsByID) == 0 {
		return
	}
	baseline := ""
	for i := range group.Files {
		keys := guard.playlistKeys(group.Files[i].RequestID)
		group.Files[i].PlaylistKeys = keys
		joined := strings.Join(keys, "\n")
		if i == 0 {
			baseline = joined
			continue
		}
		if joined != baseline {
			group.Keep = nil
			group.State = "review"
			group.Reason = "这些文件对应的歌单引用身份不同；为避免已入库曲目从歌单消失，原文件全部保留"
			return
		}
	}
}

func sortedStringSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
