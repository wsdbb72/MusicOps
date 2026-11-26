package artistnorm

import "testing"

func TestKnownArtistAliasesShareIdentity(t *testing.T) {
	for _, alias := range []string{"周杰伦", "周傑倫", "Jay Chou", "JayChou", "jay-chou"} {
		if got := Canonical(alias); got != "周杰伦" {
			t.Fatalf("Canonical(%q)=%q", alias, got)
		}
		if Key(alias) != Key("周杰伦") || !Same(alias, "周杰伦") {
			t.Fatalf("alias %q did not share the canonical key: %q != %q", alias, Key(alias), Key("周杰伦"))
		}
	}
}

func TestUnknownNamesKeepIdentityAndTraditionalDisplayFoldsOnlyForHan(t *testing.T) {
	if got := Canonical("The Weeknd"); got != "The Weeknd" {
		t.Fatalf("unknown Latin artist changed: %q", got)
	}
	if got := Canonical("張學友"); got != "张学友" {
		t.Fatalf("Han display variant was not folded: %q", got)
	}
	if Same("Jay", "JAY-Z") {
		t.Fatal("short names or punctuation alone must not collapse unrelated artists")
	}
}
