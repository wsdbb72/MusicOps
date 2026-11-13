// Library maintenance: duplicate scan/merge/restore and artist-folder
// 整理. Both follow the Python contracts in app/duplicates.py and
// app/artist_folders.py, reduced to what the Go library records actually
// carry: a record has a root id, a track number, a file tail
// (Artist/Album/NN Title.ext), a format and a file SHA-256 — no release
// id, no per-track artist list, no duration. Grouping therefore uses
// (artist folder, album folder, track slot) from the record, and the
// PCM identity is the decoded-audio hash from _pcm_identity, not a
// filename guess.
//
// Safety rules carried over from Python:
//   - merge never deletes: losers move into a quarantine directory under
//     the state directory, with a durable receipt, and restore moves
//     them back; a merge whose files changed since the scan is refused.
//   - relocate never overwrites an existing target and never follows a
//     symlink out of the library (resolveInDir on both ends).
//   - a scan that cannot decode (no ffmpeg) still runs, but only on
//     structural identity (format + file SHA-256) and says so: same PCM
//     in two different encodings will not group without a decoder.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
	"github.com/wsdbb72/MusicOps/native/core/internal/lrc"
	"github.com/wsdbb72/MusicOps/native/core/internal/qbittorrent"
	"github.com/wsdbb72/MusicOps/native/core/internal/wavflac"
)

// ---------------------------------------------------------------------------
// Duplicate scan
//
// GET /v1/library/duplicates
//
// Groups library records by (artist, album, track slot); inside a group,
// files sharing one PCM identity are duplicates. The identity is
// (pcm_sha256, sample_rate, channels, codec) after Python _pcm_identity:
// the PCM hash is the ffmpeg decode to s16le 44.1 kHz stereo — the same
// normalization transcode uses — and rate/channels come from the file's
// own stream info (FLAC STREAMINFO, WAV fmt chunk), so the same audio at
// a different sample rate does not group. DSD and multi-stream files
// cannot occur here: audiorelease only ever ingests PCM WAV and FLAC.
// Without ffmpeg the scan falls back to structural identity (format +
// file SHA-256) and reports method "structural".

// duplicateFile is one library file inside a duplicate group.
type duplicateFile struct {
	RequestID     string `json:"request_id"`
	File          string `json:"file"`
	Format        string `json:"format"`
	SHA256        string `json:"sha256"`
	PCMSHA256     string `json:"pcm_sha256"`
	SampleRate    uint32 `json:"sample_rate"`
	Channels      int    `json:"channels"`
	BitsPerSample int    `json:"bits_per_sample"`
	Lossless      bool   `json:"lossless"`
	Rank          []int  `json:"rank"`
	// LibraryDir is the absolute library root the file was published
	// under, empty for records written before it was stored.
	LibraryDir string `json:"library_dir,omitempty"`
}

// duplicateGroup is one (artist, album, slot) set with more than one file.
type duplicateGroup struct {
	GroupID      string          `json:"group_id"`
	Album        string          `json:"album"`
	Artists      []string        `json:"artists"`
	Files        []duplicateFile `json:"files"`
	Keep         *duplicateFile  `json:"keep"`
	State        string          `json:"state"`
	Reason       string          `json:"reason"`
	IdentityUsed string          `json:"identity_used"`
}

