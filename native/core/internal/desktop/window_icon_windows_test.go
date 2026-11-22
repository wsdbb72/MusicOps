//go:build windows

package desktop

import (
	"bytes"
	"testing"
)

func TestEmbeddedWindowIconSelectsRequestedFrame(t *testing.T) {
	small, err := embeddedIconBits(windowIconICO, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(small, []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatalf("small icon is not PNG data: %x", small[:min(8, len(small))])
	}
	large, err := embeddedIconBits(windowIconICO, 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(large) <= len(small) {
		t.Fatalf("64px icon length=%d, want greater than 16px length=%d", len(large), len(small))
	}
}

func TestEmbeddedWindowIconRejectsInvalidData(t *testing.T) {
	if _, err := embeddedIconBits([]byte("bad"), 32); err == nil {
		t.Fatal("invalid ICO data was accepted")
	}
}
