package musicbrainz

// ObservationTrack is one track of an Observation.
type ObservationTrack struct {
	Title       string
	Artists     []string
	DurationSec float64
	HasDuration bool
}

// Observation is this package's plain mapping of a ReleaseDetail onto the
// shape releaseidentity.Observation expects. It is defined here, rather
// than importing releaseidentity, so this package stays a leaf with no
// dependency on any other MusicOps package; the caller converts it
// field-for-field into a releaseidentity.Observation.
//
// Mapping (mirrors Python's _mb_detail candidate fields):
//   - Source is "musicbrainz" and Kind is "api": the evidence comes from
//     the official MusicBrainz API, which releaseidentity accepts as an
//     "api" observation.
//   - ReleaseID is "mbid:<MBID>", the same "mbid:" prefix the caller uses
//     to namespace MusicBrainz identifiers.
//   - Album is the release title, Edition is the release date
//     (YYYY-MM-DD, or a partial date as returned), Artists are the
//     release-level artist-credit names.
//   - Tracks carry each track's title and artists, DurationSec in seconds
//     (milliseconds / 1000), and HasDuration, which is false when the
//     service gave no positive length.
//   - TracklistComplete is carried through unchanged: it is true only when
//     every track has a title and a positive length.
//   - No Catalogue is populated: this lookup does not fetch label or
//     catalogue-number data, so none is invented.
type Observation struct {
	Source            string
	Kind              string
	ReleaseID         string
	Album             string
	Edition           string
	Artists           []string
	Tracks            []ObservationTrack
	TracklistComplete bool
}

// Observation maps a ReleaseDetail onto the Observation shape above.
func (d ReleaseDetail) Observation() Observation {
	obs := Observation{
		Source:            "musicbrainz",
		Kind:              "api",
		ReleaseID:         "mbid:" + d.MBID,
		Album:             d.Title,
		Edition:           d.Date,
		Artists:           d.Artists,
		TracklistComplete: d.TracklistComplete,
	}
	for _, track := range d.Tracks {
		obs.Tracks = append(obs.Tracks, ObservationTrack{
			Title:       track.Title,
			Artists:     track.Artists,
			DurationSec: track.DurationSec,
			HasDuration: track.HasDuration,
		})
	}
	return obs
}