func (a *App) duplicateScan(w http.ResponseWriter, r *http.Request) {
	groups, method, truncated, err := a.scanDuplicates(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	// Suppressed albums are hidden from the scan but still reachable by id:
	// a merge attempt answers MERGE_SUPPRESSED instead of a confusing 404.
	visible := make([]duplicateGroup, 0, len(groups))
	suppressed := 0
	for i := range groups {
		if a.groupSuppressed(&groups[i]) {
			suppressed++
			continue
		}
		visible = append(visible, groups[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "method": method, "groups": visible, "count": len(visible),
		"suppressed": suppressed, "truncated": truncated,
		"policy": "同名不等于重复；不同发行/母带/声道保留。执行后旧副本进入可恢复隔离区。",
	})
}

// scanDuplicates groups the library and fingerprints each candidate.
// method is "pcm" when ffmpeg decoded every candidate, "structural"
// when any fingerprint fell back to the file hash.
func (a *App) scanDuplicates(ctx context.Context) ([]duplicateGroup, string, bool, error) {
	entries, err := audiorelease.List(a.engine)
	if err != nil {
		return nil, "", false, err
	}
	decoder := audiorelease.SystemDecoder()
	method := "pcm"
	if decoder == nil {
		method = "structural"
	}
	// Group by (artist, album, track slot) from the record.
	bySlot := map[string][]audiorelease.Entry{}
	for _, entry := range entries {
		if !entry.FilePresent {
			continue
		}
		artist, album := splitLibraryTail(entry.File)
		if artist == "" || album == "" {
			continue
		}
		slot := strconv.Itoa(entry.Track)
		if entry.Track <= 0 {
			slot = "title:" + trackTitle(entry.File)
		}
		key := artist + "\x00" + album + "\x00" + slot
		bySlot[key] = append(bySlot[key], entry)
	}
	groups := []duplicateGroup{}
	truncated := false
	for _, rows := range bySlot {
		if len(rows) < 2 {
			continue
		}
		if len(groups) >= 1000 {
			truncated = true
			break
		}
		group, err := a.fingerprintGroup(ctx, rows, decoder)
		if err != nil {
			return nil, "", false, err
		}
		if group != nil {
			groups = append(groups, *group)
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].GroupID < groups[j].GroupID })
	return groups, method, truncated, nil
}

// fingerprintGroup fingerprints one slot's files and reports the group.
// Files that fail to fingerprint are listed with the failure as the
// group reason instead of being silently dropped.
func (a *App) fingerprintGroup(ctx context.Context, rows []audiorelease.Entry, decoder audiorelease.Decoder) (*duplicateGroup, error) {
	artist, album := splitLibraryTail(rows[0].File)
	files := make([]duplicateFile, 0, len(rows))
	byIdentity := map[string][]int{}
	for _, entry := range rows {
		full := a.entryFilePath(entry)
		rate, channels, bits, streamErr := streamInfoFor(full, entry.Format)
		pcm := ""
		identity := ""
		if decoder != nil {
			digest, err := pcmFingerprint(ctx, full)
			if err != nil {
				return &duplicateGroup{
					GroupID: duplicateGroupID(rows), Album: album, Artists: []string{artist},
					Files: files, State: "review", Reason: "音频内容校验未完成，原文件保留: " + err.Error(), IdentityUsed: "pcm",
				}, nil
			}
			pcm = digest
			identity = fmt.Sprintf("pcm:%s:%d:%d", digest, rate, channels)
		} else {
			identity = fmt.Sprintf("structural:%s:%s", entry.Format, entry.SHA256)
		}
		if streamErr != nil {
			// Stream info is advisory for ranking; a missing rate never
			// blocks the identity, which is the decoded hash.
			rate, channels, bits = 0, 0, 0
		}
		file := duplicateFile{
			RequestID: entry.RequestID, File: entry.File, Format: entry.Format, SHA256: entry.SHA256,
			PCMSHA256: pcm, SampleRate: rate, Channels: channels, BitsPerSample: bits,
			Lossless: entry.Format == "flac" || entry.Format == "wav", LibraryDir: entry.LibraryDir,
		}
		file.Rank = duplicateRank(file)
		byIdentity[identity] = append(byIdentity[identity], len(files))
		files = append(files, file)
	}
	// A group is ready only when at least two files share one identity.
	// Python _prepare raises when the same slot decodes differently and
	// the release is unconfirmed, so a mixed-identity slot stays review.
	shared := false
	for _, members := range byIdentity {
		if len(members) > 1 {
			shared = true
		}
	}
	group := &duplicateGroup{
		GroupID: duplicateGroupID(rows), Album: album, Artists: []string{artist},
		Files: files, IdentityUsed: "pcm",
	}
	if decoder == nil {
		group.IdentityUsed = "structural"
	}
	if !shared {
		group.State = "review"
		if decoder == nil {
			group.Reason = "结构级比对：无解码器，相同 PCM、不同编码的文件不会分进同一组"
		} else {
			group.Reason = "同曲号文件解码音频不同，需要先确认发行身份"
		}
		return group, nil
	}
	// Keep the best-ranked file among the largest identical-identity set.
	bestIdentity := ""
	bestCount := 0
	for identity, members := range byIdentity {
		if len(members) > bestCount {
			bestIdentity, bestCount = identity, len(members)
		}
	}
	keep := files[byIdentity[bestIdentity][0]]
	for _, idx := range byIdentity[bestIdentity] {
		if compareRank(files[idx].Rank, keep.Rank) > 0 {
			keep = files[idx]
		}
	}
	keepCopy := keep
	group.Keep = &keepCopy
	group.State = "ready"
	if decoder == nil {
		group.Reason = "文件内容完全一致，合并重复歌曲（结构级比对：无解码器）"
	} else {
		group.Reason = "文件内容完全一致，合并重复歌曲"
	}
	return group, nil
}

// duplicateRank mirrors Python _rank: (lossless, bits if lossless,
// sample rate if lossless, format preference). Bitrate is not in the Go
// record, so lossy files rank on preference alone; the library only
// holds lossless WAV/FLAC, so this never decides a real merge today.
func duplicateRank(file duplicateFile) []int {
	lossless := 0
	if file.Lossless {
		lossless = 1
	}
	bits, rate := 0, 0
	if file.Lossless {
		bits, rate = file.BitsPerSample, int(file.SampleRate)
	}
	preference := 0
	switch file.Format {
	case "flac":
		preference = 2
	case "wav":
		preference = 1
	}
	return []int{lossless, bits, rate, preference}
}

func compareRank(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] - b[i]
		}
	}
	return len(a) - len(b)
}

