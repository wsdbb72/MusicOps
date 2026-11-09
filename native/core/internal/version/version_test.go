package version

import "testing"

func TestDefaultVersionIsDev(t *testing.T) {
	if Version != "0.0.0-dev" {
		t.Fatalf("default Version = %q, want the honest dev marker 0.0.0-dev", Version)
	}
}
