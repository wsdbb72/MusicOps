package app

// Playlist enqueue: turn a fetched playlist into work the pipeline can run.
//
// The pipeline is album oriented: a job confirms a catalogue identity, then
// searches the trackers, then downloads the release and publishes it. A
// playlist row therefore becomes an *album* job for the album that row belongs
// to, which is also what the user asks for: if the library already holds part
// of that album, running the job fills in the rest.
//
// Two things are decided before a job starts:
//   - dedupe: a row whose track is already in the library starts nothing;
//   - album grouping: 12 rows from one album become one job, not twelve.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/musicbrainz"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

const hotSlowCompilationAlbum = "最新热歌慢摇"
const hotSlowCompilationArtist = "群星"

// normalizePlaylistAlbumTitle folds the numbered "最新热歌慢摇" source volumes
// back to the single compilation title users see in their library.
func normalizePlaylistAlbumTitle(album string) string {
	value := strings.TrimSpace(album)
	if !strings.HasPrefix(value, hotSlowCompilationAlbum) {
		return value
	}
	suffix := strings.TrimSpace(strings.TrimPrefix(value, hotSlowCompilationAlbum))
	if suffix == "" {
		return hotSlowCompilationAlbum
	}
	for _, r := range suffix {
		if (r < '0' || r > '9') && (r < '０' || r > '９') {
			return value
		}
	}
	return hotSlowCompilationAlbum
}

func normalizePlaylistAlbumPlan(plan *SearchPlan) {
	if plan == nil {
		return
	}
	original := plan.ExpectedTitle
	plan.ExpectedTitle = normalizePlaylistAlbumTitle(original)
	if plan.ExpectedTitle == hotSlowCompilationAlbum {
		plan.AlbumArtist = hotSlowCompilationArtist
	}
}

func isHotSlowCompilationAlbum(album string) bool {
	return normalizePlaylistAlbumTitle(album) == hotSlowCompilationAlbum
}

type playlistEnqueueOptions struct {
	// AlbumCompletion groups rows by album and starts one album job per
	// album, so a partially present album is completed rather than patched
	// track by track.
	AlbumCompletion bool
	NeteaseOnly     bool
	// CatalogBaseURL is a test seam (loopback only); empty uses MusicBrainz.
	CatalogBaseURL string
}

type playlistEnqueueReport struct {
	Created   []string `json:"created"`
	Skipped   []string `json:"skipped"`
	Unmatched []string `json:"unmatched"`
	Albums    int      `json:"albums"`
	Tracks    int      `json:"tracks"`
}

// Index only candidate names. Verify bytes lazily for playlist matches.
type playlistLibraryIndex struct {
	engine           *dataengine.Engine
	tracks           map[[3]string][]string
	albums           map[[2]string][]string
	verified         map[string]bool
	tagTitles        map[[2]string]map[string]bool
	records          map[string]audiorelease.Entry
	pendingSnapshots map[string]audiorelease.VerifiedPresence
}

func normalizeKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	replacer := strings.NewReplacer("_", " ", "-", " ", "/", " ", "　", " ", "’", "'", "‘", "'", "“", "\"", "”", "\"")
	s = replacer.Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// trackKeyOfPath strips the track number and extension from a library file.
func trackKeyOfPath(file string) (artist, album, title string) {
	base := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	base = strings.TrimSpace(base)
	// Drop a leading track number ("01 ", "01 - ", "1.").
	if fields := strings.Fields(base); len(fields) > 1 {
		head := strings.TrimRight(fields[0], ".-_")
		if _, err := strconv.Atoi(head); err == nil {
			base = strings.Join(fields[1:], " ")
		}
	}
	base = strings.TrimLeft(base, "-_. ")
	dir := filepath.Dir(file)
	album = filepath.Base(dir)
	artist = filepath.Base(filepath.Dir(dir))
	return normalizeKey(artist), normalizeKey(album), normalizeKey(base)
}

const playlistLibraryCacheTTL = 2 * time.Minute

func (a *App) playlistLibrary() playlistLibraryIndex {
	// One listing is reused by the preview and its immediate enqueue; the
	// per-track file state remains verified independently.
	a.playlistEntriesMu.Lock()
	defer a.playlistEntriesMu.Unlock()
	entries := a.playlistEntriesCache
	if !a.playlistEntriesCached || a.now().Sub(a.playlistEntriesAt) >= playlistLibraryCacheTTL {
		fresh, err := audiorelease.ListIndexed(a.engine)
		if err == nil {
			entries = fresh
			a.playlistEntriesCache = fresh
			a.playlistEntriesCached = true
			a.playlistEntriesAt = a.now()
		}
	}
	return indexPlaylistLibrary(a.engine, entries)
}