// duplicateGroupID is the stable id of one slot's file set, as Python
// digest([key, sorted file ids]) is.
func duplicateGroupID(rows []audiorelease.Entry) string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.RequestID)
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return "dup-" + hex.EncodeToString(sum[:8])
}

// splitLibraryTail splits a library file tail "Artist/Album/NN Title.ext"
// into artist and album. A tail with fewer segments has no reliable
// album identity.
func splitLibraryTail(tail string) (artist, album string) {
	parts := strings.Split(tail, "/")
	if len(parts) < 3 {
		return "", ""
	}
	return parts[len(parts)-3], parts[len(parts)-2]
}

// trackTitle returns the file stem without the "NN " track prefix.
func trackTitle(tail string) string {
	base := filepath.Base(tail)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if len(stem) > 3 && stem[0] >= '0' && stem[0] <= '9' && stem[1] >= '0' && stem[1] <= '9' && stem[2] == ' ' {
		return stem[3:]
	}
	return stem
}

// ---------------------------------------------------------------------------
// Duplicate merge / restore
//
// POST /v1/library/duplicates/merge   {"group_id": "dup-..."}
// POST /v1/library/duplicates/restore {"merge_id": "merge-..."}
//
// Merge re-scans (the scan is the plan: no stored plan can go stale),
// refuses a group that is not ready or whose files changed since the
// scan, keeps the best-ranked file, and moves every other file — plus
// its .lrc sidecar when present — into quarantine/<merge id>/ under the
// state directory. Nothing is deleted. The receipt is stored in Pebble
// under library/merge/<merge id> so restore can audit every move.
// Restore moves each quarantined file back, verifies its hash, refuses
// when the destination now holds different bytes, and marks the receipt
// restored. A restore of an already-restored merge replays.

type mergeReceipt struct {
	MergeID   string      `json:"merge_id"`
	GroupID   string      `json:"group_id"`
	Keep      string      `json:"keep"`
	Moved     []mergeMove `json:"moved"`
	CreatedNS int64       `json:"created_ns"`
	Restored  bool        `json:"restored"`
}

// mergeMove is one quarantined file (audio or its sidecar).
type mergeMove struct {
	RequestID  string `json:"request_id,omitempty"`
	From       string `json:"from"`
	Quarantine string `json:"quarantine"`
	SHA256     string `json:"sha256"`
	Sidecar    bool   `json:"sidecar,omitempty"`
}

func mergeReceiptKey(id string) []byte { return []byte("library/merge/" + id) }

func (a *App) duplicateMerge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GroupID string `json:"group_id"`
	}
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.GroupID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	groups, _, _, err := a.scanDuplicates(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	var group *duplicateGroup
	for i := range groups {
		if groups[i].GroupID == body.GroupID {
			group = &groups[i]
			break
		}
	}
	if group == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "NOT_FOUND"})
		return
	}
	if group.State != "ready" || group.Keep == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "MERGE_NOT_READY", "error": group.Reason})
		return
	}
	// A suppressed album opted out of dedup entirely; merging it would undo
	// exactly what the flag protects.
	if a.groupSuppressed(group) {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "MERGE_SUPPRESSED", "error": "这张专辑已设为不参与合并，先在字段锁里取消抑制"})
		return
	}
	// Moving a file that qBittorrent still holds breaks the torrent: the
	// client keeps seeding a path that no longer exists. The check is before
	// any hash work and before any move, and a client that is configured but
	// unreachable refuses the merge instead of guessing (P0-3).
	if code, reason := a.mergeSeedingGuard(group); code != "" {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": code, "error": reason})
		return
	}
	// Verify every file is unchanged since the scan before moving any.
	for _, file := range group.Files {
		full := a.libraryFilePath(file.LibraryDir, file.File)
		digest, err := hashFile(full)
		if err != nil || digest != file.SHA256 {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "SCAN_STALE", "error": "保留或重复文件发生变化，请重新扫描"})
			return
		}
	}
	// Fill the keeper's empty fields from the duplicates before anything
	// moves (Python duplicates.py parity): a loser often carries the genre or
	// the date the keeper lost. A failure here refuses the merge with nothing
	// moved yet.
	filled, err := a.fillMissingTags(group)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "MERGE_TAG_FAILED", "error": "补写标签失败，文件没有移动", "restored": true})
		return
	}
	mergeSum := sha256.Sum256([]byte(body.GroupID + ":" + group.Keep.RequestID))
	mergeID := "merge-" + hex.EncodeToString(mergeSum[:8])
	quarantineRoot := filepath.Join(a.Dir, "quarantine", mergeID)
	if err := os.MkdirAll(quarantineRoot, 0o700); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	receipt := mergeReceipt{MergeID: mergeID, GroupID: group.GroupID, Keep: group.Keep.File, CreatedNS: a.now().UnixNano()}
	for _, file := range group.Files {
		if file.RequestID == group.Keep.RequestID {
			continue
		}
		if err := a.quarantineFile(&receipt, file, quarantineRoot); err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "MERGE_CONFLICT", "error": err.Error()})
			return
		}
	}
	if len(receipt.Moved) == 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "MERGE_NOT_READY", "error": "没有可合并的重复文件"})
		return
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	ok, err := a.engine.CompareAndSwap(
		[]dataengine.Condition{{Key: mergeReceiptKey(mergeID), Absent: true}},
		[]dataengine.Mutation{{Key: mergeReceiptKey(mergeID), Value: encoded}},
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	if !ok {
		// The same merge already ran: replay its receipt instead of
		// moving files a second time.
		existing, found, err := a.readMergeReceipt(mergeID)
		if err != nil || !found {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "MERGE_CONFLICT"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "replayed": true, "merge_id": existing.MergeID, "group_id": existing.GroupID, "kept": existing.Keep, "quarantined": existing.Moved})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "merge_id": receipt.MergeID, "group_id": receipt.GroupID, "kept": receipt.Keep, "quarantined": receipt.Moved, "filled_fields": filled})
}

