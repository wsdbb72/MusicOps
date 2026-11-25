//go:build windows

package desktop

import (
	"bytes"
	"image/png"
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

func TestEmbeddedTaskbarIconKeepsACrispHighContrastGlyph(t *testing.T) {
	bits, err := embeddedIconBits(windowIconICO, 16)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(bits))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 16 || img.Bounds().Dy() != 16 {
		t.Fatalf("small icon bounds=%v", img.Bounds())
	}
	white := 0
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			if a == 0xffff && r == 0xffff && g == 0xffff && b == 0xffff {
				white++
			}
		}
	}
	if white < 30 {
		t.Fatalf("taskbar icon has only %d opaque white glyph pixels; it will blur into the default tile", white)
	}
}
