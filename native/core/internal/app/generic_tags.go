package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/artistnorm"
	"github.com/wsdbb72/MusicOps/native/core/internal/musicbrainz"
)

type genericAudioInfo struct {
	Path        string
	Codec       string
	Artist      string
	AlbumArtist string
	Album       string
	Title       string
	Date        string
	Genre       string
	Track       string
	Disc        string
	Duration    float64
	Tags        map[string]string
	// Legacy APE 4.11 carries complete metadata but the bundled FFmpeg cannot
	// rewrite it. Such files remain indexable and matchable without a doomed
	// tag rewrite on every maintenance pass.
	TagWriteUnsupported bool
}

func probeGenericAudio(ctx context.Context, path string) (genericAudioInfo, error) {
	info := genericAudioInfo{Path: path, Tags: map[string]string{}}
	raw, err := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-show_entries", "stream=codec_name:format=duration:format_tags", "-of", "json", path).Output()
	if err != nil {
		if ctx.Err() != nil {
			return info, ctx.Err()
		}
		if strings.EqualFold(filepath.Ext(path), ".ape") {
			metadata, fallbackErr := probeAPEAudio(path)
			if fallbackErr == nil {
				info.Codec = "ape"
				info.Duration = metadata.Duration
				info.Tags = metadata.Tags
				info.TagWriteUnsupported = true
				if finalizeErr := finalizeGenericAudioInfo(&info, path); finalizeErr == nil {
					return info, nil
				}
			}
			if fallbackErr != nil {
				return info, fmt.Errorf("ffprobe: %w; APE 元数据兜底: %v", err, fallbackErr)
			}
		}
		return info, err
	}
	var data struct {
		Streams []struct {
			Codec string `json:"codec_name"`
		} `json:"streams"`
		Format struct {
			Duration string            `json:"duration"`
			Tags     map[string]string `json:"tags"`
		} `json:"format"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return info, err
	}
	if len(data.Streams) == 0 || strings.TrimSpace(data.Streams[0].Codec) == "" {
		return info, errors.New("音频流无法识别")
	}
	for key, value := range data.Format.Tags {
		info.Tags[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	info.Codec = strings.ToLower(data.Streams[0].Codec)
	info.Duration, _ = strconv.ParseFloat(data.Format.Duration, 64)
	if err := finalizeGenericAudioInfo(&info, path); err != nil {
		return info, err
	}
	return info, nil
}

func finalizeGenericAudioInfo(info *genericAudioInfo, path string) error {
	info.Artist = firstNonEmpty(info.Tags["artist"], info.Tags["artists"])
	info.AlbumArtist = firstNonEmpty(info.Tags["album_artist"], info.Tags["albumartist"], info.Tags["album artist"])
	info.Album = info.Tags["album"]
	info.Title = info.Tags["title"]
	info.Date = firstNonEmpty(info.Tags["date"], info.Tags["year"])
	info.Genre = info.Tags["genre"]
	info.Track = firstNonEmpty(info.Tags["track"], info.Tags["tracknumber"])
	info.Disc = firstNonEmpty(info.Tags["disc"], info.Tags["discnumber"])
	inferredArtist, inferredAlbum, inferredTitle := scrapeIdentityFromPath(path)
	info.Artist = firstNonEmpty(info.Artist, inferredArtist)
	info.Album = firstNonEmpty(info.Album, inferredAlbum)
	info.Title = firstNonEmpty(info.Title, inferredTitle)
	if info.Duration <= 0 {
		return errors.New("音频时长无法读取")
	}
	return nil
}

func maintenanceNeedsGenericTags(info genericAudioInfo) bool {
	if info.Artist == "" || info.Album == "" || info.Title == "" {
		return true
	}
	if info.TagWriteUnsupported {
		return false
	}
	if info.Tags["album_artist"] == "" && info.Tags["albumartist"] == "" && info.Tags["album artist"] == "" {
		return true
	}
	if info.Track == "" || info.Date == "" || info.Genre == "" {
		return true
	}
	return false
}

func genericMissingFields(info genericAudioInfo) map[string]bool {
	out := map[string]bool{}
	if strings.TrimSpace(info.Title) == "" {
		out["TITLE"] = true
	}
	if strings.TrimSpace(info.Artist) == "" {
		out["ARTIST"] = true
	}
	if strings.TrimSpace(info.Album) == "" {
		out["ALBUM"] = true
	}
	if info.TagWriteUnsupported {
		return out
	}
	if strings.TrimSpace(firstNonEmpty(info.Tags["album_artist"], info.Tags["albumartist"], info.Tags["album artist"])) == "" {
		out["ALBUMARTIST"] = true
	}
	if strings.TrimSpace(info.Date) == "" {
		out["DATE"] = true
	}
	if strings.TrimSpace(info.Genre) == "" {
		out["GENRE"] = true
	}
	if strings.TrimSpace(info.Track) == "" {
		out["TRACKNUMBER"] = true
	}
	if strings.TrimSpace(info.Disc) == "" {
		out["DISCNUMBER"] = true
	}
	return out
}

func writeGenericTags(ctx context.Context, path string, updates map[string][]string) ([]string, error) {
	if len(updates) == 0 {
		return nil, nil
	}
	beforeHash, err := genericAudioHash(ctx, path)
	if err != nil {
		return nil, err
	}
	ext := filepath.Ext(path)
	temp, err := os.CreateTemp(filepath.Dir(path), ".musicops-tags-*"+ext)
	if err != nil {
		return nil, err
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		os.Remove(tempPath)
		return nil, err
	}
	defer os.Remove(tempPath)
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", path, "-map", "0:a:0", "-map", "0:v?", "-map_metadata", "0", "-c", "copy"}
	for field, values := range updates {
		if len(values) == 0 {
			continue
		}
		key := strings.ToLower(field)
		value := strings.Join(values, " / ")
		args = append(args, "-metadata", key+"="+value)
	}
	if strings.EqualFold(ext, ".mp3") {
		args = append(args, "-id3v2_version", "3", "-write_id3v1", "1")
	}
	args = append(args, tempPath)
	output, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(string(output)))
	}
	afterHash, err := genericAudioHash(ctx, tempPath)
	if err != nil || afterHash != beforeHash {
		return nil, errors.New("写标签后音频流发生变化")
	}
	backup, err := backupFile(path)
	if err != nil {
		return nil, err
	}
	if err := renameFileWithRetry(tempPath, path); err != nil {
		if restoreErr := restoreBackup(path, backup); restoreErr != nil {
			return nil, errors.Join(err, restoreErr)
		}
		return nil, err
	}
	_ = os.Remove(backup)
	fields := make([]string, 0, len(updates))
	for field := range updates {
		fields = append(fields, strings.ToLower(field))
	}
	return fields, nil
}

func genericAudioHash(ctx context.Context, path string) (string, error) {
	raw, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-i", path, "-map", "0:a:0", "-f", "streamhash", "-hash", "sha256", "-").Output()
	if err != nil {
		return "", err
	}
	return parseGenericAudioHash(string(raw))
}

// parseGenericAudioHash accepts both streamhash layouts emitted by supported
// FFmpeg releases: "0,a,SHA256=<hex>" and "0,a,<hex>".
func parseGenericAudioHash(raw string) (string, error) {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, value, found := strings.Cut(line, "="); found {
			if hash := findSHA256Hex(value); hash != "" {
				return hash, nil
			}
		}
		if hash := findSHA256Hex(line); hash != "" {
			return hash, nil
		}
	}
	return "", errors.New("无法计算音频流指纹")
}

func findSHA256Hex(value string) string {
	for index := 0; index+64 <= len(value); index++ {
		candidate := value[index : index+64]
		if _, err := hex.DecodeString(candidate); err == nil {
			return strings.ToLower(candidate)
		}
	}
	return ""
}

type genericScrapeGroup struct {
	artist, album string
	files         []genericAudioInfo
}

func (a *App) scrapeGenericFiles(ctx context.Context, paths []string, client *musicbrainz.Client, overwrite, dryRun, withArtwork bool) map[string]*scrapeFileResult {
	results := map[string]*scrapeFileResult{}
	groups := []*genericScrapeGroup{}
	byAlbum := map[string]*genericScrapeGroup{}
	for _, path := range paths {
		results[path] = &scrapeFileResult{Path: path}
		info, err := probeGenericAudio(ctx, path)
		if err != nil {
			results[path].Status, results[path].Code, results[path].Note = "skipped", "TAG_READ_FAILED", "标签或时长读不出来"
			continue
		}
		if info.Artist == "" || info.Album == "" || info.Title == "" {
			results[path].Status, results[path].Code, results[path].Note = "skipped", "NO_TAGS", "缺艺人、专辑或曲名，且路径不足以推断"
			continue
		}
		key := artistnorm.Key(info.Artist) + "\x00" + foldTagText(info.Album)
		group := byAlbum[key]
		if group == nil {
			group = &genericScrapeGroup{artist: info.Artist, album: info.Album}
			byAlbum[key] = group
			groups = append(groups, group)
		}
		group.files = append(group.files, info)
	}
	for _, group := range groups {
		tracks := make([]catalogTrackRequest, 0, len(group.files))
		for _, info := range group.files {
			duration := info.Duration
			tracks = append(tracks, catalogTrackRequest{Title: info.Title, Artists: []string{info.Artist}, Duration: &duration})
		}
		lookup, err := identifyCatalogLookup(ctx, client, a.engine, catalogIdentifyRequest{Artist: group.artist, Album: group.album, Tracks: tracks}, catalogBudget, catalogTTL)
		var artworkByPath map[string]scrapeArtworkData
		if withArtwork && !dryRun {
			requests := make([]scrapeArtworkLookup, 0, len(group.files))
			for _, info := range group.files {
				requests = append(requests, scrapeArtworkLookup{Path: info.Path, Artist: info.Artist, Album: info.Album, Title: info.Title})
			}
			artworkByPath = a.fetchScrapeArtworkBatch(ctx, requests)
		}
		if err != nil || lookup.MusicBrainz == nil {
			for _, info := range group.files {
				result := results[info.Path]
				result.Status, result.Code, result.Note = "failed", "CATALOG_UNCONFIRMED", "目录没有确认这张专辑"
				if withArtwork && !dryRun {
					lyrics, cover, artErr := a.writeScrapeArtworkSidecars(info.Path, artworkByPath[info.Path], false)
					if artErr != nil {
						result.Code = "ARTWORK_UNAVAILABLE"
						result.Note += "；歌词或封面读取失败：" + scrub(artErr.Error())
					} else if lyrics || cover {
						result.Status = "written"
						result.Lyrics, result.LyricsSidecar, result.Cover = lyrics, lyrics, cover
						result.Note += "；已按现有标签补写可独立验证的歌词或封面"
					}
				}
			}
			continue
		}
		for i, info := range group.files {
			result := results[info.Path]
			confirmedTrack := i >= len(lookup.Results) || lookup.Results[i].IdentityConfirmed
			track, found := matchCatalogTrackForResult(lookup.MusicBrainz, info.Title, info.Duration, confirmedTrack)
			if !found {
				result.Status, result.Code, result.Note = "failed", "TRACK_NOT_FOUND", "目录里没有找到对应曲目"
				continue
			}
			desired := desiredTags(lookup.MusicBrainz, track)
			updates := map[string][]string{}
			for field, value := range desired {
				if overwrite || genericFieldEmpty(info, field) {
					updates[field] = value
				}
			}
			updates, locked := a.applyFieldLocks(filepath.Dir(info.Path), updates)
			result.LockedFields = locked
			if dryRun {
				for field := range updates {
					result.WrittenFields = append(result.WrittenFields, strings.ToLower(field))
				}
				result.Status = "planned"
				if len(updates) == 0 {
					result.Status = "unchanged"
				}
				continue
			}
			written, err := writeGenericTags(ctx, info.Path, updates)
			if err != nil {
				result.Status, result.Code, result.Note = "failed", "TAG_WRITE_FAILED", err.Error()
				continue
			}
			result.WrittenFields = written
			if withArtwork {
				art := artworkByPath[info.Path]
				if art.Err != nil {
					result.Note = "标签已写入，但歌词封面服务不可用：" + scrub(art.Err.Error())
				} else {
					lyrics, cover, artErr := a.writeScrapeArtworkSidecars(info.Path, art, overwrite)
					if artErr != nil {
						result.Note = "标签已写入，但歌词封面写入失败：" + scrub(artErr.Error())
					} else {
						result.Lyrics, result.LyricsSidecar, result.Cover = lyrics, lyrics, cover
					}
				}
			}
			if len(result.WrittenFields) > 0 || result.Cover || result.Lyrics {
				result.Status = "written"
			} else {
				result.Status = "unchanged"
			}
		}
	}
	return results
}

func genericFieldEmpty(info genericAudioInfo, field string) bool {
	switch field {
	case "TITLE":
		return info.Title == ""
	case "ARTIST":
		return info.Artist == ""
	case "ALBUM":
		return info.Album == ""
	case "ALBUMARTIST":
		if info.TagWriteUnsupported {
			return false
		}
		return firstNonEmpty(info.Tags["album_artist"], info.Tags["albumartist"], info.Tags["album artist"]) == ""
	case "DATE":
		if info.TagWriteUnsupported {
			return false
		}
		return info.Date == ""
	case "GENRE":
		if info.TagWriteUnsupported {
			return false
		}
		return info.Genre == ""
	case "TRACKNUMBER":
		if info.TagWriteUnsupported {
			return false
		}
		return info.Track == ""
	case "DISCNUMBER":
		if info.TagWriteUnsupported {
			return false
		}
		return info.Disc == ""
	default:
		return false
	}
}