func indexPlaylistLibrary(engine *dataengine.Engine, entries []audiorelease.Entry) playlistLibraryIndex {
	index := playlistLibraryIndex{engine: engine, tracks: map[[3]string][]string{}, albums: map[[2]string][]string{}, verified: map[string]bool{}, tagTitles: map[[2]string]map[string]bool{}, records: map[string]audiorelease.Entry{}, pendingSnapshots: map[string]audiorelease.VerifiedPresence{}}
	for _, entry := range entries {
		if !entry.FilePresent {
			continue
		}
		index.records[entry.RequestID] = entry
		artist, album, title := trackKeyOfPath(entry.File)
		if artist != "" && artist != "." && album != "" && album != "." && title != "" {
			key := [3]string{artist, album, title}
			index.tracks[key] = append(index.tracks[key], entry.RequestID)
			albumKey := [2]string{artist, album}
			index.albums[albumKey] = append(index.albums[albumKey], entry.RequestID)
		}
	}
	return index
}

func (index playlistLibraryIndex) hasTrack(artist, album, title string) bool {
	key := [2]string{normalizeKey(artist), normalizeKey(album)}
	want := normalizeKey(title)
	if key[0] == "" || key[1] == "" || want == "" {
		return false
	}
	for _, id := range index.tracks[[3]string{key[0], key[1], want}] {
		if index.isUnchanged(id) {
			return true
		}
	}
	titles, checked := index.tagTitles[key]
	if !checked {
		titles = map[string]bool{}
		for _, id := range index.albums[key] {
			if !index.isUnchanged(id) {
				continue
			}
			path, found, err := audiorelease.PathFor(index.engine, id)
			if err != nil || !found || !strings.EqualFold(filepath.Ext(path), ".flac") {
				continue
			}
			tags, err := readFLACTagsPrefix(path)
			if err == nil && normalizeKey(tags.Album) == key[1] && normalizeKey(tags.Title) != "" {
				titles[normalizeKey(tags.Title)] = true
			}
		}
		index.tagTitles[key] = titles
	}
	return titles[want]
}

func (index playlistLibraryIndex) isUnchanged(id string) bool {
	ok, checked := index.verified[id]
	if !checked {
		if entry, found := index.records[id]; found {
			ok = entry.PresenceUnchanged()
			if ok && !entry.HasIndexedStat() {
				index.pendingSnapshots[id] = audiorelease.VerifiedPresence{RequestID: id, RootID: entry.RootID, Track: entry.Track}
			}
		} else {
			ok = audiorelease.PresenceUnchanged(index.engine, id)
		}
		index.verified[id] = ok
	}
	return ok
}

func (index *playlistLibraryIndex) commitPresenceSnapshots() {
	if index == nil || len(index.pendingSnapshots) == 0 {
		return
	}
	items := make([]audiorelease.VerifiedPresence, 0, len(index.pendingSnapshots))
	for _, item := range index.pendingSnapshots {
		items = append(items, item)
	}
	_ = audiorelease.StampVerifiedPresences(index.engine, items)
	index.pendingSnapshots = map[string]audiorelease.VerifiedPresence{}
}

// Only an explicit total in a local file is evidence that a folder has gaps.
func (index playlistLibraryIndex) hasAlbumGap(artist, album string) bool {
	ids := index.albums[[2]string{normalizeKey(artist), normalizeKey(album)}]
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		path, found, err := audiorelease.PathFor(index.engine, id)
		if err != nil || !found || !strings.EqualFold(filepath.Ext(path), ".flac") {
			continue
		}
		tags, err := readFLACTagsPrefix(path)
		if err != nil {
			continue
		}
		total := firstValue(tags.Raw["TRACKTOTAL"])
		if total == "" {
			total = firstValue(tags.Raw["TOTALTRACKS"])
		}
		if total == "" {
			_, total, _ = strings.Cut(firstValue(tags.Raw["TRACKNUMBER"]), "/")
		}
		count, err := strconv.Atoi(total)
		if err == nil && count > len(ids) {
			return true
		}
	}
	return false
}

