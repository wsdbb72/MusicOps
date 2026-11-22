package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/downloadpolicy"
	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
)

// go-music-dl is AGPL-3.0. This MIT program does not vendor its source or
// ship its binary. 0.4.31 already talked to a copy the user ran locally.
// The client below speaks that HTTP contract and only treats a response as
// a download after the bytes pass a FLAC check. musicdl is Python and is
// not used.

func allowedEngineURL(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.User != nil || parsed.Host == "" {
		return "", false
	}
	switch parsed.Scheme {
	case "http":
		if !loopbackURL(raw) {
			return "", false
		}
	case "https":
	default:
		return "", false
	}
	if metadataHost(parsed.Hostname()) {
		return "", false
	}
	return strings.TrimRight(parsed.String(), "/"), true
}

// metadataHost rejects link-local and cloud-metadata targets. A user-configured
// engine may be a public HTTPS host or a private LAN address; it must not be
// the cloud metadata address or another link-local target.
func metadataHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	switch host {
	case "metadata.google.internal", "metadata.google.com":
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

func streamServiceName(source string) (string, bool) {
	switch source {
	case "tidal":
		return "tidal", true
	case "applemusic":
		return "apple", true
	case "qqmusic":
		return "qq", true
	case "netease":
		return "netease", true
	default:
		return "", false
	}
}

// tryConfiguredStream asks the user's engine for one source. With no engine
// the note stays the historical "未接入" text. A file is returned only after
// validStreamAudio accepts it.
func (a *App) tryConfiguredStream(ctx context.Context, source string, plan SearchPlan) (string, string, string) {
	return a.tryConfiguredStreamSong(ctx, source, plan, nil)
}

func (a *App) tryConfiguredStreamSong(ctx context.Context, source string, plan SearchPlan, knownSong *streamSong) (string, string, string) {
	service, ok := streamServiceName(source)
	if !ok {
		return "", source + " 未接入（无凭证与引擎）", downloadpolicy.ClassNoResult
	}
	settings, err := a.loadDownloadSettings()
	if err != nil {
		return "", source + " 未接入（无凭证与引擎）", downloadpolicy.ClassNoResult
	}
	_ = settings // the address field is retired; see streamingEngineOverride
	base := streamingEngineOverride()
	token := ""
	if base == "" {
		// The installer ships go-music-dl; start it instead of asking the
		// user to run a service by hand.
		managedBase, managedToken, startErr := a.ensureEngine(engineStreaming)
		if startErr != nil {
			return "", source + " 未接入（内置引擎未能启动）", downloadpolicy.ClassNoResult
		}
		base, token = managedBase, managedToken
	}
	song := knownSong
	var status string
	if song == nil {
		song, status, err = a.streamSearch(ctx, base, service, plan, token)
	}
	if err != nil {
		if status == downloadpolicy.ClassAuth {
			return "", source + " 需要用户自己的账号，没有绕过访问控制", downloadpolicy.ClassAuth
		}
		if status == "" {
			status = downloadpolicy.ClassNoResult
		}
		return "", source + " 没有连上本机引擎", status
	}
	if song == nil {
		return "", source + " 未找到可通过校验的音频", downloadpolicy.ClassNoResult
	}
	body, status, err := a.streamDownload(ctx, base, song, token)
	if err != nil {
		if status == downloadpolicy.ClassAuth {
			return "", source + " 需要用户自己的账号，没有绕过访问控制", downloadpolicy.ClassAuth
		}
		if status == "" {
			status = downloadpolicy.ClassNoResult
		}
		return "", source + " 没有取到音频", status
	}
	// The bundled engine hands back mp3 for the lossy sources. The library is
	// FLAC-only, so a decodable lossy file is converted with the bundled
	// ffmpeg and kept as FLAC; the note says the source was lossy.
	lossySource := false
	if !isKnownAudio(body) {
		return "", source + " 返回的内容没有通过音频校验", downloadpolicy.ClassNoResult
	}
	if string(body[:4]) != "fLaC" {
		if plan.PlaylistTrack != "" && source != "netease" {
			return "", source + " 未提供可验证的无损音源", downloadpolicy.ClassNoResult
		}
		converted, convErr := a.transcodeEngineAudio(ctx, body, source, plan)
		if convErr != nil {
			return "", source + " 返回的是有损音频，转 FLAC 失败：" + convErr.Error(), downloadpolicy.ClassRateLimited
		}
		body = converted
		lossySource = true
	}
	if err := validStreamAudio(body, "", "", song.Duration); err != nil {
		return "", source + " 返回的内容没有通过音频校验", downloadpolicy.ClassNoResult
	}
	name := "stream-" + safeStreamID(plan.RootJobID) + "-" + source + ".flac"
	if strings.Contains(name, string(filepath.Separator)) || strings.Contains(name, "..") {
		return "", source + " 校验通过但没有写入", downloadpolicy.ClassNoResult
	}
	if err := os.MkdirAll(a.workPath("incoming"), 0o700); err != nil {
		return "", source + " 校验通过但没有写入：" + err.Error(), downloadpolicy.ClassRateLimited
	}
	dir, err := resolveInDir(a.workDir(), "incoming")
	if err != nil {
		return "", source + " 校验通过但没有写入：" + err.Error(), downloadpolicy.ClassNoResult
	}
	path := filepath.Join(dir, name)
	release, cacheErr := reserveAudioCache(dir, 2*int64(len(body))+65536)
	if cacheErr != nil {
		return "", cacheErr.Error(), downloadpolicy.ClassRateLimited
	}
	defer release()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", source + " 校验通过但没有写入：" + err.Error(), downloadpolicy.ClassRateLimited
	}
	// The identity the engine reported is written into the file: ffmpeg's own
	// output carries no tags, and the pipeline reads the album from them.
	tags := flactags.Tags{
		Title: song.Name, Artists: []string{song.Artist}, Album: song.Album, AlbumArtist: song.Artist,
	}
	if lossySource {
		tags.Raw = map[string][]string{"COMMENT": {"MusicOps source=" + source + "-lossy"}}
	}
	if err := flactags.Write(path, tags); err != nil {
		_ = os.Remove(path)
		return "", source + " 校验通过但标签写入失败：" + err.Error(), downloadpolicy.ClassRateLimited
	}
	note := source + " 音频已通过校验并保存，尚未入库"
	if lossySource {
		note = source + " 源为有损音频，已转成 FLAC 保存（不是无损来源），尚未入库"
	}
	return path, note, downloadpolicy.ClassDownload
}

