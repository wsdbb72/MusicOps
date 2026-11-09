// Package musicbrainz is a read-only client for the MusicBrainz web service
// (https://musicbrainz.org/ws/2/, JSON, no auth required).
//
// It mirrors the MusicBrainz half of the Python CatalogResolver in
// src/musicops/providers/catalog.py (_mb_search, _mb_detail, _mb_recording,
// _candidate, _rank): MBID validation, Lucene query escaping, artist-credit
// parsing, score-ordered search results, and a tracklist-complete rule that
// requires every track to have a title and a positive length.
//
// Discogs is deliberately out of scope: it needs a user token, which this
// package never handles.
//
// The package is a leaf: it uses only the standard library and imports no
// other MusicOps package, so callers can convert Observation into
// releaseidentity.Observation without a dependency cycle.
package musicbrainz

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBaseURL is the public MusicBrainz web service root.
	DefaultBaseURL = "https://musicbrainz.org"
	// DefaultUserAgent is sent on every request. MusicBrainz requires a
	// descriptive User-Agent; a missing or generic one risks being blocked.
	DefaultUserAgent = "MusicOps/0.4.32 ( https://github.com/wsdbb72/MusicOps )"

	// rateLimitInterval enforces the MusicBrainz policy of at most one
	// request per second. Callers that exceed it wait instead of failing.
	rateLimitInterval = time.Second
	// maxResponseBytes caps a single response body, matching the Python
	// client's 2 MB limit.
	maxResponseBytes = 2_000_000
	// defaultTimeout bounds a single HTTP round trip.
	defaultTimeout = 15 * time.Second
	// maxRetryAfter caps how long a Retry-After wait may last; the server
	// may ask for hours, but this client never blocks that long on one call.
	maxRetryAfter = 60 * time.Second
	// searchLimit is the number of releases requested per search.
	searchLimit = 25
)

// ErrEmptyQuery is returned when a search has no artist, album or title.
var ErrEmptyQuery = errors.New("musicbrainz: search needs an artist, album or title")

// ErrInvalidMBID is returned when an MBID is not a valid UUID-shaped
// MusicBrainz identifier.
var ErrInvalidMBID = errors.New("musicbrainz: invalid MBID")

// RateLimitError is returned when the server still answers 429 after the
// one permitted retry. RetryAfter is the value the server asked for.
type RateLimitError struct {
	RetryAfter time.Duration
	StatusCode int
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("musicbrainz: rate limited (HTTP %d, retry after %s)", e.StatusCode, e.RetryAfter)
}

// ServerError is returned for a 5xx response after the one permitted retry.
type ServerError struct {
	StatusCode int
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("musicbrainz: server error (HTTP %d)", e.StatusCode)
}

// ClientError is returned for a 4xx response (other than a retried 429).
// These are not retried: the request itself is wrong or the record is gone.
type ClientError struct {
	StatusCode int
}

func (e *ClientError) Error() string {
	return fmt.Sprintf("musicbrainz: request rejected (HTTP %d)", e.StatusCode)
}

// ParseError is returned when a 200 response is not usable JSON, is not a
// JSON object, or disagrees with the request (for example a detail lookup
// whose id differs from the MBID asked for).
type ParseError struct {
	Message string
	Err     error
}

func (e *ParseError) Error() string {
	if e.Err != nil {
		return "musicbrainz: unusable response: " + e.Message + ": " + e.Err.Error()
	}
	return "musicbrainz: unusable response: " + e.Message
}

func (e *ParseError) Unwrap() error { return e.Err }

// Client talks to the MusicBrainz web service. The zero value is usable
// after NewClient, but the exported fields may be set directly instead:
// BaseURL and UserAgent fall back to their defaults when empty, HTTPClient
// falls back to a client with a 15s timeout, and Now falls back to time.Now.
//
// Now is injectable so tests can drive the rate limiter with a fake clock.
// The limiter also sleeps through an unexported sleep hook (time.Sleep by
// default) so tests in this package can advance the fake clock instead of
// waiting a real second.
type Client struct {
	BaseURL    string
	UserAgent  string
	HTTPClient *http.Client
	Now        func() time.Time

	sleep func(time.Duration)

	mu       sync.Mutex
	lastCall time.Time
}

