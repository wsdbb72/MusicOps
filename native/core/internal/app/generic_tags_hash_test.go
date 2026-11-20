package app

import "testing"

func TestParseGenericAudioHashSupportsCurrentAndLegacyFFmpegForms(t *testing.T) {
	const hash = "2076a8bf48d94d7b78f31c8ca5fcb26f2eee42eace550ffd1f0569bddc4b6f03"
	for _, raw := range []string{"0,a,SHA256=" + hash + "\n", "0,a," + hash + "\n", "0,a,0x" + hash + "\n"} {
		got, err := parseGenericAudioHash(raw)
		if err != nil || got != hash {
			t.Fatalf("raw=%q got=%q err=%v", raw, got, err)
		}
	}
	if _, err := parseGenericAudioHash("garbage\n"); err == nil {
		t.Fatal("malformed streamhash accepted")
	}
}
