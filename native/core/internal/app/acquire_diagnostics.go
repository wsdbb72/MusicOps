package app

import (
	"errors"
	"net"
	"regexp"
	"strings"
)

const candidateDiagnosticLimit = 260

var candidateHTTP5xx = regexp.MustCompile(`(?i)\bhttp 5[0-9][0-9]\b`)

// candidateFailureMessage preserves a useful HTTP or parsing reason while
// removing credentials and bounding what reaches the workflow record.
func candidateFailureMessage(prefix string, err error) string {
	prefix = strings.TrimSpace(prefix)
	if err == nil {
		return prefix
	}
	detail := scrub(strings.Join(strings.Fields(err.Error()), " "))
	if detail == "" {
		return prefix
	}
	runes := []rune(detail)
	if len(runes) > candidateDiagnosticLimit {
		detail = string(runes[:candidateDiagnosticLimit]) + "…"
	}
	if prefix == "" {
		return detail
	}
	return prefix + "：" + detail
}

// candidateReadFailureClass separates failures that are safe to retry from
// failures that should make the caller try another candidate. These requests
// are read-only: a rate limit, timeout, connection failure, or server error
// cannot have spent a token or started an ordinary tracker download.
func candidateReadFailureClass(err error) string {
	if err == nil {
		return ""
	}
	return candidateReadFailureClassText(err.Error(), err)
}

func candidateReadFailureClassText(text string, cause error) string {
	detail := strings.ToLower(strings.TrimSpace(text))
	switch {
	case strings.Contains(detail, "http 429"), strings.Contains(detail, "rate limited"),
		strings.Contains(detail, "too many requests"):
		return "rate_limited"
	case strings.Contains(detail, "http 401"), strings.Contains(detail, "http 403"),
		strings.Contains(detail, "login required"), strings.Contains(detail, "not authenticated"):
		return "auth"
	case errors.Is(cause, net.ErrClosed), strings.Contains(detail, "connection refused"),
		strings.Contains(detail, "connection reset"), strings.Contains(detail, "no such host"),
		strings.Contains(detail, "tls handshake"), strings.Contains(detail, "timeout"),
		strings.Contains(detail, "deadline exceeded"):
		return "network"
	case candidateHTTP5xx.MatchString(detail), strings.Contains(detail, "server unavailable"):
		return "read_error"
	default:
		return "detail"
	}
}
