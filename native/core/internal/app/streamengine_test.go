package app

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
)

func oneSecondFLAC(t *testing.T, artist, title string) []byte {
	t.Helper()
	info := make([]byte, 34)
	binary.BigEndian.PutUint16(info[0:2], 16)
	binary.BigEndian.PutUint16(info[2:4], 16)
	packed := uint64(44100)<<44 | uint64(1)<<41 | uint64(15)<<36 | 44100
	binary.BigEndian.PutUint64(info[10:18], packed)
	raw := syntheticFLAC(t, false)
	copy(raw[8:42], info)
	raw = append(raw, make([]byte, 1200)...)
	out, err := flactags.WriteBytes(raw, flactags.Tags{Artist: artist, Title: title, Album: "First Album"})
	if err != nil {
		t.Fatal(err)
	}
	if err := validStreamAudio(out, artist, title, 1); err != nil {
		t.Fatal(err, len(out))
	}
	return out
}

func TestStreamEngineAcceptsOnlyValidatedAudio(t *testing.T) {
	audio := oneSecondFLAC(t, "Artist", "Example & Test")
	var tidalCalls, appleCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			if r.URL.Query().Get("sources") == "tidal" {
				tidalCalls++
				_, _ = w.Write([]byte(`{"songs":[{"id":"t1","source":"tidal","name":"Example & Test","artist":"Artist","album":"First Album","duration":1}]}`))
				return
			}
			http.NotFound(w, r)
		case "/download":
			if r.URL.Query().Get("source") != "tidal" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(audio)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	tracker := emptyResultsServer(t)
	defer tracker.Close()
	application := newApp(t)
	t.Setenv("MUSICOPS_ENGINE_URL", server.URL)
	post(t, application, "/v1/settings/download", map[string]any{"streaming_enabled": true})
	post(t, application, "/v1/search", searchBody("album-engine", tracker.URL, []string{"dicmusic", "orpheus"}, true))
	job := waitJob(t, application, "process:search:album-engine", "needs_review")
	if !strings.Contains(job.Message, "tidal 音频已通过校验并保存") || !strings.Contains(job.Message, "算作下载") || !strings.Contains(job.Message, "尚未入库") || strings.Contains(job.Message, "已入库") {
		t.Fatal(job.Message)
	}
	saved, err := filepath.Glob(filepath.Join(application.Dir, "incoming", "stream-album-engine-*-tidal.flac"))
	if err != nil || len(saved) != 1 || tidalCalls == 0 || appleCalls != 0 {
		t.Fatal(err, tidalCalls, appleCalls)
	}
	// The address field is retired: whatever the page sends is dropped and the
	// embedded engine is used, so an old (or hostile) URL cannot be stored.
	dropped := post(t, application, "/v1/settings/download", map[string]any{"streaming_enabled": true, "engine_url": "https://169.254.169.254/"})
	if dropped["engine_url"] != "" {
		t.Fatal(dropped)
	}
}

func TestStreamEngineWritesSlashSeparatedCreditsAsDiscreteArtists(t *testing.T) {
	const credits = "雪霏岚岚 / 早木旋子 / 张小她 / 倒悬的橘子 / 双笙（陈元汐） / 宁采臣丶在唱歌 / A路人 / 漆柚 / YUKIri / warma / LKs / 洛天依Official / 乐正绫 / LS"
	const title = "【bilibili音乐】一话一世界 《一花依世界》方言合唱版"
	audio := oneSecondFLAC(t, credits, title)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download" {
			_, _ = w.Write(audio)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	application := newApp(t)
	workBase := ""
	if _, statErr := os.Stat("E:\\"); statErr == nil {
		workBase = "E:\\"
	}
	work, err := os.MkdirTemp(workBase, "musicops-stream-slash-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(work) })
	if _, err := application.saveLibrarySettings(librarySettings{WorkDir: work}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSICOPS_ENGINE_URL", server.URL)
	plan := SearchPlan{RootJobID: "slash-credit-root", ExpectedTitle: "一话一世界", ExpectedArtist: credits, PlaylistTrack: title, ExpectedDuration: 1}
	song := &streamSong{ID: "slash-credit", Source: "joox", Artist: credits, Name: title, Album: "一话一世界", Duration: 1}
	path, note, _ := application.tryConfiguredStreamSong(t.Context(), "joox", plan, song)
	if path == "" {
		t.Fatal(note)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	tags, err := flactags.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"雪霏岚岚", "早木旋子", "张小她", "倒悬的橘子", "双笙（陈元汐）", "宁采臣丶在唱歌", "A路人", "漆柚", "YUKIri", "warma", "LKs", "洛天依Official", "乐正绫", "LS"}
	if len(tags.Artists) != len(want) || strings.Join(tags.Artists, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("artists=%q want discrete %q", tags.Artists, want)
	}
}
func TestStreamEngineSkipsAudioThatFailsValidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search" && r.URL.Query().Get("sources") == "tidal" {
			_, _ = w.Write([]byte(`{"songs":[{"id":"bad","source":"tidal","name":"Example & Test","artist":"Artist","duration":1}]}`))
			return
		}
		if r.URL.Path == "/download" {
			_, _ = w.Write(bytesRepeat(2000, 'x'))
			return
		}
		_, _ = w.Write([]byte(`{"songs":[]}`))
	}))
	defer server.Close()
	tracker := emptyResultsServer(t)
	defer tracker.Close()
	application := newApp(t)
	t.Setenv("MUSICOPS_ENGINE_URL", server.URL)
	post(t, application, "/v1/settings/download", map[string]any{"streaming_enabled": true})
	post(t, application, "/v1/search", searchBody("album-bad-audio", tracker.URL, []string{"dicmusic", "orpheus"}, true))
	job := waitJob(t, application, "process:search:album-bad-audio", "skipped")
	if !strings.Contains(job.Message, "没有通过音频校验") || !strings.Contains(job.Message, "尚未下载") {
		t.Fatal(job.Message)
	}
	if matches, _ := filepath.Glob(filepath.Join(application.Dir, "incoming", "stream-*.flac")); len(matches) != 0 {
		t.Fatal(matches)
	}
}

