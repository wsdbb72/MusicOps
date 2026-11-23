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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

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
	titleCandidates  map[string][]string
	albumMembers     map[string][]string
	verified         map[string]bool
	tagTitles        map[[2]string]map[string]bool
	compilationTags  map[string]map[[2]string]bool
	albumGaps        map[[2]string]bool
	records          map[string]audiorelease.Entry
	pendingSnapshots map[string]audiorelease.VerifiedPresence
	presence         *playlistPresenceCache
	metadata         *playlistMetadataCache
}

type playlistPresenceResult struct {
	unchanged bool
	snapshot  *audiorelease.VerifiedPresence
}

type playlistPresenceFlight struct {
	done      chan struct{}
	unchanged bool
}

// playlistPresenceCache lets workers share verification results. A common
// title can have many candidate files, and without single-flight
// deduplication each worker hashed the same unchanged recording again.
type playlistPresenceCache struct {
	mu      sync.Mutex
	results map[string]playlistPresenceResult
	flights map[string]*playlistPresenceFlight
	pending map[string]audiorelease.VerifiedPresence
}

func newPlaylistPresenceCache() *playlistPresenceCache {
	return &playlistPresenceCache{
		results: map[string]playlistPresenceResult{},
		flights: map[string]*playlistPresenceFlight{},
		pending: map[string]audiorelease.VerifiedPresence{},
	}
}

func (cache *playlistPresenceCache) verify(id string, compute func() playlistPresenceResult) bool {
	cache.mu.Lock()
	if result, found := cache.results[id]; found {
		cache.mu.Unlock()
		return result.unchanged
	}
	if flight, found := cache.flights[id]; found {
		cache.mu.Unlock()
		<-flight.done
		return flight.unchanged
	}
	flight := &playlistPresenceFlight{done: make(chan struct{})}
	cache.flights[id] = flight
	cache.mu.Unlock()

	result := compute()

	cache.mu.Lock()
	flight.unchanged = result.unchanged
	cache.results[id] = result
	if result.snapshot != nil {
		cache.pending[id] = *result.snapshot
	}
	delete(cache.flights, id)
	close(flight.done)
	cache.mu.Unlock()
	return result.unchanged
}

func (cache *playlistPresenceCache) takePending() []audiorelease.VerifiedPresence {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.pending) == 0 {
		return nil
	}
	items := make([]audiorelease.VerifiedPresence, 0, len(cache.pending))
	for _, item := range cache.pending {
		items = append(items, item)
	}
	cache.pending = map[string]audiorelease.VerifiedPresence{}
	return items
}

type playlistRecordingTags struct {
	artists    []string
	album      string
	title      string
	duration   float64
	trackTotal int
}

type playlistRecordingFlight struct {
	done chan struct{}
	tags playlistRecordingTags
}

// playlistMetadataCache memoizes embedded tags and derived album views. A
// compilation album can contain thousands of files, so repeated previews must
// not open and parse every candidate again during the cache window.
type playlistMetadataCache struct {
	mu                 sync.Mutex
	recordings         map[string]playlistRecordingTags
	recordingFlights   map[string]*playlistRecordingFlight
	tagTitles          map[[2]string]map[string]bool
	tagTitlesFlights   map[[2]string]chan struct{}
	compilation        map[string]map[[2]string]bool
	compilationFlights map[string]chan struct{}
	albumGaps          map[[2]string]bool
	albumGapFlights    map[[2]string]chan struct{}
}

func newPlaylistMetadataCache() *playlistMetadataCache {
	return &playlistMetadataCache{
		recordings:         map[string]playlistRecordingTags{},
		recordingFlights:   map[string]*playlistRecordingFlight{},
		tagTitles:          map[[2]string]map[string]bool{},
		tagTitlesFlights:   map[[2]string]chan struct{}{},
		compilation:        map[string]map[[2]string]bool{},
		compilationFlights: map[string]chan struct{}{},
		albumGaps:          map[[2]string]bool{},
		albumGapFlights:    map[[2]string]chan struct{}{},
	}
}

func (cache *playlistMetadataCache) recording(path string, compute func() playlistRecordingTags) playlistRecordingTags {
	cache.mu.Lock()
	if result, found := cache.recordings[path]; found {
		cache.mu.Unlock()
		return result
	}
	if flight, found := cache.recordingFlights[path]; found {
		cache.mu.Unlock()
		<-flight.done
		return flight.tags
	}
	flight := &playlistRecordingFlight{done: make(chan struct{})}
	cache.recordingFlights[path] = flight
	cache.mu.Unlock()

	result := compute()

	cache.mu.Lock()
	flight.tags = result
	cache.recordings[path] = result
	delete(cache.recordingFlights, path)
	close(flight.done)
	cache.mu.Unlock()
	return result
}

