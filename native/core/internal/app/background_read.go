package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"time"
)

const (
	// Duplicate detection reads only files that survived the in-memory index
	// filters. The cache makes this a one-time pass per changed file, while the
	// rate and import rate keep that pass from starving foreground playback.
	duplicateFingerprintBytesPerSecond int64   = 8 << 20
	duplicateFingerprintMaxPCMReadRate float64 = 32
	duplicateFingerprintMinPCMReadRate float64 = 0.25
	duplicateFingerprintReadChunk              = 256 << 10
)

// duplicatePCMReadRate chooses a realtime multiplier that keeps compressed
// source reads near the same byte-rate budget. The 32x ceiling protects very
// small files from unbounded relative speed; the byte calculation protects
// long, high-bitrate files that would otherwise exceed it.
func duplicatePCMReadRate(size int64, duration float64) float64 {
	if size <= 0 || duration <= 0 {
		return duplicateFingerprintMaxPCMReadRate
	}
	rate := float64(duplicateFingerprintBytesPerSecond) * duration / float64(size)
	if rate > duplicateFingerprintMaxPCMReadRate {
		return duplicateFingerprintMaxPCMReadRate
	}
	if rate < duplicateFingerprintMinPCMReadRate {
		return duplicateFingerprintMinPCMReadRate
	}
	return rate
}

func hashDuplicateFile(ctx context.Context, path string) (string, error) {
	var digest string
	err := runWithBackgroundIO(func() error {
		var err error
		digest, err = hashFileAtRate(ctx, path, duplicateFingerprintBytesPerSecond)
		return err
	})
	return digest, err
}

func hashFileAtRate(ctx context.Context, path string, bytesPerSecond int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	sum := sha256.New()
	if err := copyAtRate(ctx, sum, file, bytesPerSecond); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// copyAtRate caps the average read rate while retaining small read bursts.
// The caller can cancel a large file without waiting for the whole file to be
// consumed.
func copyAtRate(ctx context.Context, dst io.Writer, src io.Reader, bytesPerSecond int64) error {
	if bytesPerSecond <= 0 {
		_, err := io.Copy(dst, src)
		return err
	}
	buf := make([]byte, duplicateFingerprintReadChunk)
	started := time.Now()
	var total int64
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			emptyReads = 0
			written, writeErr := dst.Write(buf[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
			total += int64(n)
			target := time.Duration(float64(total) / float64(bytesPerSecond) * float64(time.Second))
			if wait := target - time.Since(started); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
		emptyReads++
		if emptyReads >= 100 {
			return io.ErrNoProgress
		}
	}
}
