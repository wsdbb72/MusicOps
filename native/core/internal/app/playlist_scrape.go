package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
	"github.com/wsdbb72/MusicOps/native/core/internal/lrc"
)

type playlistScrapeResult struct {
	Artwork     string `json:"artwork"`
	Lyrics      bool   `json:"lyrics"`
	TimedLyrics bool   `json:"timed_lyrics"`
	Cover       bool   `json:"cover"`
	lyrics      string
}

// Work on a separate staging file, never on the qBittorrent payload or an
// existing library file. Keep an identical staged file on replay.
func (a *App) scrapePlaylistIncoming(ctx context.Context, incoming, name, artist, album, fallbackTitle string, track int) (string, string, *playlistScrapeResult, error) {
	key := "scraped-" + name
	src, dest := filepath.Join(incoming, name), filepath.Join(incoming, key)
	for _, candidate := range []string{dest, src} {
		body, err := os.ReadFile(candidate)
		if candidate == dest && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", "", nil, err
		}
		tags, err := flactags.ReadBytes(body)
		if err != nil {
			return "", "", nil, err
		}
		title := truncateName(firstNonEmpty(tags.Title, fallbackTitle))
		lyrics := firstValue(tags.Raw["LYRICS"])
		cover := flactags.FrontCover(body)
		result := &playlistScrapeResult{Artwork: "retained"}
		if candidate == src {
			updates := tags.Raw
			for field, values := range map[string][]string{"TITLE": {title}, "ALBUM": {album}, "ARTIST": {artist}, "TRACKNUMBER": {strconv.Itoa(track)}} {
				if len(updates[field]) == 0 {
					updates[field] = values
				}
			}
			if lyrics == "" || len(cover) == 0 {
				request, cancel := context.WithTimeout(ctx, 20*time.Second)
				fetchedLyrics, fetchedCover, fetchErr := a.fetchArtworkContext(request, "", "", artist, album, title)
				cancel()
				if ctx.Err() != nil {
					return "", "", nil, ctx.Err()
				}
				switch {
				case fetchErr != nil:
					result.Artwork = "source_unavailable"
				case fetchedLyrics == "" && len(fetchedCover) == 0:
					result.Artwork = "not_found"
				default:
					result.Artwork = "fetched"
				}
				if lyrics == "" && fetchedLyrics != "" {
					lyrics = fetchedLyrics
					updates["LYRICS"] = []string{lyrics}
				}
				if len(cover) == 0 {
					cover = fetchedCover
				}
			}
			tags.Raw = updates
			body, err = flactags.WriteBytes(body, flactags.Tags{Raw: updates})
			if err != nil {
				return "", "", nil, err
			}
			if len(cover) > 0 && len(flactags.FrontCover(body)) == 0 {
				body, err = flactags.EmbedFrontCover(body, cover)
				if err != nil {
					return "", "", nil, err
				}
			}
			temp, err := os.CreateTemp(incoming, ".scraped-*.tmp")
			if err != nil {
				return "", "", nil, err
			}
			defer os.Remove(temp.Name())
			_, err = temp.Write(body)
			closeErr := temp.Close()
			if err != nil {
				return "", "", nil, err
			}
			if closeErr != nil {
				return "", "", nil, closeErr
			}
			if err := os.Rename(temp.Name(), dest); err != nil {
				return "", "", nil, err
			}
		}
		result.Lyrics, result.Cover = lyrics != "", len(cover) > 0
		if parsed, err := lrc.Parse(lyrics); err == nil {
			result.TimedLyrics = true
			result.lyrics = lrc.Format(parsed.ShiftedLines())
		}
		return key, title, result, nil
	}
	return "", "", nil, os.ErrNotExist
}

func firstValue(values []string) string {
	return strings.TrimSpace(strings.Join(values, "\n"))
}
