package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
)

var playlistAuditCacheKey = []byte("app/library/playlist-audit-cache-v1")

const (
	playlistAuditCacheVersion = 3
	playlistAuditCacheTTL     = 6 * time.Hour
	playlistAuditCacheEntries = 16
)

type playlistAuditRowState struct {
	Present                bool `json:"present"`
	AlbumPresent           bool `json:"album_present"`
	AlbumIncomplete        bool `json:"album_incomplete"`
	AlbumCompletionNeeded  bool `json:"album_completion_needed"`
	AlbumCompletionUnknown bool `json:"album_completion_unknown"`
}

type playlistAuditSummary struct {
	Total             int `json:"total"`
	Present           int `json:"present"`
	Missing           int `json:"missing"`
	AlbumPresent      int `json:"album_present"`
	AlbumIncomplete   int `json:"album_incomplete"`
	CompletionNeeded  int `json:"completion_needed"`
	CompletionUnknown int `json:"completion_unknown"`
}

type playlistAuditCacheEntry struct {
	PlaylistDigest   string                  `json:"playlist_digest"`
	LibrarySignature string                  `json:"library_signature"`
	States           []playlistAuditRowState `json:"states"`
	Summary          playlistAuditSummary    `json:"summary"`
	CheckedNS        int64                   `json:"checked_ns"`
}

type playlistAuditCacheDocument struct {
	Version int                                `json:"version"`
	Root    string                             `json:"root"`
	Entries map[string]playlistAuditCacheEntry `json:"entries"`
}

func (a *App) loadPlaylistAuditCache(root string) playlistAuditCacheDocument {
	empty := playlistAuditCacheDocument{Version: playlistAuditCacheVersion, Root: filepath.Clean(root), Entries: map[string]playlistAuditCacheEntry{}}
	doc := playlistAuditCacheDocument{}
	found, err := a.loadJSONSetting(playlistAuditCacheKey, &doc)
	if err != nil || !found || doc.Entries == nil || doc.Version != playlistAuditCacheVersion || !strings.EqualFold(filepath.Clean(doc.Root), empty.Root) {
		return empty
	}
	return doc
}

func (a *App) savePlaylistAuditCacheEntry(root, jobID string, entry playlistAuditCacheEntry) error {
	if a == nil || strings.TrimSpace(jobID) == "" {
		return nil
	}
	a.playlistAuditMu.Lock()
	defer a.playlistAuditMu.Unlock()
	doc := a.loadPlaylistAuditCache(root)
	if doc.Entries == nil {
		doc.Entries = map[string]playlistAuditCacheEntry{}
	}
	doc.Entries[jobID] = entry
	cutoff := a.now().Add(-playlistAuditCacheTTL).UnixNano()
	type staleEntry struct {
		id string
		at int64
	}
	stale := make([]staleEntry, 0, len(doc.Entries))
	for id, cached := range doc.Entries {
		if cached.CheckedNS < cutoff || len(cached.States) == 0 {
			delete(doc.Entries, id)
			continue
		}
		stale = append(stale, staleEntry{id: id, at: cached.CheckedNS})
	}
	if len(doc.Entries) > playlistAuditCacheEntries {
		sort.Slice(stale, func(i, j int) bool { return stale[i].at < stale[j].at })
		for _, item := range stale[:len(doc.Entries)-playlistAuditCacheEntries] {
			delete(doc.Entries, item.id)
		}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return a.casSaveJSON(playlistAuditCacheKey, raw)
}

func playlistRowsDigest(name string, rows []playlistTrack) string {
	raw, err := json.Marshal(struct {
		Name string          `json:"name"`
		Rows []playlistTrack `json:"rows"`
	}{Name: name, Rows: rows})
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func playlistLibrarySignature(entries []audiorelease.Entry) string {
	h := sha256.New()
	write := func(values ...string) {
		for _, value := range values {
			_, _ = h.Write([]byte(value))
			_, _ = h.Write([]byte{0})
		}
	}
	for _, entry := range entries {
		write(entry.RequestID, entry.RootID, strconv.Itoa(entry.Track), entry.File, entry.Format, entry.DecodeState, entry.SHA256,
			entry.Origin, entry.SourceName, entry.AliasOfRequestID, strconv.FormatBool(entry.FilePresent))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (entry playlistAuditCacheEntry) valid(playlistDigest, librarySignature string, count int, now time.Time) bool {
	return entry.PlaylistDigest != "" && entry.PlaylistDigest == playlistDigest &&
		entry.LibrarySignature != "" && entry.LibrarySignature == librarySignature &&
		len(entry.States) == count && entry.Summary.Total == count && entry.CheckedNS > 0 &&
		now.Sub(time.Unix(0, entry.CheckedNS)) <= playlistAuditCacheTTL
}