// isKnownAudio recognises the containers the engine can hand back, so a
// garbage response is reported as invalid audio instead of being sent to
// ffmpeg.
func isKnownAudio(body []byte) bool {
	if len(body) < 12 {
		return false
	}
	switch {
	case string(body[:4]) == "fLaC", string(body[:4]) == "OggS", string(body[:4]) == "RIFF":
		return true
	case string(body[:3]) == "ID3":
		return true
	case body[0] == 0xff && body[1]&0xe0 == 0xe0:
		return true
	case string(body[4:8]) == "ftyp":
		return true
	}
	return false
}

// transcodeEngineAudio converts the engine's lossy download into FLAC with the
// bundled ffmpeg. The temporary input and output are reserved against the
// shared SSD cache limit before either file is created, and ffmpeg is bounded
// to the same maximum output size used by the rest of the download pipeline.
func (a *App) transcodeEngineAudio(ctx context.Context, body []byte, source string, plan SearchPlan) ([]byte, error) {
	ffmpeg := ""
	if tools, ok := a.bundledTools(); ok {
		ffmpeg = filepath.Join(tools, "ffmpeg.exe")
	} else if resolved, err := exec.LookPath("ffmpeg"); err == nil {
		ffmpeg = resolved
	}
	if ffmpeg == "" {
		return nil, errors.New("没有可用的 ffmpeg")
	}
	incoming := a.workPath("incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		return nil, err
	}
	// Reserve the actual input plus the hard maximum for FLAC output before
	// creating either temporary. The reservation stays held until both files
	// are gone, so concurrent transcodes cannot each pass the old usage check.
	// ffmpeg may finish the current frame after reaching -fs. Keep room for
	// the muxer and stop above the accepted size so a capped FLAC is rejected.
	const stopAt = audiorelease.MaxStagedAudioBytes + (1 << 20)
	release, err := reserveAudioCache(incoming, int64(len(body))+stopAt+(1<<20))
	if err != nil {
		return nil, err
	}
	defer release()
	dir := filepath.Join(incoming, "engine-transcode")
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	input, err := os.CreateTemp(dir, "engine-*.audio")
	if err != nil {
		return nil, err
	}
	defer os.Remove(input.Name())
	if _, err := input.Write(body); err != nil {
		input.Close()
		return nil, err
	}
	if err := input.Close(); err != nil {
		return nil, err
	}
	output := input.Name() + ".flac"
	defer os.Remove(output)
	cmd := exec.CommandContext(ctx, ffmpeg, "-y", "-v", "error", "-nostdin", "-i", input.Name(), "-map", "0:a:0", "-fs", strconv.Itoa(stopAt), "-c:a", "flac", output)
	prepareSolverCommand(cmd)
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	info, err := os.Stat(output)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > audiorelease.MaxStagedAudioBytes {
		return nil, errors.New("转 FLAC 输出超过暂存上限")
	}
	return os.ReadFile(output)
}