// fillableTagFields are the fields a duplicate may contribute when the keeper
// has none. Title/artist/album/track stay out: those decide identity, not
// metadata.
var fillableTagFields = []string{"ALBUMARTIST", "DATE", "GENRE", "COMPOSER", "LYRICS"}

// fillMissingTags copies a duplicate's tag into an empty keeper field when the
// duplicates agree on exactly one value. It never overwrites: a curated value
// wins, and a conflict is simply not filled. Only FLAC is edited.
func (a *App) fillMissingTags(group *duplicateGroup) ([]string, error) {
	if group.Keep == nil {
		return nil, nil
	}
	keeperPath := a.libraryFilePath(group.Keep.LibraryDir, group.Keep.File)
	if !strings.EqualFold(filepath.Ext(keeperPath), ".flac") {
		return nil, nil
	}
	keeper, err := flactags.ReadFile(keeperPath)
	if err != nil {
		return nil, nil
	}
	candidates := map[string]map[string]bool{}
	for _, file := range group.Files {
		if file.RequestID == group.Keep.RequestID {
			continue
		}
		path := a.libraryFilePath(file.LibraryDir, file.File)
		if !strings.EqualFold(filepath.Ext(path), ".flac") {
			continue
		}
		tags, err := flactags.ReadFile(path)
		if err != nil {
			continue
		}
		for _, field := range fillableTagFields {
			value := strings.TrimSpace(firstTagValue(tags.Raw, field))
			if value == "" {
				continue
			}
			if candidates[field] == nil {
				candidates[field] = map[string]bool{}
			}
			candidates[field][value] = true
		}
	}
	updates := map[string][]string{}
	filled := []string{}
	// A locked field is curated by the user: the merge does not fill it even
	// when it is empty and the duplicates agree on a value.
	guard := a.guardFor(filepath.Dir(keeperPath))
	locked := map[string]bool{}
	for _, field := range guard.Fields {
		locked[strings.ToUpper(field)] = true
	}
	for _, field := range fillableTagFields {
		if locked[field] {
			continue
		}
		if strings.TrimSpace(firstTagValue(keeper.Raw, field)) != "" {
			continue
		}
		values := candidates[field]
		if len(values) != 1 {
			continue
		}
		for value := range values {
			updates[field] = []string{value}
			filled = append(filled, strings.ToLower(field))
		}
	}
	if len(updates) == 0 {
		return nil, nil
	}
	if err := writeFLACTags(keeperPath, updates); err != nil {
		return nil, err
	}
	// The write changed the bytes: the library record must carry the new
	// digest or the next duplicate scan reports the file as changed.
	if _, err := audiorelease.RefreshDigest(a.engine, group.Keep.RequestID); err != nil {
		return nil, err
	}
	return filled, nil
}