func (cache *playlistMetadataCache) tagTitlesFor(key [2]string, compute func() map[string]bool) map[string]bool {
	cache.mu.Lock()
	if result, found := cache.tagTitles[key]; found {
		cache.mu.Unlock()
		return result
	}
	if flight, found := cache.tagTitlesFlights[key]; found {
		cache.mu.Unlock()
		<-flight
		cache.mu.Lock()
		result := cache.tagTitles[key]
		cache.mu.Unlock()
		return result
	}
	flight := make(chan struct{})
	cache.tagTitlesFlights[key] = flight
	cache.mu.Unlock()

	result := compute()

	cache.mu.Lock()
	cache.tagTitles[key] = result
	delete(cache.tagTitlesFlights, key)
	close(flight)
	cache.mu.Unlock()
	return result
}

func (cache *playlistMetadataCache) compilationFor(album string, compute func() map[[2]string]bool) map[[2]string]bool {
	cache.mu.Lock()
	if result, found := cache.compilation[album]; found {
		cache.mu.Unlock()
		return result
	}
	if flight, found := cache.compilationFlights[album]; found {
		cache.mu.Unlock()
		<-flight
		cache.mu.Lock()
		result := cache.compilation[album]
		cache.mu.Unlock()
		return result
	}
	flight := make(chan struct{})
	cache.compilationFlights[album] = flight
	cache.mu.Unlock()

	result := compute()

	cache.mu.Lock()
	cache.compilation[album] = result
	delete(cache.compilationFlights, album)
	close(flight)
	cache.mu.Unlock()
	return result
}

func (cache *playlistMetadataCache) albumGapFor(key [2]string, compute func() bool) bool {
	cache.mu.Lock()
	if result, found := cache.albumGaps[key]; found {
		cache.mu.Unlock()
		return result
	}
	if flight, found := cache.albumGapFlights[key]; found {
		cache.mu.Unlock()
		<-flight
		cache.mu.Lock()
		result := cache.albumGaps[key]
		cache.mu.Unlock()
		return result
	}
	flight := make(chan struct{})
	cache.albumGapFlights[key] = flight
	cache.mu.Unlock()

	result := compute()

	cache.mu.Lock()
	cache.albumGaps[key] = result
	delete(cache.albumGapFlights, key)
	close(flight)
	cache.mu.Unlock()
	return result
}

var playlistTitleSeparators = strings.NewReplacer(
	" – ", "\x00",
	" — ", "\x00",
	" - ", "\x00",
	"｜", "\x00",
	" | ", "\x00",
)

func normalizeKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	replacer := strings.NewReplacer("_", " ", "-", " ", "/", " ", "　", " ", "’", "'", "‘", "'", "“", "\"", "”", "\"")
	s = replacer.Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// trackPathOf strips the track number and extension while preserving the raw
// artist, album and title text. Callers choose the comparison key appropriate
// for their operation.
func trackPathOf(file string) (artist, album, title string) {
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
	return filepath.Base(filepath.Dir(dir)), filepath.Base(dir), base
}

// trackKeyOfPath is the duplicate-scan key. It retains punctuation that can
// distinguish editions while folding path separators and whitespace.
func trackKeyOfPath(file string) (artist, album, title string) {
	artist, album, title = trackPathOf(file)
	return normalizeKey(artist), normalizeKey(album), normalizeKey(title)
}

// libraryPathKey is the playlist-presence key. Filesystem-safe publishing may
// replace punctuation such as ':' or '?' with underscores, while the source
// playlist keeps the catalogue spelling. Comparing letters and digits keeps
// those same recordings together without collapsing different worded albums.
func libraryPathKey(value string) string {
	var out strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(unicode.ToLower(r))
			continue
		}
		out.WriteByte(' ')
	}
	key := strings.Join(strings.Fields(out.String()), " ")
	if key == "" && strings.TrimSpace(value) != "" {
		return "\x00punctuation"
	}
	return key
}

