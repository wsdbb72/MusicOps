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
func (a *App) scrapePlaylistIncoming(ctx context.Context, incoming, name, artist, albumArtist, album, fallbackTitle string, track int) (string, string, *playlistScrapeResult, error) {
	key := "scraped-" + name
	src, dest := filepath.Join(incoming, name), filepath.Join(incoming, key)
	hotSlowTags := isHotSlowCompilationAlbum(album)
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
		if !imageBytes(cover) {
			cover = nil
		}
		result := &playlistScrapeResult{Artwork: "retained"}
		if candidate == src {
			updates := tags.Raw
			updatesByField := map[string][]string{"TITLE": {title}, "ALBUM": {album}, "ARTIST": {artist}, "TRACKNUMBER": {strconv.Itoa(track)}}
			if strings.TrimSpace(albumArtist) != "" {
				updatesByField["ALBUMARTIST"] = []string{albumArtist}
			}
			for field, values := range updatesByField {
				if len(updates[field]) == 0 {
					updates[field] = values
				}
			}
			if hotSlowTags {
				// The large compilation owns its album identity, while ARTIST
				// stays track-specific and the per-track cover remains intact.
				updates["ALBUM"] = []string{album}
				if strings.TrimSpace(albumArtist) != "" {
					updates["ALBUMARTIST"] = []string{albumArtist}
				}
			}
			if lyrics == "" || len(cover) == 0 {
				request, cancel := context.WithTimeout(ctx, 20*time.Second)
				fetchedLyrics, fetchedCover, fetchErr := a.fetchArtworkContext(request, "", "", artist, album, title)
				cancel()
				if ctx.Err() != nil {
					return "", "", nil, ctx.Err()
				}
				if hotSlowTags {
					// The source cover came from the selected NetEase song. A
					// provider lookup may map the compilation title to an unrelated
					// artist album and must not replace it.
					fetchedCover = nil
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
			currentCover := flactags.FrontCover(body)
			switch {
			case len(cover) > 0 && !imageBytes(currentCover):
				body, err = flactags.EmbedFrontCover(body, cover)
			case len(currentCover) > 0 && !imageBytes(currentCover):
				body, err = flactags.RemoveFrontCover(body)
			}
			if err != nil {
				return "", "", nil, err
			}
			release, err := reserveAudioCache(incoming, int64(len(body)))
			if err != nil {
				return "", "", nil, err
			}
			defer release()
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
			if err := renameFileWithRetry(temp.Name(), dest); err != nil {
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
