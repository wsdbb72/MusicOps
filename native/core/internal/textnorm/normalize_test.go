package textnorm

import "testing"

func TestNormalizeCompatibilityAndTypography(t *testing.T) {
	cases := map[string]string{
		"ＢＥＹＯＮＣＥ\u0301 — ＬＯＶＥ\u200b": "BEYONCÉ - LOVE",
		"ﾊﾛｰ・ﾜｰﾙﾄﾞ":                  "ハロー・ワールド",
		"track… “quoted”":            "track... \"quoted\"",
		"一話。二、三":                     "一話.二,三",
	}
	for input, want := range cases {
		if got := Normalize(input); got != want {
			t.Fatalf("Normalize(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestFoldUsesFullUnicodeCaseFolding(t *testing.T) {
	for input, want := range map[string]string{
		"ЁЖ":      "ёж",
		"Straße":  "strasse",
		"ΟΣ":      "οσ",
		"Ｂeyoncé": "beyoncé",
	} {
		if got := Fold(input); got != want {
			t.Fatalf("Fold(%q)=%q, want %q", input, got, want)
		}
	}
}