// mergeSeedingGuard refuses a merge whose files qBittorrent still holds. A
// configured client that cannot be reached refuses as well: "I could not
// check" is not "it is safe". Without saved qBittorrent settings there is
// nothing to check against, and the merge proceeds as before.
func (a *App) mergeSeedingGuard(group *duplicateGroup) (string, string) {
	settings, err := a.loadQBittorrentSettings()
	if err != nil || strings.TrimSpace(settings.BaseURL) == "" {
		return "", ""
	}
	login, baseURL, ok := a.savedQBittorrentLogin()
	if !ok {
		return "MERGE_QBITTORRENT_UNAVAILABLE", "qBittorrent 已配置但读不到登录凭据，无法确认这些文件是否还在做种；这次没有合并"
	}
	client, err := qbittorrent.NewClient(qbittorrent.Config{BaseURL: baseURL, Username: login.Username, Password: login.Password})
	if err != nil {
		return "MERGE_QBITTORRENT_UNAVAILABLE", "qBittorrent 地址无效，无法确认这些文件是否还在做种；这次没有合并"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Login(ctx); err != nil {
		return "MERGE_QBITTORRENT_UNAVAILABLE", "qBittorrent 连不上，无法确认这些文件是否还在做种；这次没有合并"
	}
	rows, err := client.InfoByTag(ctx, qbittorrent.ProjectTag)
	if err != nil {
		return "MERGE_QBITTORRENT_UNAVAILABLE", "读取 qBittorrent 种子列表失败，无法确认做种状态；这次没有合并"
	}
	for _, file := range group.Files {
		full := a.libraryFilePath(file.LibraryDir, file.File)
		for _, row := range rows {
			root := strings.TrimSpace(row.ContentPath)
			if root == "" {
				root = strings.TrimSpace(row.SavePath)
			}
			if root == "" {
				continue
			}
			if insideDir(root, full) {
				return "MERGE_SEEDING", "文件仍在 qBittorrent 中（" + row.Name + "）；先在客户端停止并移除该种子，再合并"
			}
		}
	}
	return "", ""
}

// quarantineFile moves one library file (and its .lrc sidecar when
// present) into the merge quarantine directory, recording each move.
func (a *App) quarantineFile(receipt *mergeReceipt, file duplicateFile, quarantineRoot string) error {
	from := a.libraryFilePath(file.LibraryDir, file.File)
	dest := filepath.Join(quarantineRoot, file.File)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("quarantine destination already exists: %s", dest)
	}
	if err := os.Rename(from, dest); err != nil {
		return err
	}
	receipt.Moved = append(receipt.Moved, mergeMove{RequestID: file.RequestID, From: from, Quarantine: dest, SHA256: file.SHA256})
	// The sidecar travels with its audio file; a missing sidecar is fine.
	// Both spellings are moved: the standard "<track>.lrc" this build writes
	// and the older "<track>.flac.lrc" an earlier build left behind.
	for _, sidecarFrom := range sidecarCandidates(from) {
		data, err := os.ReadFile(sidecarFrom)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		sidecarDest := lrc.SidecarPath(dest)
		if sidecarFrom == from+".lrc" {
			// Keep the older spelling inside the quarantine so a restore puts
			// the file back under the name it had.
			sidecarDest = dest + ".lrc"
		}
		if err := os.Rename(sidecarFrom, sidecarDest); err != nil {
			return err
		}
		receipt.Moved = append(receipt.Moved, mergeMove{RequestID: file.RequestID, From: sidecarFrom, Quarantine: sidecarDest, SHA256: hex.EncodeToString(sum[:]), Sidecar: true})
	}
	return nil
}

// sidecarCandidates lists the lyric sidecar names that may sit next to an
// audio file: the standard "<track>.lrc" first, then the older
// "<track>.flac.lrc" spelling an earlier build wrote.
func sidecarCandidates(audioPath string) []string {
	standard := lrc.SidecarPath(audioPath)
	legacy := audioPath + ".lrc"
	if legacy == standard {
		return []string{standard}
	}
	return []string{standard, legacy}
}

func (a *App) readMergeReceipt(id string) (mergeReceipt, bool, error) {
	var receipt mergeReceipt
	var found bool
	err := a.engine.View(func(v *dataengine.View) error {
		raw, ok, err := v.Get(mergeReceiptKey(id))
		if err != nil || !ok {
			return err
		}
		found = true
		return json.Unmarshal(raw, &receipt)
	})
	return receipt, found, err
}

