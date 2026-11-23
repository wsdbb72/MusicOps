package app

import (
	"errors"
	"strings"
	"testing"
)

func TestCandidateFailureMessageKeepsSafeDiagnostics(t *testing.T) {
	err := errors.New("detail response HTTP 503 for authkey=sentinel-auth&cookie=sentinel-cookie; parser said STREAMINFO block is 0 bytes")
	got := candidateFailureMessage("站点详情读取失败", err)
	for _, want := range []string{"HTTP 503", "STREAMINFO block is 0 bytes", "authkey=redacted", "cookie=redacted"} {
		if !strings.Contains(got, want) {
			t.Fatalf("message %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "sentinel-") {
		t.Fatalf("credential leaked: %q", got)
	}
}

func TestCandidateFailureMessageIsBounded(t *testing.T) {
	got := candidateFailureMessage("失败", errors.New(strings.Repeat("x", candidateDiagnosticLimit*4)))
	if len([]rune(got)) > len([]rune("失败："))+candidateDiagnosticLimit+1 {
		t.Fatalf("diagnostic was not bounded: %d runes", len([]rune(got)))
	}
}

func TestCandidateReadFailureClassKeepsTransientReadFailuresAutomatic(t *testing.T) {
	cases := map[string]string{
		"detail response HTTP 429 is rate limited for 22 seconds": "rate_limited",
		"detail response HTTP 503 was not usable":                 "read_error",
		"connection refused":                                      "network",
		"detail response HTTP 403 was not usable":                 "auth",
		"detail response HTTP 404 was not usable":                 "detail",
	}
	for message, want := range cases {
		if got := candidateReadFailureClass(errors.New(message)); got != want {
			t.Errorf("message %q class=%q want=%q", message, got, want)
		}
	}
}
