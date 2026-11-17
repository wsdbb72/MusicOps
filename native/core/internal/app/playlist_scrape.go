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
	strictCompilationCover := isHotSlowCompilationAlbum(album)
	var canonicalCover []byte
	if strictCompilationCover {
		canonicalCover = a.hotSlowCompilationCover()
	}
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
		if strictCompilationCover {
			// Per-track source art belongs to the original single release and
			// must not leak into the unified compilation.
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
			if lyrics == "" || (!strictCompilationCover && len(cover) == 0) {
				request, cancel := context.WithTimeout(ctx, 20*time.Second)
				fetchedLyrics, fetchedCover, fetchErr := a.fetchArtworkContext(request, "", "", artist, album, title)
				cancel()
				if ctx.Err() != nil {
					return "", "", nil, ctx.Err()
				}
				if strictCompilationCover {
					// Ignore provider covers; this title can map to an unrelated
					// artist album even for an otherwise exact track match.
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
				if !strictCompilationCover && len(cover) == 0 {
					cover = fetchedCover
				}
			}
			if strictCompilationCover {
				cover = canonicalCover
			}
			tags.Raw = updates
			body, err = flactags.WriteBytes(body, flactags.Tags{Raw: updates})
			if err != nil {
				return "", "", nil, err
			}
			currentCover := flactags.FrontCover(body)
			switch {
			case len(cover) > 0:
				body, err = flactags.EmbedFrontCover(body, cover)
			case strictCompilationCover && len(currentCover) > 0:
				body, err = flactags.RemoveFrontCover(body)
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

// hotSlowCompilationCover returns the single approved album-level cover when
// it already exists in the library. The numbered source releases carry their
// own, often unrelated, artwork, so a missing canonical image intentionally
// means no embedded cover rather than a misleading one.
func (a *App) hotSlowCompilationCover() []byte {
	folder := filepath.Join(a.libraryDir(), publishComponent(hotSlowCompilationArtist), publishComponent(hotSlowCompilationAlbum))
	for _, name := range []string{"cover.jpg", "cover.jpeg", "cover.png"} {
		body, err := os.ReadFile(filepath.Join(folder, name))
		if err == nil && imageBytes(body) {
			return body
		}
	}
	return nil
}

func firstValue(values []string) string {
	return strings.TrimSpace(strings.Join(values, "\n"))
}