// NewClient returns a Client with the defaults applied.
func NewClient() *Client {
	return &Client{
		BaseURL:    DefaultBaseURL,
		UserAgent:  DefaultUserAgent,
		HTTPClient: &http.Client{Timeout: defaultTimeout},
		Now:        time.Now,
		sleep:      time.Sleep,
	}
}

func (c *Client) baseURL() string {
	if strings.TrimSpace(c.BaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	}
	return DefaultBaseURL
}

func (c *Client) userAgent() string {
	if strings.TrimSpace(c.UserAgent) != "" {
		return c.UserAgent
	}
	return DefaultUserAgent
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: defaultTimeout}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) doSleep(d time.Duration) {
	if c.sleep != nil {
		c.sleep(d)
		return
	}
	time.Sleep(d)
}

// waitForRateLimit blocks until at least rateLimitInterval has passed since
// the previous request. It holds the mutex while waiting so concurrent
// callers serialize instead of bursting. The timestamp is re-read after the
// wait so a fake clock advanced by the sleep is honoured.
func (c *Client) waitForRateLimit() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastCall.IsZero() {
		c.lastCall = c.now()
		return
	}
	if wait := rateLimitInterval - c.now().Sub(c.lastCall); wait > 0 {
		c.doSleep(wait)
	}
	c.lastCall = c.now()
}

// get performs one GET against path (relative to BaseURL) with the given
// query parameters, applying the rate limit and the retry policy:
//
//   - 429: wait the Retry-After value (1s when absent or unparseable,
//     capped at 60s), then retry once.
//   - 5xx or a transport error: wait one second, then retry once.
//   - any other 4xx: no retry.
//   - malformed JSON is the caller's problem: get returns the raw body and
//     the caller decodes it, so a decode failure is a ParseError there.
func (c *Client) get(path string, params url.Values) ([]byte, error) {
	params.Set("fmt", "json")
	endpoint := c.baseURL() + path + "?" + params.Encode()

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		c.waitForRateLimit()

		req, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("musicbrainz: building request: %w", err)
		}
		req.Header.Set("User-Agent", c.userAgent())
		req.Header.Set("Accept", "application/json")

		resp, err := c.httpClient().Do(req)
		if err != nil {
			lastErr = fmt.Errorf("musicbrainz: request failed: %w", err)
			c.doSleep(rateLimitInterval)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("musicbrainz: reading response: %w", readErr)
		}
		if len(body) > maxResponseBytes {
			return nil, &ParseError{Message: "response exceeds size limit"}
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			return body, nil
		case resp.StatusCode == http.StatusTooManyRequests:
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), c.now())
			if attempt == 0 {
				c.doSleep(retryAfter)
				lastErr = &RateLimitError{RetryAfter: retryAfter, StatusCode: resp.StatusCode}
				continue
			}
			return nil, &RateLimitError{RetryAfter: retryAfter, StatusCode: resp.StatusCode}
		case resp.StatusCode >= 500:
			if attempt == 0 {
				lastErr = &ServerError{StatusCode: resp.StatusCode}
				c.doSleep(rateLimitInterval)
				continue
			}
			return nil, &ServerError{StatusCode: resp.StatusCode}
		default:
			// 4xx (including 404) and anything else: the request is at
			// fault, retrying cannot help.
			return nil, &ClientError{StatusCode: resp.StatusCode}
		}
	}
	// Only reachable when both attempts failed at the transport level.
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("musicbrainz: request failed")
}