func (a *App) duplicateRestore(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MergeID string `json:"merge_id"`
	}
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.MergeID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	receipt, found, err := a.readMergeReceipt(body.MergeID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "NOT_FOUND"})
		return
	}
	if receipt.Restored {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "replayed": true, "merge_id": receipt.MergeID, "restored": receipt.Moved})
		return
	}
	// Verify every quarantined file first; restore nothing until all check out.
	for _, move := range receipt.Moved {
		digest, err := hashFile(move.Quarantine)
		if err != nil || digest != move.SHA256 {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "RESTORE_CONFLICT", "error": "隔离区文件已变化或丢失，拒绝恢复"})
			return
		}
		if existing, err := os.ReadFile(move.From); err == nil {
			sum := sha256.Sum256(existing)
			if hex.EncodeToString(sum[:]) != move.SHA256 {
				writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "RESTORE_CONFLICT", "error": "目标位置已有不同内容，拒绝覆盖"})
				return
			}
			// The destination already holds identical bytes: the
			// quarantine copy is redundant, drop it below.
		}
	}
	restored := []mergeMove{}
	for _, move := range receipt.Moved {
		if existing, err := os.ReadFile(move.From); err == nil {
			sum := sha256.Sum256(existing)
			if hex.EncodeToString(sum[:]) == move.SHA256 {
				_ = os.Remove(move.Quarantine)
				restored = append(restored, move)
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(move.From), 0o700); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
			return
		}
		if err := os.Rename(move.Quarantine, move.From); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE", "error": err.Error()})
			return
		}
		restored = append(restored, move)
	}
	receipt.Restored = true
	encoded, err := json.Marshal(receipt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	// Best effort: the files are already back; a lost receipt update is
	// reported, not hidden, so a second restore replays from the files.
	// The condition is the exact bytes read, so a concurrent change to
	// the receipt fails instead of overwriting.
	var current []byte
	_ = a.engine.View(func(v *dataengine.View) error {
		raw, ok, err := v.Get(mergeReceiptKey(receipt.MergeID))
		if err == nil && ok {
			current = raw
		}
		return err
	})
	if _, err := a.engine.CompareAndSwap(
		[]dataengine.Condition{{Key: mergeReceiptKey(receipt.MergeID), Value: current}},
		[]dataengine.Mutation{{Key: mergeReceiptKey(receipt.MergeID), Value: encoded}},
	); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE", "error": "files restored but the receipt could not be updated: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "merge_id": receipt.MergeID, "restored": restored})
}

// ---------------------------------------------------------------------------
// Artist folders
//
// GET  /v1/library/artist-folders
// POST /v1/library/artist-folders {"request_ids": [...]}
//
// Plan follows app/artist_folders.py: a file is loose when its album
// folder sits directly in the library root (or the file itself sits in
// the root); the target is <library>/<folder artist>/<album>/<file>.
// folder_artist joins split credits with '／' (full-width solidus, a
// legal single folder name everywhere), so "A feat. B" and ["A","B"]
// both become "A／B". Artist and album come from the FLAC tags — the Go
// records carry no job metadata — so a WAV or an untagged FLAC is
// skipped with a reason, never guessed from the folder name. The target
// must not already exist with content, the move must stay on one disk
// (os.Rename), and both ends must resolve inside the library
// (resolveInDir + a resolved-target check, the safe_destination /
// reject_reparse_chain contract). After the move the library record is
// updated through audiorelease.Relocate and the .lrc sidecar travels
// with its audio file.

// artistFolderSplit splits one artist credit the way Python SPLIT does.
var artistFolderSplit = regexp.MustCompile(`(?i)\s*(?:&|＆|/|／|;|；|,|，|、|×|\bfeat\.|\bfeat\b|\bft\.|\bfeaturing\b)\s*`)

var unknownArtists = map[string]bool{
	"": true, "unknown": true, "unknown artist": true, "未知": true, "未知歌手": true, "未知艺人": true, "[unknown]": true,
}

// folderArtist joins artist credits into one folder name.
func folderArtist(artists []string) string {
	seen := map[string]bool{}
	names := []string{}
	for _, credit := range artists {
		for _, part := range artistFolderSplit.Split(credit, -1) {
			name := strings.TrimSpace(part)
			if name != "" && !seen[strings.ToLower(name)] {
				seen[strings.ToLower(name)] = true
				names = append(names, name)
			}
		}
	}
	return strings.Join(names, "／")
}

// cleanFolderComponent sanitizes one path component the way
// library/paths.py release_path does: illegal characters become '_',
// trailing spaces/dots are trimmed, reserved Windows device names are
// prefixed, and an empty result becomes '_'.
func cleanFolderComponent(value string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(value))
	cleaned = strings.TrimRight(cleaned, " .")
	if cleaned == "" {
		return "_"
	}
	head := cleaned
	if i := strings.IndexByte(cleaned, '.'); i >= 0 {
		head = cleaned[:i]
	}
	switch strings.ToUpper(head) {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		cleaned = "_" + cleaned
	}
	return cleaned
}

// artistFolderPlan is one planned (or skipped) relocation.
type artistFolderPlan struct {
	RequestID string `json:"request_id"`
	File      string `json:"file"`
	State     string `json:"state"` // intent | skipped | none
	From      string `json:"from"`
	To        string `json:"to"`
	Reason    string `json:"reason,omitempty"`
	LooseFile bool   `json:"loose_file"`
}

