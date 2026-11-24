package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
)

const quarantinePruneMinimumAge = time.Hour

type quarantinePruneCandidate struct {
	path string
	size int64
	mod  time.Time
}

// pruneSuccessfulQuarantineLocked removes quarantine payloads only when the
// successful merge receipt, keeper, hashes, active-task references and the
// current playlist inventory all agree. The caller holds audioCacheMu.
//
// Missing playlist rows are treated as a protection boundary per receipt:
// a move that could satisfy a missing row is retained with its whole receipt.
// Unrelated old receipts can still be reclaimed, which avoids a cache deadlock
// when one playlist row is legitimately absent from the library.
func (a *App) pruneSuccessfulQuarantineLocked(maxAge time.Duration, now time.Time) (int64, int, error) {
	if a == nil || a.engine == nil {
		return 0, 0, nil
	}
	if maxAge < quarantinePruneMinimumAge {
		maxAge = quarantinePruneMinimumAge
	}
	roots := quarantineCacheRoots(a.audioCacheRoots(""))
	if len(roots) == 0 {
		return 0, 0, nil
	}
	rows, err := a.maintenancePlaylistRows()
	if err != nil {
		return 0, 0, err
	}
	if len(rows) == 0 {
		return 0, 0, nil
	}
	activeReferences, err := a.activeAudioCacheReferences()
	if err != nil {
		return 0, 0, err
	}
	index := a.playlistLibrary()
	entries := entriesForPlaylistIndex(index)
	if len(entries) == 0 {
		return 0, 0, nil
	}
	present, err := playlistPresenceForIndexContext(context.Background(), index, rows)
	if err != nil {
		return 0, 0, err
	}
	missing := make([]playlistTrack, 0)
	for i, row := range rows {
		if i >= len(present) || !present[i] {
			missing = append(missing, row)
		}
	}

	type receiptCandidate struct {
		moves []quarantinePruneCandidate
	}
	var receipts []receiptCandidate
	for _, root := range roots {
		dirEntries, readErr := os.ReadDir(root)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			return 0, 0, readErr
		}
		for _, entry := range dirEntries {
			if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "merge-") {
				continue
			}
			receiptRoot := filepath.Join(root, entry.Name())
			receipt, found, receiptErr := a.readMergeReceipt(entry.Name())
			if receiptErr != nil {
				return 0, 0, receiptErr
			}
			if !found || receipt.Restored || receipt.CreatedNS <= 0 || receipt.CreatedNS > now.Add(-maxAge).UnixNano() {
				continue
			}
			if !mergeKeeperPresent(entries, receipt.Keep) {
				continue
			}
			candidate := receiptCandidate{}
			protected := false
			for _, move := range receipt.Moved {
				if quarantineRequestReferenced(move.RequestID, activeReferences) {
					protected = true
					break
				}
				path := filepath.Clean(move.Quarantine)
				if path == "." || !insideDir(receiptRoot, path) {
					protected = true
					break
				}
				if cachePathReferencedByTasks(path, activeReferences) {
					protected = true
					break
				}
				info, statErr := os.Lstat(path)
				if statErr != nil {
					if os.IsNotExist(statErr) {
						continue
					}
					protected = true
					break
				}
				if !info.Mode().IsRegular() {
					protected = true
					break
				}
				digest, hashErr := hashFile(path)
				if hashErr != nil || !strings.EqualFold(digest, move.SHA256) {
					protected = true
					break
				}
				if !move.Sidecar && quarantineMoveMatchesMissing(path, missing) {
					protected = true
					break
				}
				candidate.moves = append(candidate.moves, quarantinePruneCandidate{path: path, size: info.Size(), mod: info.ModTime()})
			}
			if protected || len(candidate.moves) == 0 {
				continue
			}
			receipts = append(receipts, candidate)
		}
	}
	var freed int64
	removed := 0
	for _, candidate := range receipts {
		for _, item := range candidate.moves {
			latest, statErr := os.Lstat(item.path)
			if statErr != nil {
				if os.IsNotExist(statErr) {
					continue
				}
				return freed, removed, statErr
			}
			if !latest.Mode().IsRegular() || latest.Size() != item.size || !latest.ModTime().Equal(item.mod) {
				continue
			}
			if removeErr := os.Remove(item.path); removeErr != nil && !os.IsNotExist(removeErr) {
				return freed, removed, removeErr
			}
			freed += item.size
			removed++
		}
	}
	pruneEmptyCacheDirs(roots)
	return freed, removed, nil
}

func quarantineCacheRoots(roots []string) []string {
	out := []string{}
	for _, root := range normalizeAudioCacheRoots(roots) {
		if strings.EqualFold(filepath.Base(root), "quarantine") {
			out = append(out, root)
		}
	}
	return out
}

func entriesForPlaylistIndex(index playlistLibraryIndex) []audiorelease.Entry {
	out := make([]audiorelease.Entry, 0, len(index.records))
	for _, entry := range index.records {
		out = append(out, entry)
	}
	return out
}
func mergeKeeperPresent(entries []audiorelease.Entry, keep string) bool {
	keep = strings.ToLower(filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(keep)))))
	if keep == "" || keep == "." {
		return false
	}
	for _, entry := range entries {
		if entry.FilePresent && entry.PresenceUnchanged() && strings.EqualFold(filepath.ToSlash(entry.File), keep) {
			return true
		}
	}
	return false
}

func quarantineRequestReferenced(request string, references []string) bool {
	request = normalizeCacheReference(request)
	if request == "" {
		return false
	}
	for _, reference := range references {
		if reference != "" && (strings.Contains(request, reference) || strings.Contains(reference, request)) {
			return true
		}
	}
	return false
}
func quarantineMoveMatchesMissing(path string, missing []playlistTrack) bool {
	if len(missing) == 0 {
		return false
	}
	artists, album, title, duration := playlistRecordingEvidence(path)
	pathArtist, pathAlbum, pathTitle := trackPathOf(path)
	for _, row := range missing {
		actualTitle := title
		if strings.TrimSpace(actualTitle) == "" {
			actualTitle = pathTitle
		}
		if !neteaseAlternateTitleMatches(row.Title, actualTitle) && !neteaseAlternateTitleMatches(row.Title, pathTitle) {
			continue
		}
		artistMatches := playlistArtistTagMatches(row.Artist, artists) || playlistArtistFallbackMatches(row.Artist, pathArtist)
		if !artistMatches {
			continue
		}
		albumMatches := strings.TrimSpace(row.Album) == "" || playlistAlbumTagMatches(row.Album, album) || playlistMatchKey(row.Album) == playlistMatchKey(pathAlbum)
		if !albumMatches {
			continue
		}
		seconds, parseErr := strconv.ParseFloat(strings.TrimSpace(row.Duration), 64)
		if parseErr != nil || !(seconds > 0 && seconds <= 24*3600) || duration <= 0 {
			return true
		}
		delta := int64(duration*1000) - int64(seconds*1000)
		if delta < 0 {
			delta = -delta
		}
		if delta <= 3000 {
			return true
		}
	}
	return false
}