type streamSong struct {
	ID       string
	Source   string
	Name     string
	Artist   string
	Album    string
	Duration int
}

func engineIDField(raw any) string {
	switch value := raw.(type) {
	case string:
		return textField(value)
	case float64:
		if value >= 0 && math.Trunc(value) == value && value <= math.MaxInt64 {
			return strconv.FormatInt(int64(value), 10)
		}
	case json.Number:
		return value.String()
	}
	return ""
}
func (a *App) streamSearch(ctx context.Context, base, service string, plan SearchPlan, token string) (*streamSong, string, error) {
	query := strings.TrimSpace(plan.Query)
	if query == "" {
		query = strings.TrimSpace(plan.ExpectedArtist + " " + plan.ExpectedTitle)
	}
	endpoint, err := url.Parse(base + "/search")
	if err != nil {
		return nil, downloadpolicy.ClassNoResult, err
	}
	values := endpoint.Query()
	values.Set("q", query)
	values.Set("sources", service)
	values.Set("type", "song")
	endpoint.RawQuery = values.Encode()
	body, class, err := a.engineGET(ctx, base, endpoint.String(), token)
	if err != nil {
		return nil, class, err
	}
	// Two contracts exist in the wild: older bridges answer JSON, and the
	// bundled go-music-dl answers the rendered page (one <li class="song-card"
	// data-...> per hit). Accept both instead of forcing one.
	var cards []streamSong
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '{' {
		var decoded struct {
			Songs []map[string]any `json:"songs"`
		}
		if json.Unmarshal(trimmed, &decoded) == nil {
			for _, row := range decoded.Songs {
				cards = append(cards, streamSong{
					ID: engineIDField(row["id"]), Source: textField(row["source"]), Name: textField(row["name"]),
					Artist: textField(row["artist"]), Album: textField(row["album"]), Duration: intField(row["duration"]),
				})
			}
		}
	} else {
		cards = parseEngineCards(body)
	}
	if cards == nil {
		return nil, downloadpolicy.ClassNoResult, nil
	}
	wantTitle := foldTagText(plan.ExpectedTitle)
	wantAlbum := ""
	playlistTrack := neteasePlaylistTitle(plan)
	if playlistTrack != "" {
		wantTitle = foldTagText(playlistTrack)
		if normalizeKey(playlistTrack) != normalizeKey(plan.ExpectedTitle) {
			wantAlbum = normalizeKey(normalizePlaylistAlbumTitle(plan.ExpectedTitle))
		}
	}
	wantArtist := foldTagText(plan.ExpectedArtist)
	wantNeteaseID := ""
	if service == "netease" && plan.NeteaseSongID > 0 {
		wantNeteaseID = strconv.FormatInt(plan.NeteaseSongID, 10)
	}
	for i := range cards {
		song := cards[i]
		if song.Source == "" {
			song.Source = service
		}
		if !strings.EqualFold(strings.TrimSpace(song.Source), service) {
			continue
		}
		if wantNeteaseID != "" {
			// A durable NetEase recording id is stronger than a re-tagged album
			// or compilation label. Never accept a different id heuristically.
			if strings.TrimSpace(song.ID) == wantNeteaseID {
				return &song, "", nil
			}
			continue
		}
		if wantAlbum != "" && normalizeKey(normalizePlaylistAlbumTitle(song.Album)) != wantAlbum {
			continue
		}
		if playlistTrack != "" && !playlistArtistsMatch(strings.Split(plan.ExpectedArtist, "/"), strings.Split(song.Artist, "/")) {
			continue
		}
		if wantTitle != "" && foldTagText(song.Name) != wantTitle {
			continue
		}
		if wantArtist != "" && song.Artist != "" && !strings.Contains(foldTagText(song.Artist), wantArtist) {
			continue
		}
		return &song, "", nil
	}
	return nil, "", nil
}

