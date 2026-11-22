package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
)

const (
	// A playlist recording can disappear from one catalogue row while another
	// release of the same recording remains playable. Keep that recovery
	// narrow: exact folded title, a compatible subset of the durable artist
	// credits, and no more than five seconds of duration drift.
	neteaseAlternateCandidateLimit          = 6
	neteaseAlternateDurationTolerance       = int64(5_000)
	neteaseAlternateSubsetDurationTolerance = int64(2_000)
)

// neteaseAlternateSongs returns deterministic alternative catalogue rows for
// an exact playlist recording whose own media URL is unavailable. The anchor
// is the catalogue response for the durable NetEase song id, so its title,
// artist credits and duration are stronger evidence than an arbitrary search
// result. A candidate may omit featured artists only when it is another row
// from the same album and its duration is within two seconds. Every credited
// candidate artist must be one of the durable artists, the primary artist must
// remain present, and any extra artist is rejected.
func (a *App) neteaseAlternateSongs(ctx context.Context, plan SearchPlan, anchor *neteaseSong) ([]neteaseSong, error) {
	if anchor == nil || anchor.ID <= 0 || strings.TrimSpace(plan.PlaylistTrack) == "" {
		return nil, nil
	}
	query := strings.TrimSpace(strings.Join(anchor.Artists, " ") + " " + firstNonEmpty(anchor.Name, plan.PlaylistTrack))
	if query == "" {
		return nil, nil
	}
	endpoint, err := url.Parse(a.neteaseBase() + "/api/search/get/web")
	if err != nil {
		return nil, err
	}
	values := endpoint.Query()
	values.Set("s", query)
	values.Set("type", "1")
	values.Set("offset", "0")
	values.Set("limit", "30")
	endpoint.RawQuery = values.Encode()
	body, err := a.neteaseGet(ctx, endpoint.String(), a.neteaseCookie())
	if err != nil {
		return nil, err
	}
	code, songs, err := decodeNeteaseSearch(body)
	if err != nil {
		return nil, err
	}
	if code != 0 && code != 200 {
		return nil, &neteaseHTTPError{operation: "替代录音搜索", status: code}
	}

	wantTitle := foldTagText(firstNonEmpty(anchor.Name, plan.PlaylistTrack))
	wantArtists := splitPlaylistArtists(plan.ExpectedArtist)
	if len(wantArtists) == 0 {
		wantArtists = anchor.Artists
	}
	type ranked struct {
		song          neteaseSong
		sameAlbumID   bool
		sameAlbumName bool
		exactArtists  bool
		drift         int64
		order         int
	}
	candidates := make([]ranked, 0, len(songs))
	seen := map[int64]bool{}
	for order, item := range songs {
		if item.ID <= 0 || item.ID == anchor.ID || seen[item.ID] || foldTagText(item.Name) != wantTitle {
			continue
		}
		artists := make([]string, 0, len(item.Artists))
		for _, artist := range item.Artists {
			if name := strings.TrimSpace(artist.Name); name != "" {
				artists = append(artists, name)
			}
		}
		if !neteaseAlternateArtistsMatch(wantArtists, artists) {
			continue
		}
		sameAlbumID := anchor.AlbumID > 0 && item.Album.ID == anchor.AlbumID
		exactArtists := sameNeteaseArtists(wantArtists, artists)
		drift := int64(0)
		comparableDuration := anchor.DurationMS > 0 && item.Duration > 0
		if comparableDuration {
			drift = int64(item.Duration - anchor.DurationMS)
			if drift < 0 {
				drift = -drift
			}
			if drift > neteaseAlternateDurationTolerance {
				continue
			}
		} else if !sameAlbumID {
			// Without two comparable durations, only another row from the same
			// album is specific enough to replace an unavailable recording.
			continue
		}
		if !exactArtists && !neteaseAlternateSubsetEvidenceAllowed(sameAlbumID, drift, comparableDuration) {
			// An omitted featured credit is only safe on the same album with a
			// near-identical duration. A different release needs the full set.
			continue
		}
		seen[item.ID] = true
		song := neteaseSong{
			ID: item.ID, Name: item.Name, Artists: artists, Album: item.Album.Name,
			DurationMS: item.Duration, AlbumID: item.Album.ID, CoverURL: item.Album.PicURL,
			CatalogVerified: true,
		}
		candidates = append(candidates, ranked{
			song:          song,
			sameAlbumID:   sameAlbumID,
			sameAlbumName: normalizeKey(normalizePlaylistAlbumTitle(item.Album.Name)) == normalizeKey(normalizePlaylistAlbumTitle(anchor.Album)),
			exactArtists:  exactArtists,
			drift:         drift,
			order:         order,
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.sameAlbumID != right.sameAlbumID {
			return left.sameAlbumID
		}
		if left.sameAlbumName != right.sameAlbumName {
			return left.sameAlbumName
		}
		if left.exactArtists != right.exactArtists {
			return left.exactArtists
		}
		if left.drift != right.drift {
			return left.drift < right.drift
		}
		if len(left.song.Artists) != len(right.song.Artists) {
			return len(left.song.Artists) > len(right.song.Artists)
		}
		return left.order < right.order
	})
	if len(candidates) > neteaseAlternateCandidateLimit {
		candidates = candidates[:neteaseAlternateCandidateLimit]
	}
	out := make([]neteaseSong, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.song)
	}
	return out, nil
}