// parseRetryAfter converts a Retry-After header to a wait duration. It
// accepts a number of seconds or an HTTP date, clamps the result to
// [0, maxRetryAfter], and returns one second when the header is absent or
// unparseable.
func parseRetryAfter(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return time.Second
	}
	if seconds, err := strconv.ParseFloat(header, 64); err == nil {
		wait := time.Duration(seconds * float64(time.Second))
		return clampDuration(wait, 0, maxRetryAfter)
	}
	if when, err := http.ParseTime(header); err == nil {
		return clampDuration(time.Until(when), 0, maxRetryAfter)
	}
	return time.Second
}

func clampDuration(d, low, high time.Duration) time.Duration {
	if d < low {
		return low
	}
	if d > high {
		return high
	}
	return d
}

var mbidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// normalizeMBID lower-cases an MBID and rejects anything that is not a
// UUID-shaped MusicBrainz identifier, mirroring Python's _mbid.
func normalizeMBID(value string) (string, error) {
	lowered := strings.ToLower(strings.TrimSpace(value))
	if !mbidPattern.MatchString(lowered) {
		return "", fmt.Errorf("%w: %q", ErrInvalidMBID, value)
	}
	return lowered, nil
}

// escapeLuceneTerm quotes one search term the way Python's _lucene does:
// Lucene special characters (+ - ! ( ) { } [ ] ^ " ~ * ? : \ /) and the
// && / || operators are backslash-escaped, and the whole term is wrapped
// in double quotes so multi-word values stay one phrase.
func escapeLuceneTerm(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	runes := []rune(value)
	for i := 0; i < len(runes); i++ {
		if i+1 < len(runes) && runes[i] == '&' && runes[i+1] == '&' {
			b.WriteString(`\&&`)
			i++
			continue
		}
		if i+1 < len(runes) && runes[i] == '|' && runes[i+1] == '|' {
			b.WriteString(`\||`)
			i++
			continue
		}
		if strings.ContainsRune(`+-!(){}[]^"~*?:\/`, runes[i]) {
			b.WriteByte('\\')
		}
		b.WriteRune(runes[i])
	}
	b.WriteByte('"')
	return b.String()
}

// buildQuery builds the Lucene query for a release search from the inputs,
// mirroring Python's _mb_search (release:<album> AND artist:<artist>), with
// the recording title added as a recording: clause when one is given. It
// returns ErrEmptyQuery when every input is empty; a query with no terms
// must never be sent, because the service would answer with unrelated
// releases that could be mistaken for a match.
func buildQuery(artist, album, title string) (string, error) {
	var parts []string
	if strings.TrimSpace(album) != "" {
		parts = append(parts, "release:"+escapeLuceneTerm(strings.TrimSpace(album)))
	}
	if strings.TrimSpace(title) != "" {
		parts = append(parts, "recording:"+escapeLuceneTerm(strings.TrimSpace(title)))
	}
	if strings.TrimSpace(artist) != "" {
		parts = append(parts, "artist:"+escapeLuceneTerm(strings.TrimSpace(artist)))
	}
	if len(parts) == 0 {
		return "", ErrEmptyQuery
	}
	return strings.Join(parts, " AND "), nil
}

// artistCreditEntry is one element of a MusicBrainz artist-credit array.
// Plain strings in that array are join phrases (" & ", " feat. "), not
// artists, so callers must skip them before decoding.
type artistCreditEntry struct {
	Name   string `json:"name"`
	Artist *struct {
		Name string `json:"name"`
	} `json:"artist"`
}

// parseArtistCredit extracts deduplicated artist names from a raw
// artist-credit array, mirroring Python's _credits: string elements are
// join phrases and are skipped, and each object contributes its name, or
// its nested artist name when the object has none.
func parseArtistCredit(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	var artists []string
	seen := make(map[string]bool)
	for _, item := range items {
		trimmed := strings.TrimSpace(string(item))
		if len(trimmed) == 0 || trimmed[0] == '"' {
			continue
		}
		var entry artistCreditEntry
		if err := json.Unmarshal(item, &entry); err != nil {
			continue
		}
		name := strings.TrimSpace(entry.Name)
		if name == "" && entry.Artist != nil {
			name = strings.TrimSpace(entry.Artist.Name)
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		artists = append(artists, name)
	}
	return artists
}
