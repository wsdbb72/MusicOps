package app

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/reviewmatch"
)

type localWorkflowTrack struct {
	Path                 string
	Artist, Album, Title string
	Codec                string
	Duration             float64
}

func probeLocalWorkflowTrack(ctx context.Context, path string) (localWorkflowTrack, error) {
	var result localWorkflowTrack
	raw, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name:format=duration:format_tags=artist,albumartist,album,title,comment", "-of", "json", path).Output()
	if err != nil {
		return result, err
	}
	var data struct {
		Streams []struct {
			Codec string `json:"codec_name"`
		}
		Format struct {
			Duration string
			Tags     map[string]string
		}
	}
	if json.Unmarshal(raw, &data) != nil || len(data.Streams) != 1 {
		return result, errors.New("无法核实本地音频格式")
	}
	tags := map[string]string{}
	for key, value := range data.Format.Tags {
		tags[strings.ToLower(key)] = strings.TrimSpace(value)
	}
	seconds, _ := strconv.ParseFloat(data.Format.Duration, 64)
	if tags["artist"] == "" || tags["album"] == "" || tags["title"] == "" || !(seconds > 0 && seconds < 86400) {
		return result, errors.New("本地文件缺少可核实的艺人、专辑、曲名或时长")
	}
	codec := strings.ToLower(data.Streams[0].Codec)
	if codec == "flac" && isLossyFLACProvenance(tags["comment"]) {
		codec = "flac-lossy"
	}
	return localWorkflowTrack{Path: path, Artist: tags["artist"], Album: tags["album"], Title: tags["title"], Codec: codec, Duration: seconds}, nil
}

func isLossyFLACProvenance(comment string) bool {
	comment = strings.ToLower(comment)
	return strings.Contains(comment, "musicops source=") && strings.Contains(comment, "-lossy")
}
func isLossyUpgradeSource(codec string) bool {
	switch codec {
	case "mp3", "aac", "opus", "vorbis", "flac-lossy":
		return true
	}
	return false
}

func (a *App) confirmSelectedLibrary(ctx context.Context, plan *SearchPlan) error {
	req := catalogIdentifyRequest{Artist: plan.ExpectedArtist, Album: plan.ExpectedTitle, Title: plan.LibraryTracks[0].Title}
	for _, track := range plan.LibraryTracks {
		if err := ctx.Err(); err != nil {
			return err
		}
		fresh, err := probeLocalWorkflowTrack(ctx, track.Path)
		if err != nil || fresh != track {
			return errors.New("所选音频已变化，需重新选择")
		}
		duration := track.Duration
		req.Tracks = append(req.Tracks, catalogTrackRequest{Title: track.Title, Artists: []string{track.Artist}, Duration: &duration})
	}
	resolved, err := identifyCatalogLookup(catalogClient, a.engine, req, catalogBudget, catalogTTL)
	confirmed := err == nil && resolved.MusicBrainz != nil && len(resolved.Results) == len(req.Tracks)
	if confirmed {
		for _, row := range resolved.Results {
			if !row.IdentityConfirmed {
				confirmed = false
				break
			}
		}
	}
	if confirmed {
		raw, marshalErr := json.Marshal(resolved.MusicBrainz)
		if marshalErr != nil {
			return marshalErr
		}
		plan.Identity = Identity{ID: "catalog-" + plan.RootJobID, Kind: "catalog", Source: "musicbrainz", SubjectID: resolved.MusicBrainz.ReleaseID, TracklistVerified: true, IdentityConfirmed: true, Payload: string(raw)}
		return nil
	}
	// Library maintenance must still work for private, regional and doujin
	// releases that MusicBrainz does not list. The files were already re-read
	// above, so use their local album, artist, title and duration as a
	// searchable platform identity. Tracker/streaming candidates still undergo
	// the normal title, artist, edition and duration checks before download.
	localPayload, err := json.Marshal(map[string]any{
		"source": "local-library", "artist": plan.ExpectedArtist, "album": plan.ExpectedTitle,
		"tracks": plan.LibraryTracks,
	})
	if err != nil {
		return err
	}
	plan.Identity = Identity{
		ID: "local-" + libraryFileID(plan.RootJobID), Kind: "catalog", Source: "local-library",
		SubjectID: plan.RootJobID, TracklistVerified: true, IdentityConfirmed: true, Payload: string(localPayload),
	}
	return nil
}

func localUpgradeAdmits(plan SearchPlan, hit reviewmatch.Hit) bool {
	if plan.Scene != "upgrade" {
		return true
	}
	if len(plan.LibraryTracks) == 0 || !strings.EqualFold(hit.Format, "flac") {
		return false
	}
	for _, track := range plan.LibraryTracks {
		if !isLossyUpgradeSource(track.Codec) {
			return false
		}
	}
	return true
}
