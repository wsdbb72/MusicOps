package app

// Lyrics sidecars. Roon, Plex and Navidrome all read a "<track>.lrc" next to
// the audio (the audio file's name without its extension); an embedded LYRICS
// tag alone is invisible to players that are configured to prefer files, and
// Roon's tag import has to be enabled by hand. So every path that writes
// lyrics writes both: the tag (for players that read tags) and the sidecar
// (for the ones that read files).

import (
	"os"

	"github.com/wsdbb72/MusicOps/native/core/internal/lrc"
)

// lyricsSidecarPath is "<track>.lrc" beside the audio file.
func lyricsSidecarPath(audioPath string) string {
	return lrc.SidecarPath(audioPath)
}

// hasLyricsSidecar reports whether a non-empty sidecar already exists, in the
// standard name or the older "<audio>.flac.lrc" spelling. An existing file is
// the user's own lyrics, so the overwrite flag is what decides whether it may
// be replaced.
func hasLyricsSidecar(audioPath string) bool {
	for _, path := range []string{lrc.SidecarPath(audioPath), audioPath + ".lrc"} {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return true
		}
	}
	return false
}

// writeLyricsSidecar writes the sidecar with a trailing newline. UTF-8 without
// BOM: a BOM turns into a stray character in some players, and none of the
// three need it to detect the encoding. The older "<audio>.flac.lrc" name is
// removed so a player cannot pick up a stale copy.
func writeLyricsSidecar(audioPath, lyrics string) error {
	body := trimLyricsBody(lyrics)
	if body == "" {
		return nil
	}
	if err := os.WriteFile(lrc.SidecarPath(audioPath), []byte(body+"\n"), 0o600); err != nil {
		return err
	}
	if legacy := audioPath + ".lrc"; legacy != lrc.SidecarPath(audioPath) {
		_ = os.Remove(legacy)
	}
	return nil
}

func trimLyricsBody(lyrics string) string {
	trimmed := lyrics
	for len(trimmed) > 0 && (trimmed[len(trimmed)-1] == '\n' || trimmed[len(trimmed)-1] == '\r') {
		trimmed = trimmed[:len(trimmed)-1]
	}
	for _, r := range trimmed {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			return trimmed
		}
	}
	return ""
}
