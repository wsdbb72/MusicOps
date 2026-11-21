package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Roon, Plex and Navidrome read "<track>.lrc"; "<track>.flac.lrc" is read by
// none of them. The sidecar is UTF-8 without BOM and ends with a newline.
func TestWriteLyricsSidecarUsesTheStandardName(t *testing.T) {
	dir := t.TempDir()
	audio := filepath.Join(dir, "01 - Song.flac")
	if err := os.WriteFile(audio, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeLyricsSidecar(audio, "[00:01.00]hello\n\n"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "01 - Song.lrc"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "[00:01.00]hello\n" {
		t.Fatalf("%q", body)
	}
	if _, err := os.Stat(audio + ".lrc"); err == nil {
		t.Fatal("the audio-extension spelling was written")
	}
}

// An older "<track>.flac.lrc" counts as existing lyrics and is replaced by the
// standard file, so a player cannot pick up a stale copy.
func TestWriteLyricsSidecarReplacesTheLegacyName(t *testing.T) {
	dir := t.TempDir()
	audio := filepath.Join(dir, "track.flac")
	if err := os.WriteFile(audio, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audio+".lrc", []byte("[00:01.00]old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !hasLyricsSidecar(audio) {
		t.Fatal("a legacy sidecar was not recognised as existing lyrics")
	}
	if err := writeLyricsSidecar(audio, "[00:02.00]new"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(audio + ".lrc"); !os.IsNotExist(err) {
		t.Fatal("the legacy sidecar was left behind")
	}
	body, err := os.ReadFile(filepath.Join(dir, "track.lrc"))
	if err != nil || !strings.Contains(string(body), "new") {
		t.Fatal(err, string(body))
	}
}

// Empty lyrics never create an empty file.
func TestWriteLyricsSidecarSkipsEmptyText(t *testing.T) {
	dir := t.TempDir()
	audio := filepath.Join(dir, "track.flac")
	if err := os.WriteFile(audio, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeLyricsSidecar(audio, "  \n\t\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "track.lrc")); !os.IsNotExist(err) {
		t.Fatal("an empty sidecar was written")
	}
}

func TestWriteLyricsSidecarKeepsOldFileWhenAtomicRenameFails(t *testing.T) {
	dir := t.TempDir()
	audio := filepath.Join(dir, "track.flac")
	sidecar := filepath.Join(dir, "track.lrc")
	old := []byte("[00:01.00]old\n")
	if err := os.WriteFile(audio, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecar, old, 0o644); err != nil {
		t.Fatal(err)
	}
	previous := atomicReplaceRename
	atomicReplaceRename = func(oldPath, newPath string) error {
		if newPath == sidecar {
			return errors.New("injected lrc rename failure")
		}
		return os.Rename(oldPath, newPath)
	}
	t.Cleanup(func() { atomicReplaceRename = previous })
	if err := writeLyricsSidecar(audio, "[00:02.00]new"); err == nil {
		t.Fatal("lyrics write unexpectedly succeeded")
	}
	if body, err := os.ReadFile(sidecar); err != nil || string(body) != string(old) {
		t.Fatalf("old lyrics changed: %q err=%v", body, err)
	}
}
