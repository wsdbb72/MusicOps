package app

import (
	"context"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/downloadpolicy"
)

// recoverNeteaseByTitleDuration is the last narrow match for soundtrack or
// compilation uploads where another provider credits the album as the artist.
// The recording must still have the same normalized title and version words,
// plus a duration within five seconds of the durable NetEase row.
func (a *App) recoverNeteaseByTitleDuration(ctx context.Context, plan SearchPlan, anchor *neteaseSong) (string, string, string) {
	if anchor == nil || anchor.DurationMS <= 0 || strings.TrimSpace(anchor.Name) == "" {
		return "", "备用流媒体无法按标题和时长恢复：缺少网易云时长", downloadpolicy.ClassNoResult
	}
	notes := []string{}
	class := downloadpolicy.ClassNoResult
	for _, source := range []string{"qqmusic", "kugou", "kuwo"} {
		service, ok := streamServiceName(source)
		if !ok {
			continue
		}
		base := streamingEngineOverride()
		token := ""
		if base == "" {
			managedBase, managedToken, err := a.ensureEngine(engineStreaming)
			if err != nil {
				notes = append(notes, source+" 内置引擎不可用")
				class = preferStreamFailure(class, downloadpolicy.ClassNetwork)
				continue
			}
			base, token = managedBase, managedToken
		}
		endpoint, err := url.Parse(base + "/search")
		if err != nil {
			continue
		}
		values := endpoint.Query()
		values.Set("q", anchor.Name)
		values.Set("sources", service)
		values.Set("type", "song")
		endpoint.RawQuery = values.Encode()
		body, stepClass, err := a.engineGET(ctx, base, endpoint.String(), token)
		if err != nil {
			notes = append(notes, source+" 检索失败："+err.Error())
			class = preferStreamFailure(class, stepClass)
			continue
		}
		cards := parseEngineCards(body)
		type candidate struct {
			song  streamSong
			delta int64
		}
		matches := make([]candidate, 0, len(cards))
		for _, card := range cards {
			if !neteaseAlternateTitleMatches(anchor.Name, card.Name) {
				continue
			}
			delta := int64(1 << 62)
			if card.Duration > 0 {
				delta = int64(card.Duration)*1000 - int64(anchor.DurationMS)
				if delta < 0 {
					delta = -delta
				}
				if delta > neteaseAlternateDurationTolerance {
					continue
				}
			}
			if card.Source == "" {
				card.Source = service
			}
			matches = append(matches, candidate{song: card, delta: delta})
		}
		sort.SliceStable(matches, func(i, j int) bool { return matches[i].delta < matches[j].delta })
		for _, match := range matches {
			recoveryPlan := plan
			recoveryPlan.ExpectedTitle = anchor.Name
			recoveryPlan.ExpectedArtist = match.song.Artist
			recoveryPlan.PlaylistTrack = ""
			recoveryPlan.Query = anchor.Name
			known := match.song
			path, note, _ := a.tryConfiguredStreamSong(ctx, source, recoveryPlan, &known)
			if path == "" {
				notes = append(notes, source+" / "+match.song.Artist+"："+note)
				continue
			}
			if err := a.finalizeNeteaseAlternate(ctx, path, plan, nil, anchor, anchor.DurationMS); err != nil {
				_ = os.Remove(path)
				notes = append(notes, source+" / "+match.song.Artist+"："+err.Error())
				continue
			}
			return path, "网易云原曲无可用音频，已按标题和时长从 " + source + " 取得安全匹配，并恢复网易云专辑、歌手和封面；" + note, downloadpolicy.ClassDownload
		}
	}
	if len(notes) == 0 {
		notes = append(notes, "没有标题与时长同时匹配的备用流媒体")
	}
	if len(notes) > 6 {
		notes = notes[:6]
	}
	return "", "备用流媒体标题/时长匹配失败：" + strings.Join(notes, "；"), class
}