// neteaseAlternateArtistsMatch accepts the complete credit list or a subset
// that retains the primary artist. This covers releases which drop a featured
// credit while rejecting an unrelated cover or remix artist.
func neteaseAlternateArtistsMatch(want, got []string) bool {
	want = splitPlaylistArtists(strings.Join(want, " / "))
	got = splitPlaylistArtists(strings.Join(got, " / "))
	if len(want) == 0 || len(got) == 0 {
		return false
	}
	primary := normalizeKey(want[0])
	hasPrimary := false
	for _, actual := range got {
		normalized := normalizeKey(actual)
		if normalized == primary {
			hasPrimary = true
		}
		found := false
		for _, expected := range want {
			if normalized != "" && normalized == normalizeKey(expected) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return hasPrimary
}

// neteaseAlternateSubsetEvidenceAllowed permits missing featured credits only
// on another row of the durable album with a near-identical duration. A subset
// from a different release is too easy to confuse with a remix or re-recording.
func neteaseAlternateSubsetEvidenceAllowed(sameAlbumID bool, drift int64, comparableDuration bool) bool {
	return sameAlbumID && comparableDuration && drift >= 0 && drift <= neteaseAlternateSubsetDurationTolerance
}

func sameNeteaseArtists(want, got []string) bool {
	want = splitPlaylistArtists(strings.Join(want, " / "))
	got = splitPlaylistArtists(strings.Join(got, " / "))
	if len(want) != len(got) {
		return false
	}
	for _, expected := range want {
		found := false
		for _, actual := range got {
			if normalizeKey(expected) == normalizeKey(actual) && normalizeKey(expected) != "" {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return len(want) > 0
}

func (a *App) finalizeNeteaseAlternate(ctx context.Context, path string, plan SearchPlan, alternate, anchor *neteaseSong, anchorDurationMS int) error {
	if err := a.ensureNeteaseAlternateCover(ctx, path, anchor, alternate); err != nil {
		return fmt.Errorf("对应封面无法保留：%w", err)
	}
	if err := validateNeteaseAlternateDuration(path, anchorDurationMS); err != nil {
		return err
	}
	if err := a.rewriteNeteaseRecoveredPlaylistTags(path, plan); err != nil {
		return fmt.Errorf("写回原曲信息失败：%w", err)
	}
	return nil
}

func (a *App) ensureNeteaseAlternateCover(ctx context.Context, path string, preferred, fallback *neteaseSong) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	existing := flactags.FrontCover(body)
	if preferred != nil {
		if cover, coverErr := a.neteaseCoverBytes(ctx, preferred); coverErr == nil && imageBytes(cover) {
			if imageBytes(existing) && bytes.Equal(existing, cover) {
				return nil
			}
			if len(existing) > 0 {
				body, err = flactags.RemoveFrontCover(body)
				if err != nil {
					return err
				}
			}
			body, err = flactags.EmbedFrontCover(body, cover)
			if err != nil {
				return err
			}
			return replaceFileAtomically(path, body, 0o600)
		}
	}
	if imageBytes(existing) {
		return nil
	}
	if len(existing) > 0 {
		body, err = flactags.RemoveFrontCover(body)
		if err != nil {
			return err
		}
	}
	if fallback == nil {
		return errors.New("网易云没有返回可用封面")
	}
	fetched, err := a.neteaseCoverBytes(ctx, fallback)
	if err != nil {
		return err
	}
	body, err = flactags.EmbedFrontCover(body, fetched)
	if err != nil {
		return err
	}
	return replaceFileAtomically(path, body, 0o600)
}

func validateNeteaseAlternateDuration(path string, wantMS int) error {
	if wantMS <= 0 {
		return nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	tags, err := flactags.ReadBytes(body)
	if err != nil || tags.StreamInfo.SampleRate == 0 || tags.StreamInfo.TotalSamples == 0 {
		return errors.New("替代录音没有可验证时长")
	}
	actualMS := int64(tags.StreamInfo.TotalSamples) * 1000 / int64(tags.StreamInfo.SampleRate)
	drift := actualMS - int64(wantMS)
	if drift < 0 {
		drift = -drift
	}
	if drift > neteaseAlternateDurationTolerance {
		return fmt.Errorf("替代录音实际时长偏差 %d 毫秒，超过安全上限", drift)
	}
	return nil
}

// rewriteNeteaseRecoveredPlaylistTags restores the playlist's durable identity
// after a playable alternate release supplied the audio bytes. The durable
// recording cover is preferred and the alternate's cover is the fallback;
// title, credits and album follow the row the user actually imported.
func (a *App) rewriteNeteaseRecoveredPlaylistTags(path string, plan SearchPlan) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	tags, err := flactags.ReadBytes(body)
	if err != nil {
		return err
	}
	title := strings.TrimSpace(firstNonEmpty(plan.PlaylistTrack, plan.ExpectedTitle))
	artists := splitPlaylistArtists(plan.ExpectedArtist)
	album := strings.TrimSpace(plan.ExpectedTitle)
	raw := tags.Raw
	if raw == nil {
		raw = map[string][]string{}
	}
	if title != "" {
		raw["TITLE"] = []string{truncateName(title)}
	}
	if len(artists) > 0 {
		raw["ARTIST"] = append([]string(nil), artists...)
	}
	if album != "" {
		raw["ALBUM"] = []string{truncateName(album)}
	}
	albumArtist := strings.TrimSpace(plan.AlbumArtist)
	if albumArtist == "" && len(artists) > 0 {
		albumArtist = strings.Join(artists, " / ")
	}
	if albumArtist != "" {
		raw["ALBUMARTIST"] = []string{truncateName(albumArtist)}
	}
	return flactags.Write(path, flactags.Tags{Raw: raw})
}
