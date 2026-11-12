package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	service, ok := streamServiceName(source)
	if !ok {
		return "", source + " 未接入（无凭证与引擎）", downloadpolicy.ClassNoResult
	}
	settings, err := a.loadDownloadSettings()
	if err != nil {
		return "", source + " 未接入（无凭证与引擎）", downloadpolicy.ClassNoResult
	}
	base, ok := allowedEngineURL(settings.EngineURL)
	if !ok {
		return "", source + " 未接入（无凭证与引擎）", downloadpolicy.ClassNoResult
	}
	song, status, err := a.streamSearch(ctx, base, service, plan)
	if err != nil {
		if status == downloadpolicy.ClassAuth {
			return "", source + " 需要用户自己的账号，没有绕过访问控制", downloadpolicy.ClassAuth
		}
		return "", source + " 没有连上本机引擎", downloadpolicy.ClassNoResult
	}
	if song == nil {
		return "", source + " 未找到可通过校验的音频", downloadpolicy.ClassNoResult
	}
	body, status, err := a.streamDownload(ctx, base, song)
	if err != nil {
		if status == downloadpolicy.ClassAuth {
			return "", source + " 需要用户自己的账号，没有绕过访问控制", downloadpolicy.ClassAuth
		}
		return "", source + " 没有取到音频", downloadpolicy.ClassNoResult
	}
	if err := validStreamAudio(body, song.Artist, song.Name, song.Duration); err != nil {
		return "", source + " 返回的内容没有通过音频校验", downloadpolicy.ClassNoResult
	}
	name := "stream-" + safeStreamID(plan.RootJobID) + "-" + source + ".flac"
	if strings.Contains(name, string(filepath.Separator)) || strings.Contains(name, "..") {
		return "", source + " 校验通过但没有写入", downloadpolicy.ClassNoResult
	}
	if err := os.MkdirAll(a.workPath("incoming"), 0o700); err != nil {
		return "", source + " 校验通过但没有写入", downloadpolicy.ClassNoResult
	}
	dir, err := resolveInDir(a.workDir(), "incoming")
	if err != nil {
		return "", source + " 校验通过但没有写入", downloadpolicy.ClassNoResult
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", source + " 校验通过但没有写入", downloadpolicy.ClassNoResult
	}
	return path, source + " 音频已通过校验并保存，尚未入库", downloadpolicy.ClassDownload
}

type streamSong struct {
	ID       string
	Source   string
	Name     string
	Artist   string
	Album    string
	Duration int
}

func (a *App) streamSearch(ctx context.Context, base, service string, plan SearchPlan) (*streamSong, string, error) {
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
	body, class, err := engineGET(ctx, endpoint.String())
	if err != nil {
		return nil, class, err
	}
	if len(body) == 0 || body[0] == '<' {
		return nil, downloadpolicy.ClassNoResult, nil
	}
	var decoded struct {
		Songs []map[string]any `json:"songs"`
	}
	if json.Unmarshal(body, &decoded) != nil {
		return nil, downloadpolicy.ClassNoResult, errors.New("引擎响应不是曲目 JSON")
	}
	for _, row := range decoded.Songs {
		song := streamSong{
			ID: textField(row["id"]), Source: textField(row["source"]), Name: textField(row["name"]),
			Artist: textField(row["artist"]), Album: textField(row["album"]), Duration: intField(row["duration"]),
		}
		if song.Name == "" {
			song.Name = textField(row["title"])
		}
		if song.Source == "" {
			song.Source = service
		}
		if song.ID == "" || song.Name == "" {
			continue
		}
		if plan.ExpectedTitle != "" && !strings.EqualFold(song.Name, plan.ExpectedTitle) {
			continue
		}
		if plan.ExpectedArtist != "" && song.Artist != "" && !strings.EqualFold(song.Artist, plan.ExpectedArtist) {
			continue
		}
		return &song, "", nil
	}
	return nil, "", nil
}

func (a *App) streamDownload(ctx context.Context, base string, song *streamSong) ([]byte, string, error) {
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
	return engineGET(ctx, endpoint.String())
}

func engineGET(ctx context.Context, endpoint string) ([]byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, downloadpolicy.ClassNoResult, err
	}
	request.Header.Set("Accept", "application/json, audio/flac")
	request.Header.Set("User-Agent", "MusicOps")
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil, downloadpolicy.ClassNoResult, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, downloadpolicy.ClassAuth, errors.New("auth")
	}
	if response.StatusCode != http.StatusOK {
		return nil, downloadpolicy.ClassNoResult, errors.New("status")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 80<<20))
	if err != nil {
		return nil, downloadpolicy.ClassNoResult, err
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
