package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type discogsHit struct {
	Title             string `json:"title"`
	ID                int    `json:"id"`
	TrackCount        int    `json:"track_count,omitempty"`
	IdentityConfirmed bool   `json:"identity_confirmed"`
}

// attachDiscogs queries Discogs when a token is saved. A hit is a candidate
// only: IdentityConfirmed stays false, and MusicBrainz remains the release check.
func (a *App) attachDiscogs(resp *catalogIdentifyResponse, req catalogIdentifyRequest) {
	saved, _ := a.discogsTokenSaved()
	if !saved {
		return
	}
	base := "https://api.discogs.com"
	if strings.TrimSpace(req.DiscogsBaseURL) != "" {
		if !loopbackURL(req.DiscogsBaseURL) {
			return
		}
		base = strings.TrimRight(req.DiscogsBaseURL, "/")
	}
	hits, reason, ok := a.discogsSearch(base, req.Artist, req.Album)
	for i := range resp.Sources {
		if resp.Sources[i].Source != "discogs" {
			continue
		}
		resp.Sources[i].Available = ok
		resp.Sources[i].Reason = reason
	}
	resp.Discogs = hits
}

func (a *App) discogsSearch(base, artist, album string) ([]discogsHit, string, bool) {
	token, found, err := a.vault.Get(discogsTokenRef, "api.discogs.com")
	if err != nil || !found || token == "" {
		return nil, "Discogs Token 没有读到", false
	}
	endpoint, err := url.Parse(base + "/database/search")
	if err != nil {
		return nil, "Discogs 地址无效", false
	}
	values := endpoint.Query()
	values.Set("type", "release")
	values.Set("q", strings.TrimSpace(artist+" "+album))
	endpoint.RawQuery = values.Encode()
	request, err := http.NewRequest(http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, "Discogs 地址无效", false
	}
	request.Header.Set("Authorization", "Discogs token="+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "MusicOps/catalog")
	client := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil, "Discogs 没有连上", false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "Discogs 拒绝了查询", false
	}
	var decoded struct {
		Results []struct {
			Title string `json:"title"`
			ID    int    `json:"id"`
		} `json:"results"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&decoded) != nil {
		return nil, "Discogs 响应无法解析", false
	}
	hits := make([]discogsHit, 0, len(decoded.Results))
	for _, result := range decoded.Results {
		if strings.TrimSpace(result.Title) == "" {
			continue
		}
		hit := discogsHit{Title: result.Title, ID: result.ID, IdentityConfirmed: false}
		if loopbackURL(base) {
			hit.TrackCount = a.discogsTrackCount(base, result.ID, token)
		}
		hits = append(hits, hit)
		if len(hits) == 8 {
			break
		}
	}
	return hits, "Discogs 候选不能单独确认发行身份", true
}

func (a *App) discogsTrackCount(base string, id int, token string) int {
	if id <= 0 || !loopbackURL(base) {
		return 0
	}
	endpoint := strings.TrimRight(base, "/") + "/releases/" + strconv.Itoa(id)
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return 0
	}
	request.Header.Set("Authorization", "Discogs token="+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "MusicOps/catalog")
	client := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0
	}
	var decoded struct {
		Tracklist []struct {
			Title string `json:"title"`
		} `json:"tracklist"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&decoded) != nil {
		return 0
	}
	count := 0
	for _, track := range decoded.Tracklist {
		if strings.TrimSpace(track.Title) != "" {
			count++
		}
	}
	return count
}
