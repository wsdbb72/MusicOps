package app

import (
	"log"
	"os"
	"path/filepath"
	"sort"
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

type quarantinePlaylistRowIndex struct {
	rows    []playlistTrack
	byTitle map[string][]int
}

func newQuarantinePlaylistRowIndex(rows []playlistTrack) quarantinePlaylistRowIndex {
	index := quarantinePlaylistRowIndex{rows: rows, byTitle: map[string][]int{}}
	for i, row := range rows {
		for _, key := range quarantineTitleKeys(row.Title) {
			index.byTitle[key] = append(index.byTitle[key], i)
		}
	}
	return index
}

func (index quarantinePlaylistRowIndex) candidates(titles ...string) []playlistTrack {
	seen := map[int]bool{}
	out := []playlistTrack{}
	for _, title := range titles {
		for _, key := range quarantineTitleKeys(title) {
			for _, i := range index.byTitle[key] {
				if !seen[i] {
					seen[i] = true
					out = append(out, index.rows[i])
				}
			}
		}
	}
	return out
}

func quarantineTitleKeys(title string) []string {
	folded := foldPlaylistMatchText(title)
	versions := neteaseTitleVersionTokens(folded)
	tokens := make([]string, 0, len(versions))
	for token := range versions {
		tokens = append(tokens, token)
	}
	sort.Strings(tokens)
	normalized := foldTagText(stripNeteaseTitleQualifiers(folded)) + "\x00" + strings.Join(tokens, "\x00")
	return []string{"exact\x00" + folded, "normalized\x00" + normalized}
}

// pruneSuccessfulQuarantineLocked removes quarantine payloads only when the
// successful merge receipt, keeper, hashes, active-task references and the
// current playlist inventory all agree. The caller holds audioCacheMu.
//
// Missing playlist rows are treated as a protection boundary per receipt:
// a move that could satisfy a missing row is retained with its whole receipt.
// Unrelated old receipts can still be reclaimed, which avoids a cache deadlock
// when one playlist row is legitimately absent from the library.
const quarantineActiveReferenceRetention = 6 * time.Hour
const quarantineMoveSettleWindow = 10 * time.Minute

func (a *App) pruneSuccessfulQuarantineLocked(maxAge time.Duration, now time.Time, need int64) (int64, int, error) {
	if a == nil || a.engine == nil || need <= 0 {
		return 0, 0, nil
	}
	trace := os.Getenv("MUSICOPS_CACHE_TRACE") == "1"
	if maxAge < quarantinePruneMinimumAge {
		maxAge = quarantinePruneMinimumAge
	}
	roots := a.quarantineCacheRoots()
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
	rowIndex := newQuarantinePlaylistRowIndex(rows)
	keepers := make(map[string]audiorelease.Entry, len(entries))
	for _, entry := range entries {
		if !entry.FilePresent {
			continue
		}
		key := normalizedKeeperPath(entry.File)
		if _, exists := keepers[key]; !exists {
			keepers[key] = entry
		}
	}
	log.Printf("cache: quarantine reclaim started need=%d rows=%d library_entries=%d", need, len(rows), len(entries))
	var freed int64
	removed := 0
	receipts := 0
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
			receipts++
			if trace {
				log.Printf("cache trace: receipt %d start %s", receipts, entry.Name())
			} else if receipts%50 == 0 {
				log.Printf("cache: quarantine progress receipts=%d freed=%d removed=%d", receipts, freed, removed)
			}
			receiptRoot := filepath.Join(root, entry.Name())
			receipt, found, receiptErr := a.readMergeReceipt(entry.Name())
			if receiptErr != nil {
				return freed, removed, receiptErr
			}
			if !found || receipt.Restored || receipt.CreatedNS <= 0 || receipt.CreatedNS > now.Add(-maxAge).UnixNano() {
				continue
			}
			keeper, keeperOK := mergeKeeperEntry(keepers, receipt.Keep)
			if !keeperOK {
				continue
			}
			var candidateMoves []quarantinePruneCandidate
			protected := false
			protectActiveReferences := receipt.CreatedNS > now.Add(-quarantineActiveReferenceRetention).UnixNano()
			for _, move := range receipt.Moved {
				if trace {
					log.Printf("cache trace: receipt %d move %s", receipts, move.Quarantine)
				}
				if protectActiveReferences && quarantineRequestReferenced(move.RequestID, activeReferences) {
					protected = true
					break
				}
				path := filepath.Clean(move.Quarantine)
				if path == "." || !insideDir(receiptRoot, path) {
					protected = true
					break
				}
				if protectActiveReferences && cachePathReferencedByTasks(path, activeReferences) {
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
				// The receipt is written immediately after the checked move.
				// If the quarantined file has not been modified since then, its
				// bytes are still the hash that moveCheckedFile verified; avoid
				// rereading dozens of gigabytes on every maintenance pass.
				if info.ModTime().After(time.Unix(0, receipt.CreatedNS).Add(quarantineMoveSettleWindow)) {
					digest, hashErr := hashFile(path)
					if hashErr != nil || !strings.EqualFold(digest, move.SHA256) {
						protected = true
						break
					}
				}
				if !move.Sidecar && quarantineMoveNeedsProtection(path, rowIndex, index, keeper) {
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
	log.Printf("cache: quarantine reclaim finished receipts=%d freed=%d removed=%d", receipts, freed, removed)
	return freed, removed, nil
}

func (a *App) quarantineCacheRoots() []string {
	if a == nil {
		return nil
	}
	candidates := []string{}
	for _, root := range []string{a.Dir, a.workDir()} {
		candidates = append(candidates, filepath.Join(root, "quarantine"))
	}
	return normalizeAudioCacheRoots(candidates)
}

func entriesForPlaylistIndex(index playlistLibraryIndex) []audiorelease.Entry {
	out := make([]audiorelease.Entry, 0, len(index.records))
	for _, entry := range index.records {
		out = append(out, entry)
	}
	return out
}
func normalizedKeeperPath(path string) string {
	return strings.ToLower(filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(path)))))
}

func mergeKeeperEntry(entries map[string]audiorelease.Entry, keep string) (audiorelease.Entry, bool) {
	keep = normalizedKeeperPath(keep)
	if keep == "" || keep == "." {
		return audiorelease.Entry{}, false
	}
	entry, found := entries[keep]
	if !found || !entry.PresenceUnchanged() {
		return audiorelease.Entry{}, false
	}
	return entry, true
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
func quarantineMoveNeedsProtection(path string, rows quarantinePlaylistRowIndex, index playlistLibraryIndex, keeper audiorelease.Entry) bool {
	trace := os.Getenv("MUSICOPS_CACHE_TRACE") == "1"
	if trace {
		log.Printf("cache trace: evidence start %s", path)
	}
	artists, album, title, duration := playlistRecordingEvidence(path)
	if trace {
		log.Printf("cache trace: evidence done title=%q candidates=%d", title, len(rows.candidates(title)))
	}
	pathArtist, pathAlbum, pathTitle := trackPathOf(path)
	for _, row := range rows.candidates(title, pathTitle) {
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