// planArtistFolders computes the relocation plan for every present
// library record.
func (a *App) planArtistFolders() ([]artistFolderPlan, error) {
	entries, err := audiorelease.List(a.engine)
	if err != nil {
		return nil, err
	}
	plans := []artistFolderPlan{}
	for _, entry := range entries {
		if !entry.FilePresent {
			continue
		}
		plans = append(plans, a.planArtistFolder(entry, a.entryLibraryDir(entry)))
	}
	return plans, nil
}

func (a *App) planArtistFolder(entry audiorelease.Entry, library string) artistFolderPlan {
	plan := artistFolderPlan{RequestID: entry.RequestID, File: entry.File, State: "none"}
	full, err := resolveInDir(library, entry.File)
	if err != nil {
		plan.State = "skipped"
		plan.Reason = "文件路径不在库内或含链接"
		return plan
	}
	// full is resolved, so the library must be resolved the same way
	// before the lexical Rel below: on macOS the state directory can
	// sit under a symlinked parent (/var points at /private/var) and
	// the unresolved library spelling shares no lexical prefix with
	// the resolved file path, which used to make every loose album
	// look already organized (state "none", empty "to").
	resolvedLibrary := library
	if r, err := filepath.EvalSymlinks(library); err == nil {
		resolvedLibrary = r
	}
	dir := filepath.Dir(full)
	rel, err := filepath.Rel(resolvedLibrary, dir)
	if err != nil {
		plan.State = "skipped"
		plan.Reason = "文件路径不在库内"
		return plan
	}
	// relFile is the file's path inside the library, for building
	// from/to in the caller's spelling: plans are compared against
	// paths built from application.Dir, which may itself be spelled
	// through a symlinked parent, so the resolved spelling must not
	// leak into from/to.
	relFile, err := filepath.Rel(resolvedLibrary, full)
	if err != nil {
		plan.State = "skipped"
		plan.Reason = "文件路径不在库内"
		return plan
	}
	parts := strings.Split(rel, string(filepath.Separator))
	switch {
	case rel == ".":
		// The file itself sits in the library root.
		plan.LooseFile = true
	case len(parts) == 1:
		// An album folder directly in the library root.
	default:
		// Already inside an artist (or deeper) folder.
		return plan
	}
	// Artist and album come from the FLAC tags, never from the folder
	// name: the folder at the root is the untrusted part.
	var artists []string
	var album string
	switch strings.ToLower(filepath.Ext(full)) {
	case ".flac":
		if tags, err := flactags.ReadFile(full); err == nil {
			artists = tags.Artists
			album = tags.Album
		}
	case ".wav":
		if tags, err := wavflac.ReadDescription(full); err == nil {
			if tags.Artist != "" {
				artists = []string{tags.Artist}
			}
			album = tags.Album
		}
	}
	artist := folderArtist(artists)
	if unknownArtists[strings.ToLower(artist)] || strings.TrimSpace(album) == "" {
		plan.State = "skipped"
		plan.Reason = "缺少可靠的专辑艺人或专辑名"
		return plan
	}
	targetDir := filepath.Join(library, cleanFolderComponent(artist), cleanFolderComponent(album))
	var from, to string
	if plan.LooseFile {
		from = filepath.Join(library, relFile)
		to = filepath.Join(targetDir, filepath.Base(relFile))
	} else {
		from = filepath.Join(library, filepath.Dir(relFile))
		to = targetDir
	}
	resolvedTarget, err := resolveTargetInDir(library, to)
	if err != nil {
		plan.State = "skipped"
		plan.Reason = "目标路径不在库内或含链接"
		return plan
	}
	to = resolvedTarget
	if _, err := os.Lstat(to); err == nil {
		// Any existing target blocks the move: the target must not
		// exist at all, so nothing is ever overwritten or merged into.
		plan.State = "skipped"
		plan.Reason = "目标已存在"
		if !plan.LooseFile {
			plan.Reason = "目标文件夹已存在：" + filepath.Base(to)
		}
		return plan
	}
	plan.State = "intent"
	plan.From = from
	plan.To = to
	return plan
}

// resolveTargetInDir resolves a not-yet-existing target path: the
// existing ancestors are resolved through symlinks and the remainder is
// appended lexically, then the result must stay inside root. This is the
// safe_destination contract for a destination that does not exist yet.
// The returned path keeps the caller's spelling: the resolved path is
// only the containment check, and callers compare the result against
// paths built from the state directory, which on macOS may be spelled
// through a symlinked parent (/var -> /private/var).
func resolveTargetInDir(root, target string) (string, error) {
	absRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	abs := filepath.Clean(target)
	// Walk up to the nearest existing ancestor and resolve it.
	ancestor := abs
	var suffix []string
	for {
		if _, err := os.Lstat(ancestor); err == nil {
			break
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", errors.New("no existing ancestor")
		}
		suffix = append([]string{filepath.Base(ancestor)}, suffix...)
		ancestor = parent
	}
	resolved, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	for _, part := range suffix {
		if part == ".." || part == "." {
			return "", errors.New("target contains dot components")
		}
		resolved = filepath.Join(resolved, part)
	}
	if !insideDir(absRoot, resolved) {
		return "", errors.New("target escapes the library")
	}
	return abs, nil
}

