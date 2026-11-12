package app

// Extra metadata sources. MusicBrainz does not cover every release — a K-pop
// single often has no entry — while the streaming catalogues know it. This
// endpoint asks the built-in NetEase client and the bundled go-music-dl for
// candidates so a person can see the real album/title/artist. Nothing here
// confirms an identity: the result is labelled as a candidate and never feeds
// the tag writer on its own.

import (
	"net/http"
	"sort"
	"strings"
)

// sourceCandidate is one hit from one source.
type sourceCandidate struct {
	Source   string `json:"source"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Album    string `json:"album,omitempty"`
	Duration int    `json:"duration,omitempty"`
	Exact    bool   `json:"exact"`
}

// lookupSources is POST /v1/sources/lookup.
func (a *App) lookupSources(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query   string   `json:"query"`
		Title   string   `json:"title"`
		Artist  string   `json:"artist"`
		Album   string   `json:"album"`
		Sources []string `json:"sources"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	title := strings.TrimSpace(body.Title)
	artist := strings.TrimSpace(body.Artist)
	query := strings.TrimSpace(body.Query)
	if query == "" {
		query = strings.TrimSpace(artist + " " + title)
	}
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "至少给一个曲名或检索词"})
		return
	}
	wanted := map[string]bool{}
	for _, name := range body.Sources {
		wanted[strings.ToLower(strings.TrimSpace(name))] = true
	}
	want := func(name string) bool { return len(wanted) == 0 || wanted[name] }

	candidates := []sourceCandidate{}
	notes := []string{}
	plan := SearchPlan{Query: query, ExpectedTitle: title, ExpectedArtist: artist}

	if want("netease") {
		if song, err := a.neteaseSearchSong(r.Context(), plan); err == nil && song != nil {
			candidates = append(candidates, sourceCandidate{
				Source: "netease", Title: song.Name, Artist: strings.Join(song.Artists, " / "), Album: song.Album,
				Duration: song.DurationMS / 1000, Exact: true,
			})
		} else if err != nil {
			notes = append(notes, "netease："+err.Error())
		}
	}
	if want("qq") || want("kugou") || want("kuwo") || want("migu") {
		base, token, startErr := a.ensureEngine(engineStreaming)
		if startErr != nil {
			notes = append(notes, "内置引擎不可用："+startErr.Error())
		} else {
			for _, service := range []string{"qq", "kugou", "kuwo"} {
				if !want(service) {
					continue
				}
				song, _, err := a.streamSearch(r.Context(), base, service, plan, token)
				if err != nil || song == nil {
					continue
				}
				exact := foldTagText(song.Name) == foldTagText(title) && (artist == "" || strings.Contains(foldTagText(song.Artist), foldTagText(artist)))
				candidates = append(candidates, sourceCandidate{
					Source: service, Title: song.Name, Artist: song.Artist, Album: song.Album,
					Duration: song.Duration, Exact: exact,
				})
			}
		}
	}
	// Exact hits first, then by source name so the list is stable.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Exact != candidates[j].Exact {
			return candidates[i].Exact
		}
		return candidates[i].Source < candidates[j].Source
	})
	if candidates == nil {
		candidates = []sourceCandidate{}
	}
	note := "这些是候选信息，不是已确认的发行身份；要写标签仍然走补齐流程。"
	if len(notes) > 0 {
		note += " " + strings.Join(notes, "；")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "candidates": candidates, "count": len(candidates), "note": note})
}
