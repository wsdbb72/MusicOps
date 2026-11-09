package musicbrainz

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
)

// Release is one row of a MusicBrainz release search (/ws/2/release/).
// Score is the server's relevance score; higher is a better match.
type Release struct {
	MBID       string
	Title      string
	Date       string
	Country    string
	Status     string
	Artists    []string
	TrackCount int
	Score      int
}

// DetailTrack is one track of a release lookup. LengthMs is the track
// length in milliseconds as reported by the service (0 when absent);
// DurationSec is LengthMs / 1000 and HasDuration is false when the service
// gave no positive length.
type DetailTrack struct {
	RecordingMBID string
	Disc          int
	Position      int
	Title         string
	Artists       []string
	LengthMs      int64
	DurationSec   float64
	HasDuration   bool
}

// ReleaseDetail is a full release lookup (/ws/2/release/<mbid> with
// inc=artists+recordings). TracklistComplete is true only when the release
// has at least one track and every track has a title and a positive length;
// anything less means the tracklist cannot be trusted as complete.
type ReleaseDetail struct {
	MBID              string
	Title             string
	Date              string
	Country           string
	Status            string
	Artists           []string
	Tracks            []DetailTrack
	TracklistComplete bool
}

type searchReleaseRow struct {
	ID           string          `json:"id"`
	Title        string          `json:"title"`
	Date         string          `json:"date"`
	Country      string          `json:"country"`
	Status       string          `json:"status"`
	ArtistCredit json.RawMessage `json:"artist-credit"`
	Media        []struct {
		TrackCount int `json:"track-count"`
	} `json:"media"`
	Score int `json:"score"`
}

type searchResponse struct {
	Releases []searchReleaseRow `json:"releases"`
}

// SearchRelease searches releases by artist, album and/or recording title.
// At least one input must be non-empty; an all-empty query is rejected
// with ErrEmptyQuery and no request is sent.
//
// Rows missing an id or title, or carrying a malformed MBID, are skipped
// rather than returned: a candidate without a verifiable identity must
// never reach the caller. An empty result set is not an error: it returns
// an empty (non-nil) slice. Results are sorted by score descending.
func (c *Client) SearchRelease(artist, album, title string) ([]Release, error) {
	query, err := buildQuery(artist, album, title)
	if err != nil {
		return nil, err
	}
	params := url.Values{
		"query": {query},
		"limit": {strconv.Itoa(searchLimit)},
	}
	body, err := c.get("/ws/2/release", params)
	if err != nil {
		return nil, err
	}
	var payload searchResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, &ParseError{Message: "search response is not valid JSON", Err: err}
	}
	releases := make([]Release, 0, len(payload.Releases))
	for _, row := range payload.Releases {
		mbid, err := normalizeMBID(row.ID)
		if err != nil {
			continue
		}
		if row.Title == "" {
			continue
		}
		trackCount := 0
		for _, medium := range row.Media {
			trackCount += medium.TrackCount
		}
		releases = append(releases, Release{
			MBID:       mbid,
			Title:      row.Title,
			Date:       row.Date,
			Country:    row.Country,
			Status:     row.Status,
			Artists:    parseArtistCredit(row.ArtistCredit),
			TrackCount: trackCount,
			Score:      row.Score,
		})
	}
	sort.SliceStable(releases, func(i, j int) bool {
		return releases[i].Score > releases[j].Score
	})
	return releases, nil
}

type detailTrackRow struct {
	ID           string          `json:"id"`
	Title        string          `json:"title"`
	Position     int             `json:"position"`
	Length       *int64          `json:"length"`
	ArtistCredit json.RawMessage `json:"artist-credit"`
	Recording    *struct {
		ID           string          `json:"id"`
		Title        string          `json:"title"`
		Length       *int64          `json:"length"`
		ArtistCredit json.RawMessage `json:"artist-credit"`
	} `json:"recording"`
}

type detailResponse struct {
	ID           string          `json:"id"`
	Title        string          `json:"title"`
	Date         string          `json:"date"`
	Country      string          `json:"country"`
	Status       string          `json:"status"`
	ArtistCredit json.RawMessage `json:"artist-credit"`
	Media        []struct {
		Position   int              `json:"position"`
		TrackCount int              `json:"track-count"`
		Tracks     []detailTrackRow `json:"tracks"`
	} `json:"media"`
}

// GetRelease fetches one release by MBID, including its recordings. The
// MBID is validated before any request is sent. A response whose id does
// not match the requested MBID is a ParseError, never a result.
func (c *Client) GetRelease(mbid string) (ReleaseDetail, error) {
	normalised, err := normalizeMBID(mbid)
	if err != nil {
		return ReleaseDetail{}, err
	}
	params := url.Values{"inc": {"artists+recordings"}}
	body, err := c.get("/ws/2/release/"+normalised, params)
	if err != nil {
		return ReleaseDetail{}, err
	}
	var payload detailResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return ReleaseDetail{}, &ParseError{Message: "detail response is not valid JSON", Err: err}
	}
	if id, err := normalizeMBID(payload.ID); err != nil || id != normalised {
		return ReleaseDetail{}, &ParseError{Message: fmt.Sprintf("detail response id %q does not match requested MBID %q", payload.ID, normalised)}
	}
	if payload.Title == "" {
		return ReleaseDetail{}, &ParseError{Message: "detail response has no title"}
	}
	detail := ReleaseDetail{
		MBID:    normalised,
		Title:   payload.Title,
		Date:    payload.Date,
		Country: payload.Country,
		Status:  payload.Status,
		Artists: parseArtistCredit(payload.ArtistCredit),
	}
	complete := true
	for _, medium := range payload.Media {
		for _, row := range medium.Tracks {
			track := DetailTrack{
				Disc:     medium.Position,
				Position: row.Position,
				Title:    row.Title,
			}
			if row.Recording != nil {
				track.RecordingMBID, _ = normalizeMBID(row.Recording.ID)
				if track.Title == "" {
					track.Title = row.Recording.Title
				}
			}
			artists := parseArtistCredit(row.ArtistCredit)
			if len(artists) == 0 && row.Recording != nil {
				artists = parseArtistCredit(row.Recording.ArtistCredit)
			}
			if len(artists) == 0 {
				artists = detail.Artists
			}
			track.Artists = artists
			length := row.Length
			if length == nil && row.Recording != nil {
				length = row.Recording.Length
			}
			if length != nil && *length > 0 {
				track.LengthMs = *length
				track.DurationSec = float64(*length) / 1000
				track.HasDuration = true
			}
			if track.Title == "" || !track.HasDuration {
				complete = false
			}
			detail.Tracks = append(detail.Tracks, track)
		}
	}
	detail.TracklistComplete = complete && len(detail.Tracks) > 0
	return detail, nil
}