// engineCard matches one song card on the engine's result page.
var engineCard = regexp.MustCompile(`(?s)<li class="song-card"(.*?)>`)

// engineCardAttr reads one data- attribute out of a card's attribute block.
func engineCardAttr(block, name string) string {
	match := regexp.MustCompile(`data-` + name + `="([^"]*)"`).FindStringSubmatch(block)
	if match == nil {
		return ""
	}
	return html.UnescapeString(strings.TrimSpace(match[1]))
}

// parseEngineCards reads every result card. A page without cards means "no
// result", which is not an error.
func parseEngineCards(body []byte) []streamSong {
	matches := engineCard.FindAllStringSubmatch(string(body), -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]streamSong, 0, len(matches))
	for _, match := range matches {
		block := match[1]
		id := engineCardAttr(block, "id")
		name := engineCardAttr(block, "name")
		if id == "" || name == "" {
			continue
		}
		seconds := 0
		if value, err := strconv.Atoi(engineCardAttr(block, "duration")); err == nil {
			seconds = value
		}
		out = append(out, streamSong{
			ID: id, Source: engineCardAttr(block, "source"), Name: name,
			Artist: engineCardAttr(block, "artist"), Album: engineCardAttr(block, "album"), Duration: seconds,
		})
	}
	return out
}

func (a *App) streamDownload(ctx context.Context, base string, song *streamSong, token string) ([]byte, string, error) {
	endpoint, err := url.Parse(base + "/download")
	if err != nil {
		return nil, downloadpolicy.ClassNoResult, err
	}
	values := endpoint.Query()
	values.Set("id", song.ID)
	values.Set("source", song.Source)
	values.Set("name", song.Name)
	values.Set("artist", song.Artist)
	values.Set("album", song.Album)
	endpoint.RawQuery = values.Encode()
	return a.engineGET(ctx, base, endpoint.String(), token)
}

func (a *App) engineGET(ctx context.Context, base, endpoint, token string) ([]byte, string, error) {
	finish := a.beginEngineRequest(base)
	defer finish()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, downloadpolicy.ClassNoResult, err
	}
	request.Header.Set("Accept", "application/json, audio/flac")
	request.Header.Set("User-Agent", "MusicOps")
	if token != "" {
		// The bundled go-music-dl refuses requests without this header.
		request.Header.Set(engineTokenHeader, token)
	}
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil, downloadpolicy.ClassNetwork, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, downloadpolicy.ClassAuth, errors.New("auth")
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return nil, downloadpolicy.ClassRateLimited, errors.New("rate limited")
	}
	if response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooEarly || response.StatusCode >= 500 {
		return nil, downloadpolicy.ClassNetwork, errors.New("temporary upstream failure")
	}
	if response.StatusCode != http.StatusOK {
		return nil, downloadpolicy.ClassNoResult, errors.New("status")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 80<<20))
	if err != nil {
		return nil, downloadpolicy.ClassNetwork, err
	}
	return body, "", nil
}

func validStreamAudio(data []byte, artist, title string, wantSeconds int) error {
	if len(data) < 1024 || !strings.HasPrefix(string(data[:4]), "fLaC") {
		return errors.New("不是可校验的 FLAC")
	}
	tags, err := flactags.ReadBytes(data)
	if err != nil {
		return err
	}
	if tags.StreamInfo.SampleRate == 0 || tags.StreamInfo.TotalSamples == 0 {
		return errors.New("没有时长")
	}
	if artist != "" && !strings.EqualFold(strings.TrimSpace(tags.Artist), artist) {
		return errors.New("艺人与文件标签不一致")
	}
	if title != "" && !strings.EqualFold(strings.TrimSpace(tags.Title), title) {
		return errors.New("曲名与文件标签不一致")
	}
	if wantSeconds > 0 {
		got := float64(tags.StreamInfo.TotalSamples) / float64(tags.StreamInfo.SampleRate)
		if math.Abs(got-float64(wantSeconds)) > 2 {
			return errors.New("时长超出 2 秒")
		}
	}
	return nil
}

func safeStreamID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "track"
	}
	if b.Len() > 40 {
		return b.String()[:40]
	}
	return b.String()
}