func TestStreamEngineContinuesAfterTidalMisses(t *testing.T) {
	audio := oneSecondFLAC(t, "Artist", "Example & Test")
	var sources []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download" {
			if r.URL.Query().Get("source") != "apple" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(audio)
			return
		}
		sources = append(sources, r.URL.Query().Get("sources"))
		if r.URL.Query().Get("sources") == "apple" {
			_, _ = w.Write([]byte(`{"songs":[{"id":"a1","source":"apple","name":"Example & Test","artist":"Artist","album":"First Album","duration":1}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"songs":[]}`))
	}))
	defer server.Close()
	tracker := emptyResultsServer(t)
	defer tracker.Close()
	application := newApp(t)
	t.Setenv("MUSICOPS_ENGINE_URL", server.URL)
	post(t, application, "/v1/settings/download", map[string]any{"streaming_enabled": true})
	post(t, application, "/v1/search", searchBody("album-apple", tracker.URL, []string{"dicmusic", "orpheus"}, true))
	job := waitJob(t, application, "process:search:album-apple", "needs_review")
	if !strings.Contains(job.Message, "applemusic 音频已通过校验并保存") || strings.Contains(job.Message, "tidal 音频已通过校验") {
		t.Fatal(job.Message, sources)
	}
}

func bytesRepeat(n int, b byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestStreamEngineScrapesEachTrackInAnAlbumIndependently(t *testing.T) {
	first := oneSecondFLAC(t, "Artist", "First")
	second := oneSecondFLAC(t, "Artist", "Second")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("id") {
		case "1":
			_, _ = w.Write(first)
		case "2":
			_, _ = w.Write(second)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("MUSICOPS_ENGINE_URL", server.URL)
	a := newApp(t)
	plan := SearchPlan{RootJobID: "same-album-root", ExpectedArtist: "Artist", ExpectedTitle: "First Album"}
	for i, song := range []*streamSong{
		{ID: "1", Source: "netease", Artist: "Artist", Name: "First", Album: "First Album", Duration: 1},
		{ID: "2", Source: "netease", Artist: "Artist", Name: "Second", Album: "First Album", Duration: 1},
	} {
		path, note, _ := a.tryConfiguredStreamSong(t.Context(), "netease", plan, song)
		if path == "" {
			t.Fatal(note)
		}
		_, title, _, err := a.scrapePlaylistIncoming(t.Context(), filepath.Dir(path), filepath.Base(path), song.Artist, "", song.Album, song.Name, i+1)
		if err != nil || title != song.Name {
			t.Fatalf("scraped %q for %q: %v", title, song.Name, err)
		}
	}
}

func TestStreamSearchMatchesVerifiedFallbackVariantsAndDuration(t *testing.T) {
	tests := []struct {
		name     string
		service  string
		response string
		plan     SearchPlan
		wantID   string
	}{
		{
			name: "joox exact studio recording in another album", service: "joox",
			response: `{"songs":[{"id":"joox-trysail","source":"joox","name":"Adrenaline!!!","artist":"TrySail","album":"Adrenaline!!!","duration":274}]}`,
			plan:     SearchPlan{ExpectedTitle: "adrenaline!!!", ExpectedArtist: "TrySail", PlaylistTrack: "adrenaline!!!", ExpectedDuration: 274},
			wantID:   "joox-trysail",
		},
		{
			name: "joox traditional title and two-second container drift", service: "joox",
			response: `{"songs":[{"id":"joox-barbie","source":"joox","name":"觸電","artist":"芭比","album":"搖頭舞曲3","duration":195}]}`,
			plan:     SearchPlan{ExpectedTitle: "摇头舞曲 3", ExpectedArtist: "芭比", PlaylistTrack: "触电 (完整版)", ExpectedDuration: 197},
			wantID:   "joox-barbie",
		},
		{
			name: "gelato bilingual extended title", service: "joox",
			response: `{"songs":[{"id":"joox-rabbit","source":"joox","name":"Penguin's Game (Extended Version)","artist":"Gelato","album":"Penguin's Game (Extended Version)","duration":239}]}`,
			plan:     SearchPlan{ExpectedTitle: "野狼王的士高", ExpectedArtist: "Gelato", PlaylistTrack: "兔子舞", ExpectedDuration: 241},
			wantID:   "joox-rabbit",
		},
		{
			name: "migu compact artist spacing", service: "migu",
			response: `{"songs":[{"id":"migu-home","source":"migu","name":"Home(Blaze U Remix Edit)","artist":"Thimlife / BibianeZ","album":"Home (Remixes)","duration":183}]}`,
			plan:     SearchPlan{ExpectedTitle: "Futuresque - The Future House Collection", ExpectedArtist: "ThimLife / Bibiane Z", PlaylistTrack: "Home (Blaze U Remix Edit)", ExpectedDuration: 183},
			wantID:   "migu-home",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.response))
			}))
			defer server.Close()
			song, _, err := (&App{}).streamSearch(t.Context(), server.URL, test.service, test.plan, "")
			if err != nil || song == nil || song.ID != test.wantID {
				t.Fatalf("song=%+v err=%v", song, err)
			}
		})
	}
}