// foldPlaylistMatchText normalizes bounded orthographic variants that commonly
// split the same recording between Chinese catalogue metadata and Japanese or
// Traditional tags. It deliberately does not strip title qualifiers or words.
func foldPlaylistMatchText(value string) string {
	var b strings.Builder
	for _, r := range foldTagText(value) {
		switch r {
		case '願':
			r = '愿'
		case '後':
			r = '后'
		case '間':
			r = '间'
		case '觸':
			r = '触'
		case '電':
			r = '电'
		case '搖':
			r = '摇'
		case '樂':
			r = '乐'
		case '愛':
			r = '爱'
		case '時':
			r = '时'
		case '說':
			r = '说'
		case '會':
			r = '会'
		case '來':
			r = '来'
		case '為':
			r = '为'
		case '無':
			r = '无'
		case '與':
			r = '与'
		case '這':
			r = '这'
		case '過':
			r = '过'
		case '還':
			r = '还'
		case '將':
			r = '将'
		case '對':
			r = '对'
		case '萬':
			r = '万'
		case '歲':
			r = '岁'
		case '長':
			r = '长'
		case '場':
			r = '场'
		case '飛':
			r = '飞'
		case '車':
			r = '车'
		case '門':
			r = '门'
		case '風':
			r = '风'
		case '雲':
			r = '云'
		case '見':
			r = '见'
		case '實':
			r = '实'
		case '現':
			r = '现'
		case '開':
			r = '开'
		case '關':
			r = '关'
		case '發':
			r = '发'
		case '頭':
			r = '头'
		case '體':
			r = '体'
		case '聽':
			r = '听'
		case '讀':
			r = '读'
		case '書':
			r = '书'
		case '畫':
			r = '画'
		case '數':
			r = '数'
		case '當':
			r = '当'
		case '麼':
			r = '么'
		case '樣':
			r = '样'
		case '讓':
			r = '让'
		case '從':
			r = '从'
		case '～', '〜', '~', '…', '･', '・', '•':
			r = ' '
		case '—', '–', '−', '－':
			r = '-'
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func playlistMatchKey(value string) string {
	return libraryPathKey(foldPlaylistMatchText(value))
}

func trackPathKeyOfPath(file string) (artist, album, title string) {
	artist, album, title = trackPathOf(file)
	return playlistMatchKey(artist), playlistMatchKey(album), playlistMatchKey(title)
}

// playlistTitleCandidateKeys indexes both the complete stem and the possible
// title suffixes after an "Artist - Title" separator. The suffix is only a
// lookup optimization: hasTrack still verifies the embedded title, album and
// credited artists before treating a file as the same recording.
func playlistTitleCandidateKeys(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, 3)
	add := func(value string) {
		for _, value := range []string{value, foldPlaylistMatchText(value)} {
			key := libraryPathKey(value)
			for _, key := range []string{key, playlistCompactTitleKey(key)} {
				if key != "" && !seen[key] {
					seen[key] = true
					out = append(out, key)
				}
			}
		}
	}
	add(raw)
	parts := strings.Split(playlistTitleSeparators.Replace(raw), "\x00")
	for i := 1; i < len(parts); i++ {
		add(strings.Join(parts[i:], " - "))
	}
	return out
}

func playlistCompactTitleKey(key string) string {
	if strings.HasPrefix(key, "\x00") {
		return ""
	}
	return "\x00compact:" + strings.ReplaceAll(key, " ", "")
}

const (
	playlistLibraryCacheTTL = 10 * time.Minute
	// Presence checks may need to inspect thousands of files on a slow library
	// disk. Immutable catalogue maps and the single-flight file verification
	// cache are shared; only each worker's tag memo remains independent.
	playlistPresenceWorkers = 4
)

func (a *App) playlistLibrary() playlistLibraryIndex {
	// One immutable listing and its name index are reused by the preview and
	// its immediate enqueue. Each file's state is still verified on first use.
	a.playlistEntriesMu.Lock()
	defer a.playlistEntriesMu.Unlock()
	fresh := false
	if !a.playlistEntriesCached || a.now().Sub(a.playlistEntriesAt) >= playlistLibraryCacheTTL {
		entries, err := audiorelease.ListIndexed(a.engine)
		if err == nil {
			a.playlistEntriesCache = entries
			a.playlistEntriesCached = true
			a.playlistEntriesAt = a.now()
			fresh = true
		}
	}
	if fresh || !a.playlistIndexCached {
		a.playlistIndexCache = indexPlaylistLibrary(a.engine, a.playlistEntriesCache)
		a.playlistIndexCached = true
	}
	return a.playlistIndexCache
}

func (index playlistLibraryIndex) presenceWorker() playlistLibraryIndex {
	return playlistLibraryIndex{
		engine: index.engine, tracks: index.tracks, albums: index.albums,
		titleCandidates: index.titleCandidates, albumMembers: index.albumMembers,
		verified: map[string]bool{}, tagTitles: map[[2]string]map[string]bool{},
		compilationTags: map[string]map[[2]string]bool{}, albumGaps: map[[2]string]bool{},
		records: index.records, pendingSnapshots: map[string]audiorelease.VerifiedPresence{},
		presence: index.presence, metadata: index.metadata,
	}
}

// playlistPresence evaluates rows concurrently. The expensive step is the
// per-file unchanged check on external/mechanical library disks, so serial
// iteration made a 4,000-track list block for minutes even though catalogue
// matching itself was in memory.
func (a *App) playlistPresence(rows []playlistTrack) []bool {
	// The cached name index and shared verification memo stay valid for this
	// cache window. Every library mutation explicitly invalidates both, while
	// a repeated preview/enqueue no longer rebuilds the 75k-record map.
	return playlistPresenceForIndex(a.playlistLibrary(), rows)
}

func playlistPresenceForIndex(index playlistLibraryIndex, rows []playlistTrack) []bool {
	present := make([]bool, len(rows))
	workers := playlistPresenceWorkers
	if len(rows) < workers {
		workers = len(rows)
	}
	if workers <= 1 {
		local := index.presenceWorker()
		for i, row := range rows {
			present[i] = local.hasPlaylistRow(row)
		}
		local.commitPresenceSnapshots()
		return present
	}
	type result struct {
		index   int
		present bool
	}
	jobs := make(chan int)
	results := make(chan result, workers*2)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		local := index.presenceWorker()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results <- result{index: i, present: local.hasPlaylistRow(rows[i])}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i := range rows {
			jobs <- i
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()
	for item := range results {
		present[item.index] = item.present
	}
	index.commitPresenceSnapshots()
	return present
}

// invalidatePlaylistLibraryCache makes the next presence check observe a
// just-published, moved or removed file. The cache only serves the large
// preview/enqueue pair; library mutations must not leave a two-minute blind
// spot that re-queues an already ingested track.
func (a *App) invalidatePlaylistLibraryCache() {
	if a == nil {
		return
	}
	a.playlistEntriesMu.Lock()
	a.playlistEntriesCache = nil
	a.playlistEntriesCached = false
	a.playlistEntriesAt = time.Time{}
	a.playlistIndexCache = playlistLibraryIndex{}
	a.playlistIndexCached = false
	a.playlistEntriesMu.Unlock()
}
func indexPlaylistLibrary(engine *dataengine.Engine, entries []audiorelease.Entry) playlistLibraryIndex {
	index := playlistLibraryIndex{engine: engine, tracks: map[[3]string][]string{}, albums: map[[2]string][]string{}, titleCandidates: map[string][]string{}, albumMembers: map[string][]string{}, verified: map[string]bool{}, tagTitles: map[[2]string]map[string]bool{}, compilationTags: map[string]map[[2]string]bool{}, albumGaps: map[[2]string]bool{}, records: map[string]audiorelease.Entry{}, pendingSnapshots: map[string]audiorelease.VerifiedPresence{}, presence: newPlaylistPresenceCache(), metadata: newPlaylistMetadataCache()}
	for _, entry := range entries {
		if !entry.FilePresent {
			continue
		}
		index.records[entry.RequestID] = entry
		rawArtist, rawAlbum, rawTitle := trackPathOf(entry.File)
		artist, album, title := playlistMatchKey(rawArtist), playlistMatchKey(rawAlbum), playlistMatchKey(rawTitle)
		if artist != "" && artist != "." && album != "" && album != "." && title != "" {
			key := [3]string{artist, album, title}
			index.tracks[key] = append(index.tracks[key], entry.RequestID)
			for _, candidate := range playlistTitleCandidateKeys(rawTitle) {
				index.titleCandidates[candidate] = append(index.titleCandidates[candidate], entry.RequestID)
			}
			albumKey := [2]string{artist, album}
			index.albums[albumKey] = append(index.albums[albumKey], entry.RequestID)
			index.albumMembers[album] = append(index.albumMembers[album], entry.RequestID)
		}
	}
	return index
}

func (index playlistLibraryIndex) recording(path string) playlistRecordingTags {
	compute := func() playlistRecordingTags {
		if strings.EqualFold(filepath.Ext(path), ".flac") {
			if tags, err := readFLACTagsPrefix(path); err == nil {
				duration, _ := flacDuration(tags.StreamInfo)
				total := firstValue(tags.Raw["TRACKTOTAL"])
				if total == "" {
					total = firstValue(tags.Raw["TOTALTRACKS"])
				}
				if total == "" {
					_, total, _ = strings.Cut(firstValue(tags.Raw["TRACKNUMBER"]), "/")
				}
				trackTotal, _ := strconv.Atoi(total)
				return playlistRecordingTags{artists: tags.Artists, album: tags.Album, title: tags.Title, duration: duration, trackTotal: trackTotal}
			}
			return playlistRecordingTags{}
		}
		artists, album, title, duration := playlistRecordingEvidence(path)
		return playlistRecordingTags{artists: artists, album: album, title: title, duration: duration}
	}
	if index.metadata != nil {
		return index.metadata.recording(path, compute)
	}
	return compute()
}

func (index playlistLibraryIndex) trackTags(path string) (artists []string, album, title string) {
	tags := index.recording(path)
	return tags.artists, tags.album, tags.title
}

func (index playlistLibraryIndex) albumTagTitles(key [2]string) map[string]bool {
	build := func() map[string]bool {
		titles := map[string]bool{}
		for _, id := range index.albums[key] {
			if !index.isUnchanged(id) {
				continue
			}
			path, found, err := audiorelease.PathFor(index.engine, id)
			if err != nil || !found || !strings.EqualFold(filepath.Ext(path), ".flac") {
				continue
			}
			tags := index.recording(path)
			if playlistMatchKey(tags.album) == key[1] && playlistMatchKey(tags.title) != "" {
				titles[playlistMatchKey(tags.title)] = true
			}
		}
		return titles
	}
	if index.metadata != nil {
		return index.metadata.tagTitlesFor(key, build)
	}
	if titles, checked := index.tagTitles[key]; checked {
		return titles
	}
	titles := build()
	index.tagTitles[key] = titles
	return titles
}
func (index playlistLibraryIndex) hasPlaylistRow(row playlistTrack) bool {
	if index.hasTrack(row.Artist, row.Album, row.Title) {
		return true
	}
	seconds, err := strconv.ParseFloat(row.Duration, 64)
	if row.NeteaseSongID <= 0 || err != nil || !(seconds > 0 && seconds <= 24*3600) ||
		isHotSlowCompilationAlbum(row.Album) {
		return false
	}
	durationMS := int64(seconds * 1000)
	truncatedMS := int64(0)
	if _, err := strconv.Atoi(row.Duration); err == nil {
		truncatedMS = 999
	}
	titleKeys := []string{playlistMatchKey(row.Title)}
	if featured := playlistFeaturedTitleKey(row.Title, row.Artist); featured != "" && featured != titleKeys[0] {
		titleKeys = append(titleKeys, featured)
	}
	ids := make([]string, 0, 32)
	seen := map[string]bool{}
	add := func(candidates []string) {
		for _, id := range candidates {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	for _, key := range titleKeys {
		add(index.titleCandidates[key])
		add(index.titleCandidates[playlistCompactTitleKey(key)])
	}
	if strings.TrimSpace(row.Album) != "" {
		albumKey := [2]string{playlistMatchKey(publishComponent(row.Artist)), playlistMatchKey(publishComponent(row.Album))}
		add(index.albums[albumKey])
	}
	// Playlist membership is recording-level. A single release may be filed
	// under its parent album, while uploader tags can credit a compilation or a
	// different romanization. Accept cross-album evidence only when title,
	// credited artist and measured duration still agree; an exact album/path
	// match can use the normal three-second padding tolerance.
	for _, id := range ids {
		if !index.isUnchanged(id) {
			continue
		}
		path, found, err := audiorelease.PathFor(index.engine, id)
		if err != nil || !found {
			continue
		}
		tags := index.recording(path)
		artists, albumTag, titleTag, duration := tags.artists, tags.album, tags.title, tags.duration
		pathArtist, pathAlbum, pathTitle := trackPathOf(path)
		if titleTag == "" {
			titleTag = pathTitle
		}
		if !neteaseAlternateTitleMatches(row.Title, titleTag) {
			continue
		}
		artistMatches := playlistArtistTagMatches(row.Artist, artists)
		if !artistMatches {
			artistMatches = playlistArtistFallbackMatches(row.Artist, pathArtist)
		}
		if !artistMatches {
			continue
		}
		if duration <= 0 {
			continue
		}
		albumMatches := strings.TrimSpace(row.Album) == "" || playlistAlbumTagMatches(row.Album, albumTag) ||
			playlistMatchKey(row.Album) == playlistMatchKey(pathAlbum)
		tolerance := int64(1500)
		if albumMatches {
			tolerance = 3000
		}
		delta := int64(duration*1000) - durationMS
		if delta < 0 {
			delta = -delta
		}
		if delta <= tolerance+truncatedMS {
			return true
		}
	}
	return false
}

// hasRecordingIdentity answers the narrow recovery question used for stale
// tracker failures: is there a verified local file with this exact title and
// credited artist? Duration is intentionally unavailable on old failure
// records, so callers must only use this after an explicit library reindex and
// only for acquisition failures that can never hide a quality-upgrade debt.
func (index playlistLibraryIndex) hasRecordingIdentity(artist, title string) bool {
	artist = strings.TrimSpace(artist)
	title = strings.TrimSpace(title)
	if artist == "" || title == "" {
		return false
	}
	seen := map[string]bool{}
	for _, key := range playlistTitleCandidateKeys(title) {
		for _, id := range index.titleCandidates[key] {
			if seen[id] || !index.isUnchanged(id) {
				continue
			}
			seen[id] = true
			path, found, err := audiorelease.PathFor(index.engine, id)
			if err != nil || !found {
				continue
			}
			tags := index.recording(path)
			artists, titleTag := tags.artists, tags.title
			pathArtist, _, pathTitle := trackPathOf(path)
			if titleTag == "" {
				titleTag = pathTitle
			}
			if !neteaseAlternateTitleMatches(title, titleTag) {
				continue
			}
			if playlistArtistTagMatches(artist, artists) ||
				playlistArtistFallbackMatches(artist, pathArtist) {
				return true
			}
		}
	}
	return false
}
func (index playlistLibraryIndex) hasTrack(artist, album, title string) bool {
	// A source row without an album is published under its own title.
	// Use that same folder when checking whether it has already been filed.
	key := [2]string{playlistMatchKey(artist), playlistMatchKey(firstNonEmpty(album, title))}
	want := playlistMatchKey(title)
	if key[0] == "" || key[1] == "" || want == "" {
		return false
	}
	if isHotSlowCompilationAlbum(album) {
		return index.hasCompilationTrack(artist, key[1], want)
	}
	for _, id := range index.tracks[[3]string{key[0], key[1], want}] {
		if index.isUnchanged(id) {
			return true
		}
	}
	if index.albumTagTitles(key)[want] {
		return true
	}
	// A filesystem component may be truncated or otherwise altered after the
	// catalogue identity was written. Candidate files are found by title, then
	// accepted only when the embedded full album and every credited artist also
	// match and the file snapshot is unchanged.
	expectedAlbum := firstNonEmpty(album, title)
	titleKeys := []string{want}
	featuredTitle := playlistFeaturedTitleKey(title, artist)
	if featuredTitle != "" && featuredTitle != want {
		titleKeys = append(titleKeys, featuredTitle)
	}
	for _, titleKey := range titleKeys {
		ids := append([]string(nil), index.titleCandidates[titleKey]...)
		ids = append(ids, index.titleCandidates[playlistCompactTitleKey(titleKey)]...)
		for _, id := range ids {
			if !index.isUnchanged(id) {
				continue
			}
			path, found, err := audiorelease.PathFor(index.engine, id)
			if err != nil || !found {
				continue
			}
			artists, albumTag, titleTag := index.trackTags(path)
			if playlistMatchKey(titleTag) != titleKey || !playlistAlbumTagMatches(expectedAlbum, albumTag) {
				continue
			}
			if playlistArtistTagMatches(artist, artists) {
				return true
			}
		}
	}
	return false
}

// A title may repeat a guest already present in the playlist credits. Keep
// every other version suffix and still require matching album and artist tags.
func playlistFeaturedTitleKey(title, artist string) string {
	marker := playlistFeaturing.FindStringIndex(title)
	if marker == nil {
		return ""
	}
	guest := libraryPathKey(strings.TrimSpace(strings.TrimSuffix(title[marker[1]:], ")")))
	credits := splitPlaylistArtists(artist)
	if len(credits) < 2 || guest == "" {
		return ""
	}
	for _, credit := range credits {
		if libraryPathKey(credit) == guest {
			return playlistMatchKey(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(title[:marker[0]]), "(")))
		}
	}
	return ""
}

var playlistFeaturing = regexp.MustCompile(`(?i) feat\. +`)

func playlistRecordingEvidence(path string) (artists []string, album, title string, duration float64) {
	if strings.EqualFold(filepath.Ext(path), ".flac") {
		if tags, err := readFLACTagsPrefix(path); err == nil {
			duration, _ = flacDuration(tags.StreamInfo)
			return tags.Artists, tags.Album, tags.Title, duration
		}
		return nil, "", "", 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if info, err := probeGenericAudio(ctx, path); err == nil {
		artist := firstNonEmpty(info.Artist, info.AlbumArtist, info.Tags["artist"], info.Tags["artists"])
		return splitPlaylistArtists(artist), firstNonEmpty(info.Album, info.Tags["album"]), firstNonEmpty(info.Title, info.Tags["title"]), info.Duration
	}
	return nil, "", "", 0
}

func playlistTrackTags(path string) (artists []string, album, title string) {
	artists, album, title, _ = playlistRecordingEvidence(path)
	return artists, album, title
}

func playlistAlbumTagMatches(expected, actual string) bool {
	want := playlistMatchKey(expected)
	got := playlistMatchKey(actual)
	if want == "" || got == "" {
		return false
	}
	if got == want {
		return true
	}
	// Some tools prepend the release artist to the album tag while the
	// playlist carries only the release title. Artist evidence is checked
	// separately, so a complete word-boundary suffix is safe here.
	return strings.HasSuffix(got, " "+want)
}

func playlistArtistFallbackMatches(expected, pathArtist string) bool {
	want := splitPlaylistArtists(expected)
	if playlistArtistsMatch(want, []string{pathArtist}) {
		return true
	}
	// Publish paths use an underscore where the source catalogue used a slash,
	// while imported folders often use a comma or ampersand between credits.
	// Split those separators only when the playlist itself names multiple
	// artists, so a legitimate comma inside one stage name cannot regress.
	if len(flattenPlaylistArtists(want)) < 2 {
		return false
	}
	normalized := strings.NewReplacer(
		",", "/", "，", "/", "&", "/", "＆", "/", "+", "/", "＋", "/", " and ", "/", " & ", "/",
	).Replace(pathArtist)
	return playlistArtistsMatch(want, splitPlaylistArtists(normalized))
}
func playlistArtistTagMatches(expected string, actual []string) bool {
	if len(actual) == 0 {
		return false
	}
	switch libraryPathKey(expected) {
	case "various artists", "群星":
		for _, artist := range actual {
			if libraryPathKey(artist) != "" {
				return true
			}
		}
		return false
	default:
		return playlistArtistsMatch(splitPlaylistArtists(expected), actual)
	}
}

// Compilation folders share an album artist, so only the embedded track
// artist can distinguish recordings with the same title.
func (index playlistLibraryIndex) hasCompilationTrack(artist, album, title string) bool {
	pathArtist := playlistMatchKey(artist)
	for _, id := range index.tracks[[3]string{pathArtist, album, playlistMatchKey(title)}] {
		if pathArtist != playlistMatchKey(hotSlowCompilationArtist) && index.isUnchanged(id) {
			return true
		}
	}
	unified := playlistMatchKey(hotSlowCompilationAlbum)
	track := [2]string{compilationArtistKey([]string{artist}), playlistMatchKey(title)}
	return index.compilationMatches(unified)[track] || album != unified && index.compilationMatches(album)[track]
}

func (index playlistLibraryIndex) compilationMatches(album string) map[[2]string]bool {
	build := func() map[[2]string]bool {
		matches := map[[2]string]bool{}
		for _, id := range index.albumMembers[album] {
			if !index.isUnchanged(id) {
				continue
			}
			path, found, err := audiorelease.PathFor(index.engine, id)
			if err != nil || !found {
				continue
			}
			artists, albumTag, titleTag := index.trackTags(path)
			artistKey := compilationArtistKey(artists)
			if isHotSlowCompilationAlbum(albumTag) && artistKey != "" && titleTag != "" {
				matches[[2]string{artistKey, playlistMatchKey(titleTag)}] = true
			}
		}
		return matches
	}
	if index.metadata != nil {
		return index.metadata.compilationFor(album, build)
	}
	if matches, checked := index.compilationTags[album]; checked {
		return matches
	}
	matches := build()
	index.compilationTags[album] = matches
	return matches
}

func compilationArtistKey(values []string) string {
	var artists []string
	for _, value := range values {
		for _, artist := range splitPlaylistArtists(value) {
			if key := playlistMatchKey(artist); key != "" {
				artists = append(artists, key)
			}
		}
	}
	sort.Strings(artists)
	return strings.Join(artists, "\x00")
}

func compilationTrackTags(path string) (artists []string, album, title string) {
	return playlistTrackTags(path)
}

func (index playlistLibraryIndex) isUnchanged(id string) bool {
	compute := func() playlistPresenceResult {
		if entry, found := index.records[id]; found {
			ok := entry.PresenceUnchanged()
			if !ok || entry.HasIndexedStat() {
				return playlistPresenceResult{unchanged: ok}
			}
			return playlistPresenceResult{unchanged: true, snapshot: &audiorelease.VerifiedPresence{RequestID: id, RootID: entry.RootID, Track: entry.Track}}
		}
		return playlistPresenceResult{unchanged: audiorelease.PresenceUnchanged(index.engine, id)}
	}
	if index.presence != nil {
		return index.presence.verify(id, compute)
	}
	ok, checked := index.verified[id]
	if !checked {
		result := compute()
		ok = result.unchanged
		if result.snapshot != nil {
			index.pendingSnapshots[id] = *result.snapshot
		}
		index.verified[id] = ok
	}
	return ok
}

func (index *playlistLibraryIndex) commitPresenceSnapshots() {
	if index == nil || index.engine == nil {
		return
	}
	items := []audiorelease.VerifiedPresence{}
	if index.presence != nil {
		items = index.presence.takePending()
	} else {
		for _, item := range index.pendingSnapshots {
			items = append(items, item)
		}
		index.pendingSnapshots = map[string]audiorelease.VerifiedPresence{}
	}
	if len(items) > 0 {
		_ = audiorelease.StampVerifiedPresences(index.engine, items)
	}
}

// Only an explicit total in a local file is evidence that a folder has gaps.
func (index playlistLibraryIndex) hasAlbumGap(artist, album string) bool {
	key := [2]string{libraryPathKey(artist), libraryPathKey(album)}
	build := func() bool {
		ids := index.albums[key]
		for _, id := range ids {
			path, found, err := audiorelease.PathFor(index.engine, id)
			if err != nil || !found || !strings.EqualFold(filepath.Ext(path), ".flac") {
				continue
			}
			tags := index.recording(path)
			if tags.trackTotal > len(ids) {
				return true
			}
		}
		return false
	}
	if index.metadata != nil {
		return index.metadata.albumGapFor(key, build)
	}
	if gap, checked := index.albumGaps[key]; checked {
		return gap
	}
	gap := build()
	index.albumGaps[key] = gap
	return gap
}

// enqueuePlaylistTracks starts the work for the selected rows.
func (a *App) enqueuePlaylistTracks(ctx context.Context, playlistJobID string, rows []playlistTrack, indices []int, opts playlistEnqueueOptions) (playlistEnqueueReport, error) {
	report := playlistEnqueueReport{}
	library := a.playlistLibrary()
	selectedRows := make([]playlistTrack, len(indices))
	for position, index := range indices {
		if index < 0 || index >= len(rows) {
			return report, fmt.Errorf("曲目序号超出范围：%d", index)
		}
		selectedRows[position] = rows[index]
	}
	selectedPresence := playlistPresenceForIndex(library, selectedRows)
	present := make(map[int]bool, len(indices))
	for position, index := range indices {
		present[index] = selectedPresence[position]
	}
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
	seenTracks := map[string]bool{}
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
		trackKey := normalizeKey(artist) + "\x00" + normalizeKey(normalizePlaylistAlbumTitle(row.Album)) + "\x00" + normalizeKey(title)
		if row.NeteaseSongID > 0 {
			trackKey = "netease:" + strconv.FormatInt(row.NeteaseSongID, 10)
		}
		if seenTracks[trackKey] {
			report.Skipped = append(report.Skipped, artist+" - "+title+"（歌单内重复）")
			continue
		}
		seenTracks[trackKey] = true
		report.Tracks++
		album := normalizePlaylistAlbumTitle(row.Album)
		albumCompletion := opts.AlbumCompletion && album != "" && !isHotSlowCompilationAlbum(album)
		if present[index] && (!albumCompletion || !library.hasAlbumGap(artist, album)) {
			report.Skipped = append(report.Skipped, artist+" - "+title)
			continue
		}
		if albumCompletion && album != "" {
			key := playlistAlbumGroupKey(row, album)
			if seenAlbums[key] {
				continue
			}
			jobID, err := a.startPlaylistAlbumJob(ctx, client, playlistJobID, sites, budget, row, albumCompletion, opts.NeteaseOnly)
			if err != nil {
				report.Unmatched = append(report.Unmatched, artist+" - "+album+": "+err.Error())
				continue
			}
			report.Created = append(report.Created, jobID)
			seenAlbums[key] = true
			report.Albums++
			continue
		}
		jobID, err := a.startPlaylistAlbumJob(ctx, client, playlistJobID, sites, budget, row, albumCompletion, opts.NeteaseOnly)
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
	if isHotSlowCompilationAlbum(album) {
		return "compilation:" + normalizeKey(album)
	}
	if row.NeteaseAlbumID > 0 {
		return "netease-album:" + strconv.FormatInt(row.NeteaseAlbumID, 10)
	}
	return "artist-album:" + normalizeKey(row.Artist) + "\x00" + normalizeKey(album)
}

func playlistJobReusable(stage string) bool {
	switch stage {
	case "completed", "failed", "cancelled", "skipped", "needs_review":
		return false
	default:
		return true
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
		if existing.Stage == "needs_review" && !existing.CancelRequested {
			// Re-adding a playlist must supersede the old review, not leave the
			// same track in both the old review queue and a fresh replacement
			// chain. Cancellation is durable and the scheduler gate keeps any
			// already-claimed legacy task out of the new workflow.
			_, _ = a.flows.Control("supersede-"+baseJobID+"-"+strconv.FormatUint(existing.Revision, 10), baseJobID, "cancel", a.now().UnixNano())
		}
		jobID, err = a.replacementPlaylistJobID(baseJobID, existing.Revision)
		if err != nil {
			return "", err
		}
	}
	identity := a.playlistIdentity(ctx, client, jobID, artist, album, title)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	expectedDuration := 0
	if seconds, parseErr := strconv.ParseFloat(strings.TrimSpace(row.Duration), 64); parseErr == nil && seconds > 0 && seconds <= 24*3600 {
		expectedDuration = int(seconds + 0.5)
	}
	plan := SearchPlan{
		RootJobID: jobID, Scene: "expand",
		Query: strings.TrimSpace(artist + " " + firstNonEmpty(title, expected)), ExpectedTitle: expected, ExpectedArtist: artist,
		EnabledSites: sites, StreamingEnabled: true, Identity: identity, Budget: budget, PlaylistTrack: title, ExpectedDuration: expectedDuration,
		NeteaseOnly: neteaseOnly, NeteaseTrackOnly: album == "" || isHotSlowCompilationAlbum(album) || neteaseOnly && !albumCompletion, NeteaseSongID: row.NeteaseSongID, NeteaseAlbumID: row.NeteaseAlbumID,
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
func (a *App) playlistIdentity(ctx context.Context, client *musicbrainz.Client, jobID, artist, album, title string) Identity {
	if artist == "" || album == "" {
		return Identity{}
	}
	if client != nil {
		if lookup, err := identifyCatalogLookup(ctx, client, a.engine, catalogIdentifyRequest{
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
	type rowView struct {
		Index   int    `json:"index"`
		Artist  string `json:"artist"`
		Title   string `json:"title"`
		Album   string `json:"album,omitempty"`
		Present bool   `json:"present"`
	}
	out := make([]rowView, 0, len(rows))
	present := a.playlistPresence(rows)
	for i, row := range rows {
		out = append(out, rowView{Index: i, Artist: row.Artist, Title: row.Title, Album: row.Album, Present: present[i]})
	}
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
