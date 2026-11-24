package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/reviewmatch"
)

type localWorkflowTrack struct {
	Path                              string
	Artist, AlbumArtist, Album, Title string
	Codec                             string
	Duration                          float64
}

func probeLocalWorkflowTrack(ctx context.Context, path string) (localWorkflowTrack, error) {
	return probeLocalWorkflowTrackMode(ctx, path, false)
}

func probeLocalWorkflowTrackForRepair(ctx context.Context, path string) (localWorkflowTrack, error) {
	return probeLocalWorkflowTrackMode(ctx, path, true)
}

func probeLocalWorkflowTrackMode(ctx context.Context, path string, allowPathFallback bool) (localWorkflowTrack, error) {
	var result localWorkflowTrack
	info, statErr := os.Stat(path)
	if statErr != nil || !info.Mode().IsRegular() {
		if statErr != nil {
			return result, statErr
		}
		return result, errors.New("本地音频不是普通文件")
	}
	raw, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name:format=duration:format_tags=artist,albumartist,album,title,comment", "-of", "json", path).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if !allowPathFallback || !errors.As(err, &exitErr) || transientAudioProbeFailure(err) {
			return result, err
		}
		return localWorkflowTrackFromPath(path, "invalid", 0)
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
		if !allowPathFallback {
			return result, errors.New("无法核实本地音频格式")
		}
		return localWorkflowTrackFromPath(path, "invalid", 0)
	}
	tags := map[string]string{}
	for key, value := range data.Format.Tags {
		tags[strings.ToLower(key)] = strings.TrimSpace(value)
	}
	seconds, _ := strconv.ParseFloat(data.Format.Duration, 64)
	codec := strings.ToLower(data.Streams[0].Codec)
	if tags["artist"] == "" || tags["album"] == "" || tags["title"] == "" || !(seconds > 0 && seconds < 86400) {
		if !allowPathFallback {
			return result, errors.New("本地文件缺少可核实的艺人、专辑、曲名或时长")
		}
		if !(seconds > 0 && seconds < 86400) || codec == "" {
			codec = "invalid"
		}
		return localWorkflowTrackFromPath(path, codec, seconds)
	}
	if codec == "flac" && isLossyFLACProvenance(tags["comment"]) {
		codec = "flac-lossy"
	}
	return localWorkflowTrack{Path: path, Artist: tags["artist"], AlbumArtist: firstNonEmpty(tags["albumartist"], tags["album_artist"], tags["album artist"]), Album: tags["album"], Title: tags["title"], Codec: codec, Duration: seconds}, nil
}

func localWorkflowTrackFromPath(path, codec string, duration float64) (localWorkflowTrack, error) {
	artist, album, title, ok := localWorkflowIdentityFromPath(path)
	if !ok {
		return localWorkflowTrack{}, errors.New("本地文件损坏且路径不足以恢复艺人、专辑和曲名")
	}
	if strings.TrimSpace(codec) == "" {
		codec = "invalid"
	}
	if duration < 0 || duration >= 86400 {
		duration = 0
	}
	return localWorkflowTrack{Path: path, Artist: artist, AlbumArtist: artist, Album: album, Title: title, Codec: strings.ToLower(codec), Duration: duration}, nil
}

func transientAudioProbeFailure(err error) bool {
	text := strings.ToLower(err.Error())
	for _, marker := range []string{"access is denied", "permission denied", "sharing violation", "used by another process", "resource busy", "device or resource busy", "text file busy"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func localWorkflowIdentityFromPath(path string) (artist, album, title string, ok bool) {
	title = localWorkflowTitleFromPath(path)
	folder := filepath.Base(filepath.Dir(path))
	parent := filepath.Base(filepath.Dir(filepath.Dir(path)))
	if left, right, found := strings.Cut(folder, " - "); found && strings.TrimSpace(left) != "" && strings.TrimSpace(right) != "" {
		artist = strings.TrimSpace(left)
		album = strings.TrimSpace(right)
	} else {
		album = strings.TrimSpace(folder)
		artist = strings.TrimSpace(parent)
	}
	if title == "" || album == "" || artist == "" || parent == "." || parent == string(filepath.Separator) {
		return "", "", "", false
	}
	if left, right, found := strings.Cut(title, " - "); found && strings.TrimSpace(left) != "" && strings.TrimSpace(right) != "" {
		folderArtist := normalizeNeteaseArtist(artist)
		fileArtist := normalizeNeteaseArtist(left)
		if folderArtist == fileArtist || neteasePlaylistArtistsAreGeneric([]string{artist}) {
			if neteasePlaylistArtistsAreGeneric([]string{artist}) {
				artist = strings.TrimSpace(left)
			}
			title = strings.TrimSpace(right)
		}
	}
	return artist, album, title, true
}

func localWorkflowTitleFromPath(path string) string {
	title := strings.TrimSpace(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	digits, number, separator := 0, 0, -1
	for index, char := range title {
		if char < '0' || char > '9' {
			separator = index
			break
		}
		digits++
		number = number*10 + int(char-'0')
		if digits > 3 {
			break
		}
	}
	if digits > 0 && digits <= 3 && number >= 1 && number <= 999 && separator >= 0 {
		rest := title[separator:]
		trimmed := strings.TrimLeft(rest, " .-_")
		if trimmed != rest && strings.TrimSpace(trimmed) != "" {
			title = trimmed
		}
	}
	return strings.TrimSpace(title)
}

func isLossyFLACProvenance(comment string) bool {
	comment = strings.ToLower(comment)
	return strings.Contains(comment, "musicops source=") && strings.Contains(comment, "-lossy")
}
func isLossyUpgradeSource(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "invalid", "mp3", "mp2", "aac", "opus", "vorbis", "flac-lossy", "eac3", "ac3", "wma", "wmav1", "wmav2":
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
		var fresh localWorkflowTrack
		var err error
		if plan.Scene == "upgrade" {
			fresh, err = probeLocalWorkflowTrackForRepair(ctx, track.Path)
		} else {
			fresh, err = probeLocalWorkflowTrack(ctx, track.Path)
		}
		if err != nil || fresh != track {
			return errors.New("所选音频已变化，需重新选择")
		}
		duration := track.Duration
		req.Tracks = append(req.Tracks, catalogTrackRequest{Title: track.Title, Artists: []string{track.Artist}, Duration: &duration})
	}
	resolved, err := identifyCatalogLookup(ctx, catalogClient, a.engine, req, catalogBudget, catalogTTL)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
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