// enqueuePlaylistTracks starts the work for the selected rows.
func (a *App) enqueuePlaylistTracks(ctx context.Context, playlistJobID string, rows []playlistTrack, indices []int, opts playlistEnqueueOptions) (playlistEnqueueReport, error) {
	report := playlistEnqueueReport{}
	library := a.playlistLibrary()
	sites := []string{}
	if settings, err := a.loadSiteSettings(); err == nil {
		sites = settings.EnabledSites
	}
	budget := 0
	if settings, err := a.loadDownloadSettings(); err == nil {
		budget = settings.TokenBudget
	}
	// Playlist rows lack the measured duration required for catalogue confirmation.
	// Do not issue one serial external lookup per album while building the queue.
	var client *musicbrainz.Client
	if opts.CatalogBaseURL != "" && loopbackURL(opts.CatalogBaseURL) {
		client = musicbrainz.NewClient()
		client.BaseURL = opts.CatalogBaseURL
	}
	seenAlbums := map[string]bool{}
	for _, index := range indices {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if index < 0 || index >= len(rows) {
			return report, fmt.Errorf("曲目序号超出范围：%d", index)
		}
		row := rows[index]
		title := strings.TrimSpace(row.Title)
		artist := strings.TrimSpace(row.Artist)
		if title == "" {
			continue
		}
		report.Tracks++
		album := normalizePlaylistAlbumTitle(row.Album)
		if library.hasTrack(artist, album, title) && (!opts.AlbumCompletion || !library.hasAlbumGap(artist, album)) {
			report.Skipped = append(report.Skipped, artist+" - "+title)
			continue
		}
		if opts.AlbumCompletion && album != "" {
			key := playlistAlbumGroupKey(row, album)
			if seenAlbums[key] {
				continue
			}
			jobID, err := a.startPlaylistAlbumJob(ctx, client, playlistJobID, sites, budget, row, opts.AlbumCompletion, opts.NeteaseOnly)
			if err != nil {
				report.Unmatched = append(report.Unmatched, artist+" - "+album+": "+err.Error())
				continue
			}
			report.Created = append(report.Created, jobID)
			seenAlbums[key] = true
			report.Albums++
			continue
		}
		jobID, err := a.startPlaylistAlbumJob(ctx, client, playlistJobID, sites, budget, row, opts.AlbumCompletion, opts.NeteaseOnly)
		if err != nil {
			report.Unmatched = append(report.Unmatched, artist+" - "+title+": "+err.Error())
			continue
		}
		report.Created = append(report.Created, jobID)
	}
	library.commitPresenceSnapshots()
	return report, nil
}

func playlistAlbumGroupKey(row playlistTrack, album string) string {
	if row.NeteaseAlbumID > 0 {
		return "netease-album:" + strconv.FormatInt(row.NeteaseAlbumID, 10)
	}
	if isHotSlowCompilationAlbum(album) {
		return "compilation:" + normalizeKey(album)
	}
	return "artist-album:" + normalizeKey(row.Artist) + "\x00" + normalizeKey(album)
}

func playlistJobReusable(stage string) bool {
	switch stage {
	case "identifying", "searching", "requesting", "downloading", "fallback", "validating", "publishing":
		return true
	default:
		return false
	}
}