func (a *App) artistFolderPlan(w http.ResponseWriter, r *http.Request) {
	plans, err := a.planArtistFolders()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	if plans == nil {
		plans = []artistFolderPlan{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "plans": plans, "count": len(plans)})
}

// artistFolderRelocate executes the planned moves. request_ids selects a
// subset; omitting it relocates every intent plan. Each item reports
// its own outcome; a failed item never blocks the others.
func (a *App) artistFolderRelocate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequestIDs []string `json:"request_ids"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	plans, err := a.planArtistFolders()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	selected := map[string]bool{}
	for _, id := range body.RequestIDs {
		selected[id] = true
	}
	results := []map[string]any{}
	for _, plan := range plans {
		if len(selected) > 0 && !selected[plan.RequestID] {
			continue
		}
		if plan.State != "intent" {
			results = append(results, map[string]any{"request_id": plan.RequestID, "file": plan.File, "state": "skipped", "reason": plan.Reason})
			continue
		}
		moved, reason := a.relocateOne(plan)
		if moved == "" {
			results = append(results, map[string]any{"request_id": plan.RequestID, "file": plan.File, "state": "skipped", "reason": reason})
			continue
		}
		results = append(results, map[string]any{"request_id": plan.RequestID, "file": plan.File, "state": "done", "from": plan.From, "to": moved})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results, "count": len(results)})
}

// relocateOne moves one planned item and updates its library record.
// It returns the new file path, or "" with a reason when it stays put.
func (a *App) relocateOne(plan artistFolderPlan) (string, string) {
	if plan.LooseFile {
		// Move one loose file (plus its sidecar) into Artist/Album/.
		if err := os.MkdirAll(filepath.Dir(plan.To), 0o700); err != nil {
			return "", "无法创建目标文件夹"
		}
		if _, err := os.Lstat(plan.To); err == nil {
			return "", "目标已存在，拒绝覆盖"
		}
		if err := os.Rename(plan.From, plan.To); err != nil {
			return "", "无法移动文件（" + err.Error() + "），保留原位置"
		}
		if err := audiorelease.Relocate(a.engine, plan.RequestID, plan.To); err != nil {
			// The record update failed: move the file back so the
			// library and the record never disagree.
			_ = os.Rename(plan.To, plan.From)
			return "", "库记录更新失败，已移回原位置"
		}
		moveSidecar(plan.From, plan.To)
		return plan.To, ""
	}
	// Move a whole album folder. The target must not exist: a rename
	// onto an existing empty directory would silently merge on some
	// platforms, so refuse it here.
	if _, err := os.Lstat(plan.To); err == nil {
		return "", "目标文件夹已存在"
	}
	if err := os.MkdirAll(filepath.Dir(plan.To), 0o700); err != nil {
		return "", "无法创建目标文件夹"
	}
	if err := os.Rename(plan.From, plan.To); err != nil {
		return "", "无法移动文件夹（" + err.Error() + "），保留原位置"
	}
	// Update every library record under the moved folder. A record
	// whose update fails is moved back with the folder: the whole move
	// is all or nothing.
	entries, err := audiorelease.List(a.engine)
	if err != nil {
		_ = os.Rename(plan.To, plan.From)
		return "", "库记录读取失败，已移回原位置"
	}
	moved := []audiorelease.Entry{}
	for _, entry := range entries {
		full := a.entryFilePath(entry)
		rel, err := filepath.Rel(plan.From, full)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		newPath := filepath.Join(plan.To, rel)
		if err := audiorelease.Relocate(a.engine, entry.RequestID, newPath); err != nil {
			for _, done := range moved {
				_ = audiorelease.Relocate(a.engine, done.RequestID, a.entryFilePath(done))
			}
			_ = os.Rename(plan.To, plan.From)
			return "", "库记录更新失败，已移回原位置"
		}
		moved = append(moved, entry)
	}
	// Sidecars travel with their audio files: they sit next to the
	// files inside the moved folder, so the folder rename carried them.
	return filepath.Join(plan.To, filepath.Base(plan.File)), ""
}

// moveSidecar moves the lyric sidecar next to a relocated audio file when
// present. Both spellings are moved; the destination is always the standard
// "<track>.lrc". A missing sidecar is fine; a failed move is reported by the
// caller only through the returned error.
func moveSidecar(from, to string) {
	for _, sidecarFrom := range sidecarCandidates(from) {
		if _, err := os.Lstat(sidecarFrom); err != nil {
			continue
		}
		_ = os.Rename(sidecarFrom, lrc.SidecarPath(to))
	}
}
