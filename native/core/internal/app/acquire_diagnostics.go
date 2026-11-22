package app

import "strings"

const candidateDiagnosticLimit = 260

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