func (a *App) replacementPlaylistJobID(base string, revision uint64) (string, error) {
	for attempt := int64(0); attempt < 64; attempt++ {
		seed := fmt.Sprintf("%s/%d/%d/%d", base, revision, a.now().UnixNano(), attempt)
		sum := sha256.Sum256([]byte(seed))
		candidate := base + "-r-" + hex.EncodeToString(sum[:6])
		existing, err := a.flows.Job(candidate)
		if err != nil {
			return "", err
		}
		if existing == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not allocate a replacement playlist job id")
}

// startPlaylistAlbumJob creates one expand job. The album is the unit the
// pipeline works in, so the job is named after it and the row is the evidence.
func (a *App) startPlaylistAlbumJob(ctx context.Context, client *musicbrainz.Client, playlistJobID string, sites []string, budget int, row playlistTrack, albumCompletion, neteaseOnly bool) (string, error) {
	artist := strings.TrimSpace(row.Artist)
	album := normalizePlaylistAlbumTitle(row.Album)
	title := strings.TrimSpace(row.Title)
	expected := title
	if album != "" {
		expected = album
	}
	slug := strings.TrimPrefix(playlistJobID, "pl-")
	if len(slug) > 32 {
		slug = slug[:32]
	}
	keyTitle := title
	if albumCompletion && album != "" {
		keyTitle = ""
	}
	parts := []string{playlistJobID, normalizeKey(artist), normalizeKey(album), normalizeKey(keyTitle)}
	if neteaseOnly {
		parts = append(parts, "netease-only")
	}
	jobKey, _ := json.Marshal(parts)
	sum := sha256.Sum256(jobKey)
	baseJobID := "trk-" + slug + "-" + hex.EncodeToString(sum[:6])
	jobID := baseJobID
	existing, err := a.flows.Job(baseJobID)
	if err != nil {
		return "", err
	}
	if existing != nil {
		if !existing.CancelRequested && playlistJobReusable(existing.Stage) {
			return baseJobID, nil
		}
		jobID, err = a.replacementPlaylistJobID(baseJobID, existing.Revision)
		if err != nil {
			return "", err
		}
	}
	identity := a.playlistIdentity(client, jobID, artist, album, title)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	plan := SearchPlan{
		RootJobID: jobID, Scene: "expand",
		Query: strings.TrimSpace(artist + " " + firstNonEmpty(title, expected)), ExpectedTitle: expected, ExpectedArtist: artist,
		EnabledSites: sites, StreamingEnabled: true, Identity: identity, Budget: budget, PlaylistTrack: title,
		NeteaseOnly: neteaseOnly, NeteaseTrackOnly: neteaseOnly && !albumCompletion, NeteaseSongID: row.NeteaseSongID, NeteaseAlbumID: row.NeteaseAlbumID,
		AlbumArtist: func() string {
			if isHotSlowCompilationAlbum(album) {
				return hotSlowCompilationArtist
			}
			return ""
		}(),
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	label := expected
	if artist != "" {
		label = artist + " - " + expected
	}
	evidence := plan.evidence()
	if evidence.ID == "" {
		evidence = playlistRowEvidence(jobID, playlistJobID, row)
	}
	if _, err := a.flows.Start("start-"+jobID, workflow.Start{
		JobID: jobID, BatchID: playlistJobID, Scene: "expand", Stage: "identifying", Label: label,
		Payload: raw, Evidence: []workflow.EvidenceInput{evidence}, NowNS: a.now().UnixNano(),
	}); err != nil {
		return "", err
	}
	return jobID, nil
}

// playlistIdentity states what the playlist itself asserted: this artist has
// this title on this album. It is platform catalogue data, not an AI guess, so
// it is enough to start a search — the search decides the release, and the
// review model decides ambiguous candidates. A catalogue lookup is tried first
// because a MusicBrainz release id is the better claim, but the playlist row
// is a valid identity on its own: waiting for a catalogue that may not answer
// would park every track at a manual checkpoint, which is the behaviour this
// whole path exists to remove.
func (a *App) playlistIdentity(client *musicbrainz.Client, jobID, artist, album, title string) Identity {
	if artist == "" || album == "" {
		return Identity{}
	}
	if client != nil {
		if lookup, err := identifyCatalogLookup(client, a.engine, catalogIdentifyRequest{
			Artist: artist, Album: album, Title: title,
			Tracks: []catalogTrackRequest{{Title: title, Artists: []string{artist}}},
		}, catalogBudget, catalogTTL); err == nil && lookup.MusicBrainz != nil && lookup.MusicBrainz.ReleaseID != "" {
			for _, result := range lookup.Results {
				if result.IdentityConfirmed && result.Status == "confirmed" {
					payload, _ := json.Marshal(lookup.MusicBrainz)
					sum := sha256.Sum256([]byte(jobID + "/" + lookup.MusicBrainz.ReleaseID))
					return Identity{
						ID:   "plid" + hex.EncodeToString(sum[:10]),
						Kind: "catalog", Source: "musicbrainz", SubjectID: lookup.MusicBrainz.ReleaseID,
						TracklistVerified: true, IdentityConfirmed: true, Payload: string(payload),
					}
				}
			}
		}
	}
	payload, _ := json.Marshal(map[string]any{"artist": artist, "album": album, "title": title, "source": "playlist"})
	// The evidence id includes the job: the store keeps ids unique across the
	// whole profile, so two jobs built from the same row (a re-enqueue, or a
	// second row from the same album) must not reuse one.
	sum := sha256.Sum256(append([]byte(jobID+"/"+artist+"/"+album+"/"+title+"/"), payload...))
	return Identity{
		ID: "plsrc" + hex.EncodeToString(sum[:10]), Kind: "api", Source: "playlist",
		SubjectID: "", TracklistVerified: true, IdentityConfirmed: true, Payload: string(payload),
	}
}

// playlistRowEvidence records the row a job came from when the catalogue had
// nothing to confirm.
func playlistRowEvidence(jobID, playlistJobID string, row playlistTrack) workflow.EvidenceInput {
	payload, _ := json.Marshal(map[string]any{"artist": row.Artist, "title": row.Title, "album": row.Album, "duration_ms": row.Duration, "netease_song_id": row.NeteaseSongID, "netease_album_id": row.NeteaseAlbumID, "playlist_job": playlistJobID})
	sum := sha256.Sum256(append([]byte(jobID+"/"), payload...))
	return workflow.EvidenceInput{
		ID: "plrow" + hex.EncodeToString(sum[:10]), Kind: "catalog", Source: "playlist",
		SubjectID: playlistJobID, Payload: payload,
	}
}

func playlistSelection(total int, requested []int) []int {
	if len(requested) == 0 {
		out := make([]int, total)
		for i := range out {
			out[i] = i
		}
		return out
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(requested))
	for _, index := range requested {
		if index < 0 || index >= total || seen[index] {
			continue
		}
		seen[index] = true
		out = append(out, index)
	}
	return out
}

// playlistTrackRows reads the M3U a playlist job was created with.
func (a *App) playlistTrackRows(jobID string) ([]playlistTrack, string, error) {
	path, found := a.playlistFile(jobID)
	if !found {
		return nil, "", fmt.Errorf("没有找到这份歌单的曲目文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	rows, name, err := parseM3U(data)
	if err != nil {
		return nil, "", err
	}
	if len(rows) == 0 {
		return nil, name, fmt.Errorf("歌单里没有曲目")
	}
	return rows, name, nil
}

// playlistTracksView is GET /v1/playlists/tracks?job=<id>.
func (a *App) playlistTracksView(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimSpace(r.URL.Query().Get("job"))
	if !workflow.ValidID(jobID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	rows, name, err := a.playlistTrackRows(jobID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "PLAYLIST_NOT_FOUND", "error": err.Error()})
		return
	}
	library := a.playlistLibrary()
	type rowView struct {
		Index   int    `json:"index"`
		Artist  string `json:"artist"`
		Title   string `json:"title"`
		Album   string `json:"album,omitempty"`
		Present bool   `json:"present"`
	}
	out := make([]rowView, 0, len(rows))
	for i, row := range rows {
		out = append(out, rowView{Index: i, Artist: row.Artist, Title: row.Title, Album: row.Album, Present: library.hasTrack(row.Artist, row.Album, row.Title)})
	}
	library.commitPresenceSnapshots()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name, "tracks": out, "total": len(out)})
}

// enqueuePlaylist is POST /v1/playlists/enqueue {job_id, indices, album_completion}.
func (a *App) enqueuePlaylist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		JobID           string `json:"job_id"`
		Indices         []int  `json:"indices"`
		AlbumCompletion *bool  `json:"album_completion"`
		NeteaseOnly     bool   `json:"netease_only"`
	}
	if err := decode(r, &body); err != nil || !workflow.ValidID(body.JobID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	if body.NeteaseOnly && !strings.HasPrefix(body.JobID, "pl-netease-") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "NETEASE_ORIGIN_REQUIRED"})
		return
	}
	rows, name, err := a.playlistTrackRows(body.JobID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "PLAYLIST_NOT_FOUND", "error": err.Error()})
		return
	}
	indices := playlistSelection(len(rows), body.Indices)
	if len(indices) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "没有选中任何曲目"})
		return
	}
	albumCompletion := true
	if body.AlbumCompletion != nil {
		albumCompletion = *body.AlbumCompletion
	}
	report, err := a.enqueuePlaylistTracks(r.Context(), body.JobID, rows, indices, playlistEnqueueOptions{AlbumCompletion: albumCompletion, NeteaseOnly: body.NeteaseOnly})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "PLAYLIST_ENQUEUE_FAILED", "error": err.Error()})
		return
	}
	// Retiring the list job is cosmetic: if it fails (already terminal),
	// the track jobs still started, so the response stays a success.
	_, _ = a.flows.Control("pl-enqueue-"+body.JobID, body.JobID, "cancel", a.now().UnixNano())
	note := "已开始 " + strconv.Itoa(len(report.Created)) + " 个任务（" + strconv.Itoa(report.Albums) + " 张专辑），已跳过库里已有的 " + strconv.Itoa(len(report.Skipped)) + " 首。"
	if len(report.Unmatched) > 0 {
		note += " 有 " + strconv.Itoa(len(report.Unmatched)) + " 项没能自动建任务，会留在核对里。"
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "name": name, "report": report, "note": note})
}