func TestStreamSearchRejectsVersionOrDurationDrift(t *testing.T) {
	tests := []struct {
		name     string
		response string
		plan     SearchPlan
	}{
		{
			name: "live version", response: `{"songs":[{"id":"live","source":"joox","name":"adrenaline!!! (Live)","artist":"TrySail","album":"Live","duration":274}]}`,
			plan: SearchPlan{ExpectedTitle: "adrenaline!!!", ExpectedArtist: "TrySail", PlaylistTrack: "adrenaline!!!", ExpectedDuration: 274},
		},
		{
			name: "wrong duration", response: `{"songs":[{"id":"long","source":"joox","name":"爱河","artist":"DJ奶小深","album":"Remix","duration":340}]}`,
			plan: SearchPlan{ExpectedTitle: "Remix", ExpectedArtist: "DJ奶小深", PlaylistTrack: "爱河", ExpectedDuration: 326},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.response))
			}))
			defer server.Close()
			song, _, err := (&App{}).streamSearch(t.Context(), server.URL, "joox", test.plan, "")
			if err != nil || song != nil {
				t.Fatalf("unexpected match: song=%+v err=%v", song, err)
			}
		})
	}
}
func TestStreamSearchRanksClosestDurationCandidate(t *testing.T) {
	response := `{"songs":[{"id":"wrong","source":"joox","name":"爱河","artist":"DJ奶小深","album":"Remix","duration":340},{"id":"right","source":"joox","name":"爱河","artist":"DJ奶小深","album":"Remix","duration":326}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()
	plan := SearchPlan{ExpectedTitle: "Remix", ExpectedArtist: "DJ奶小深", PlaylistTrack: "爱河", ExpectedDuration: 326}
	song, _, err := (&App{}).streamSearch(t.Context(), server.URL, "joox", plan, "")
	if err != nil || song == nil || song.ID != "right" {
		t.Fatalf("song=%+v err=%v", song, err)
	}
}

func TestStreamEngineTriesNextCandidateWhenFirstDownloadIsInvalid(t *testing.T) {
	audio := oneSecondFLAC(t, "Artist", "Example & Test")
	downloads := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"songs":[{"id":"bad","source":"joox","name":"Example & Test","artist":"Artist","album":"First Album","duration":1},{"id":"good","source":"joox","name":"Example & Test","artist":"Artist","album":"First Album","duration":1}]}`))
		case "/download":
			id := r.URL.Query().Get("id")
			downloads = append(downloads, id)
			if id == "bad" {
				_, _ = w.Write([]byte("not audio"))
				return
			}
			_, _ = w.Write(audio)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	application := newApp(t)
	t.Setenv("MUSICOPS_ENGINE_URL", server.URL)
	plan := SearchPlan{RootJobID: "candidate-retry", ExpectedTitle: "Example & Test", ExpectedArtist: "Artist", ExpectedDuration: 1}
	path, note, class := application.tryConfiguredStreamSong(t.Context(), "joox", plan, nil)
	if path == "" || class != "download" || strings.Join(downloads, ",") != "bad,good" {
		t.Fatalf("path=%q note=%q class=%q downloads=%v", path, note, class, downloads)
	}
}

func testCoverPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 120, 120))
	for y := 0; y < 120; y++ {
		for x := 0; x < 120; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 96, A: 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestNeteaseTitleDurationRecoveryAcceptsJSONSearchContract(t *testing.T) {
	audio, err := flactags.EmbedFrontCover(oneSecondFLAC(t, "Artist", "Example & Test"), testCoverPNG(t))
	if err != nil {
		t.Fatal(err)
	}
	searches, downloads := 0, []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			searches++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"songs":[{"id":"wrong","source":"qq","name":"Example & Test","artist":"Artist","album":"Original Album","duration":340},{"id":"right","source":"qq","name":"Example & Test","artist":"Artist","album":"Original Album","duration":1}]}`))
		case "/download":
			downloads = append(downloads, r.URL.Query().Get("id"))
			_, _ = w.Write(audio)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	application := newApp(t)
	t.Setenv("MUSICOPS_ENGINE_URL", server.URL)
	plan := SearchPlan{RootJobID: "netease-json-recovery", ExpectedTitle: "Original Album", ExpectedArtist: "Artist", PlaylistTrack: "Example & Test", ExpectedDuration: 1}
	anchor := &neteaseSong{Name: "Example & Test", Artists: []string{"Artist"}, Album: "Original Album", DurationMS: 1000}
	path, note, class := application.recoverNeteaseByTitleDuration(t.Context(), plan, anchor)
	if path == "" || class != "download" || searches != 1 || strings.Join(downloads, ",") != "right" {
		t.Fatalf("path=%q note=%q class=%q searches=%d downloads=%v", path, note, class, searches, downloads)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := flactags.ReadBytes(body)
	if err != nil || tags.Title != "Example & Test" || tags.Album != "Original Album" {
		t.Fatalf("tags=%+v err=%v", tags, err)
	}
}

func TestNeteaseTitleDurationRecoveryQueriesEachSourceOnce(t *testing.T) {
	searches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			http.NotFound(w, r)
			return
		}
		searches++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"songs":[]}`))
	}))
	defer server.Close()
	application := newApp(t)
	t.Setenv("MUSICOPS_ENGINE_URL", server.URL)
	plan := SearchPlan{ExpectedTitle: "Album", ExpectedArtist: "Artist", PlaylistTrack: "Song", ExpectedDuration: 1}
	anchor := &neteaseSong{Name: "Song", Artists: []string{"Artist"}, DurationMS: 1000}
	path, _, _ := application.recoverNeteaseByTitleDuration(t.Context(), plan, anchor)
	if path != "" || searches != 3 {
		t.Fatalf("path=%q searches=%d", path, searches)
	}
}

func TestParseStreamEngineSongsAcceptsJSONAndHTML(t *testing.T) {
	jsonCards := parseStreamEngineSongs([]byte(`{"songs":[{"id":"j","source":"joox","name":"JSON","artist":"Artist","album":"Album","duration":90}]}`))
	htmlCards := parseStreamEngineSongs([]byte(`<li class="song-card" data-id="h" data-source="joox" data-name="HTML" data-artist="Artist" data-album="Album" data-duration="91"></li>`))
	if len(jsonCards) != 1 || jsonCards[0].ID != "j" || len(htmlCards) != 1 || htmlCards[0].ID != "h" {
		t.Fatalf("json=%+v html=%+v", jsonCards, htmlCards)
	}
}

func TestStreamEngineStopsCandidateWalkOnRateLimit(t *testing.T) {
	downloads := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"songs":[{"id":"first","source":"joox","name":"Example & Test","artist":"Artist","album":"First Album","duration":1},{"id":"second","source":"joox","name":"Example & Test","artist":"Artist","album":"First Album","duration":1}]}`))
		case "/download":
			downloads = append(downloads, r.URL.Query().Get("id"))
			http.Error(w, "limited", http.StatusTooManyRequests)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	application := newApp(t)
	t.Setenv("MUSICOPS_ENGINE_URL", server.URL)
	plan := SearchPlan{RootJobID: "candidate-rate-limit", ExpectedTitle: "Example & Test", ExpectedArtist: "Artist", ExpectedDuration: 1}
	path, _, class := application.tryConfiguredStreamSong(t.Context(), "joox", plan, nil)
	if path != "" || class != "rate_limited" || strings.Join(downloads, ",") != "first" {
		t.Fatalf("path=%q class=%q downloads=%v", path, class, downloads)
	}
}
