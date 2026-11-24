package app

import (
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
func (a *App) pruneSuccessfulQuarantineLocked(maxAge time.Duration, now time.Time, need int64) (int64, int, error) {
	if a == nil || a.engine == nil || need <= 0 {
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
	var freed int64
	removed := 0
	touched := map[string]struct{}{}
reclaimLoop:
	for _, root := range roots {
		dirEntries, readErr := os.ReadDir(root)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			return freed, removed, readErr
		}
		for _, entry := range dirEntries {
			if freed >= need {
				break reclaimLoop
			}
			if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "merge-") {
				continue
			}
			receiptRoot := filepath.Join(root, entry.Name())
			receipt, found, receiptErr := a.readMergeReceipt(entry.Name())
			if receiptErr != nil {
				return freed, removed, receiptErr
			}
			if !found || receipt.Restored || receipt.CreatedNS <= 0 || receipt.CreatedNS > now.Add(-maxAge).UnixNano() {
				continue
			}
			keeper, keeperOK := mergeKeeperEntry(entries, receipt.Keep)
			if !keeperOK {
				continue
			}
			var candidateMoves []quarantinePruneCandidate
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
				if !move.Sidecar && quarantineMoveNeedsProtection(path, rows, index, keeper) {
					protected = true
					break
				}
				candidateMoves = append(candidateMoves, quarantinePruneCandidate{path: path, size: info.Size(), mod: info.ModTime()})
			}
			if protected || len(candidateMoves) == 0 {
				continue
			}
			for _, item := range candidateMoves {
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
				if removeErr := os.Remove(item.path); removeErr != nil {
					if !os.IsNotExist(removeErr) {
						return freed, removed, removeErr
					}
					continue
				}
				freed += item.size
				removed++
				touched[filepath.Dir(item.path)] = struct{}{}
			}
			if freed >= need {
				break reclaimLoop
			}
		}
	}
	pruneEmptyCacheParents(roots, touched)
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
func mergeKeeperEntry(entries []audiorelease.Entry, keep string) (audiorelease.Entry, bool) {
	keep = strings.ToLower(filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(keep)))))
	if keep == "" || keep == "." {
		return audiorelease.Entry{}, false
	}
	for _, entry := range entries {
		if entry.FilePresent && entry.PresenceUnchanged() && strings.EqualFold(filepath.ToSlash(entry.File), keep) {
			return entry, true
		}
	}
	return audiorelease.Entry{}, false
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

// quarantineMoveNeedsProtection checks only playlist rows that can actually
// match this quarantined recording. The old implementation verified all 4,000+
// playlist rows before every cache pass, then searched the entire library for
// each match even though the receipt already names the surviving keeper.
func quarantineMoveNeedsProtection(path string, rows []playlistTrack, index playlistLibraryIndex, keeper audiorelease.Entry) bool {
	artists, album, title, duration := playlistRecordingEvidence(path)
	pathArtist, pathAlbum, pathTitle := trackPathOf(path)
	for _, row := range rows {
		if !quarantineMoveMatchesRow(row, artists, album, title, duration, pathArtist, pathAlbum, pathTitle) {
			continue
		}
		// The receipt already identifies the one surviving file for this
		// merge. Match that keeper directly instead of searching every
		// title/album candidate in the 75k-file library for each move.
		if !index.playlistIDMatches(row, keeper.RequestID) {
			return true
		}
	}
	return false
}

func quarantineMoveMatchesRow(row playlistTrack, artists []string, album, title string, duration float64, pathArtist, pathAlbum, pathTitle string) bool {
	actualTitle := title
	if strings.TrimSpace(actualTitle) == "" {
		actualTitle = pathTitle
	}
	if !neteaseAlternateTitleMatches(row.Title, actualTitle) && !neteaseAlternateTitleMatches(row.Title, pathTitle) {
		return false
	}
	if !playlistArtistTagMatches(row.Artist, artists) && !playlistArtistFallbackMatches(row.Artist, pathArtist) {
		return false
	}
	if strings.TrimSpace(row.Album) != "" && !playlistAlbumTagMatches(row.Album, album) && playlistMatchKey(row.Album) != playlistMatchKey(pathAlbum) {
		return false
	}
	seconds, parseErr := strconv.ParseFloat(strings.TrimSpace(row.Duration), 64)
	if parseErr != nil || !(seconds > 0 && seconds <= 24*3600) || duration <= 0 {
		return true
	}
	delta := int64(duration*1000) - int64(seconds*1000)
	if delta < 0 {
		delta = -delta
	}
	return delta <= 3000
}
