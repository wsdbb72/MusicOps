package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDuplicatePCMReadRateCapsCompressedBytes(t *testing.T) {
	if got := duplicatePCMReadRate(8<<20, 120); got != 32 {
		t.Fatalf("small-file read rate = %v, want 32", got)
	}
	if got := duplicatePCMReadRate(100<<20, 120); got < 9.5 || got > 9.7 {
		t.Fatalf("large-file read rate = %v, want about 9.6x", got)
	}
	if got := duplicatePCMReadRate(4096<<20, 120); got != duplicateFingerprintMinPCMReadRate {
		t.Fatalf("huge-file read rate = %v, want floor %v", got, duplicateFingerprintMinPCMReadRate)
	}
}

func TestHashFileAtRateMatchesNormalDigest(t *testing.T) {
	body := bytes.Repeat([]byte("musicops-background-read-"), 4096)
	path := filepath.Join(t.TempDir(), "audio.flac")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := hashFileAtRate(context.Background(), path, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("digest = %s, want %s", got, want)
	}
}

func TestCopyAtRateHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelAfterReader{cancel: cancel}
	err := copyAtRate(ctx, &bytes.Buffer{}, reader, 1024)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copyAtRate error = %v, want context.Canceled", err)
	}
}

func TestRateLimitedMoveCancellationKeepsSource(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.flac")
	target := filepath.Join(dir, "target.flac")
	if err := os.WriteFile(source, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := moveCheckedFileAtRate(ctx, source, target, "ignored", 1024)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("move error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(source); statErr != nil {
		t.Fatalf("source disappeared after cancellation: %v", statErr)
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("partial target remained after cancellation: %v", statErr)
	}
}

func TestCopyAtRateRejectsShortWrite(t *testing.T) {
	err := copyAtRate(context.Background(), shortWriter{}, bytes.NewReader([]byte("abc")), 64<<20)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("copyAtRate error = %v, want short write", err)
	}
}

type cancelAfterReader struct{ cancel context.CancelFunc }

func (r *cancelAfterReader) Read(p []byte) (int, error) {
	n := copy(p, "x")
	r.cancel()
	return n, nil
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
