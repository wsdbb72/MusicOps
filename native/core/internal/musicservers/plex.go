package musicservers

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// plexMaxResponseBytes mirrors the 2,000,000 byte cap in the Python client.
const plexMaxResponseBytes = 2_000_000

var plexSectionKeyPattern = regexp.MustCompile(`^[0-9]{1,12}$`)

// Section is one Plex music library (a type="artist" Directory in the
// /library/sections response).
type Section struct {
	// ID is the Plex section key.
	ID string `json:"id"`
	// Name is the section title.
	Name string `json:"name"`
	// Paths lists the Location paths configured for the section.
	Paths []string `json:"paths"`
}

// ValidatePlexURL mirrors plex.validate: the URL must be a clean HTTP(S)
// service address with no userinfo, query or fragment, and plain HTTP is
// only allowed for local-service hosts (loopback, LAN, Docker internal);
// a public host must use HTTPS.
func ValidatePlexURL(raw string) error {
	if strings.TrimSpace(raw) == "" || strings.ContainsAny(raw, " \t\r\n") {
		return fmt.Errorf("musicservers: Plex URL must be a clean HTTP(S) service address: %w", ErrInvalidInput)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("musicservers: Plex URL must be a clean HTTP(S) service address: %w: %w", ErrInvalidInput, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("musicservers: Plex URL must be a clean HTTP(S) service address: %w", ErrInvalidInput)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("musicservers: Plex URL must not contain credentials, a query or a fragment: %w", ErrInvalidInput)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("musicservers: Plex URL must be a clean HTTP(S) service address: %w", ErrInvalidInput)
	}
	if scheme == "http" && !localServiceHost(host) {
		return fmt.Errorf("musicservers: public Plex must use HTTPS; HTTP is only allowed for local or LAN hosts: %w", ErrInvalidInput)
	}
	return nil
}

func plexHeaders(token string) map[string]string {
	return map[string]string{
		"X-Plex-Token": token,
		"Accept":       "application/xml",
	}
}

// PlexSections mirrors plex.sections: it fetches GET {url}/library/sections
// with the X-Plex-Token header and returns the artist-type sections.
// A 401/403 maps to ErrAuthFailed, any other non-200 status to
// ErrAccessBlocked, and an oversized, DOCTYPE-bearing or unparsable
// response to ErrInvalidPayload.
func PlexSections(ctx context.Context, baseURL, token string) ([]Section, error) {
	if err := ValidatePlexURL(baseURL); err != nil {
		return nil, err
	}
	if token == "" {
		return nil, fmt.Errorf("musicservers: Plex X-Plex-Token is required: %w", ErrAuthRequired)
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/library/sections"
	body, status, err := httpGet(ctx, endpoint, plexHeaders(token), plexMaxResponseBytes)
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, fmt.Errorf("musicservers: Plex token is invalid or lacks permission: %w", ErrAuthFailed)
	case status != http.StatusOK:
		return nil, fmt.Errorf("musicservers: Plex rejected the request (status %d): %w", status, ErrAccessBlocked)
	}
	if len(body) > plexMaxResponseBytes || bytes.Contains(bytes.ToUpper(body), []byte("<!DOCTYPE")) {
		return nil, fmt.Errorf("musicservers: Plex returned an invalid library response: %w", ErrInvalidPayload)
	}
	return parsePlexSections(body)
}

// PlexScan mirrors plex.scan: it first reads the section list and verifies
// sectionID is one of the accessible music libraries (otherwise
// ErrInvalidInput), then requests GET
// /library/sections/<sectionID>/refresh. A refresh answered with
// 200/201/202/204 is success.
func PlexScan(ctx context.Context, baseURL, token, sectionID string) error {
	sections, err := PlexSections(ctx, baseURL, token)
	if err != nil {
		return err
	}
	found := false
	for _, section := range sections {
		if section.ID == sectionID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("musicservers: Plex section %q is not an accessible music library: %w", sectionID, ErrInvalidInput)
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/library/sections/" + sectionID + "/refresh"
	_, status, err := httpGet(ctx, endpoint, plexHeaders(token), 0)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("musicservers: Plex token is invalid or lacks permission: %w", ErrAuthFailed)
	default:
		return fmt.Errorf("musicservers: Plex rejected the scan request (status %d): %w", status, ErrAccessBlocked)
	}
}

type plexMediaContainer struct {
	Directories []plexDirectory `xml:"Directory"`
}

type plexDirectory struct {
	Key       string         `xml:"key,attr"`
	Type      string         `xml:"type,attr"`
	Title     string         `xml:"title,attr"`
	Locations []plexLocation `xml:"Location"`
}

type plexLocation struct {
	Path string `xml:"path,attr"`
}

// parsePlexSections decodes a /library/sections body. The root element must
// be MediaContainer; only direct Directory children with type="artist" and
// a 1-12 digit key are kept, with the key as the section ID, the title as
// the name and the Location paths in order.
func parsePlexSections(body []byte) ([]Section, error) {
	invalid := func(err error) ([]Section, error) {
		if err == nil {
			return nil, fmt.Errorf("musicservers: Plex returned an invalid library format: %w", ErrInvalidPayload)
		}
		return nil, fmt.Errorf("musicservers: Plex returned an invalid library format: %w: %w", ErrInvalidPayload, err)
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if err != nil {
			return invalid(err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local != "MediaContainer" {
			return invalid(nil)
		}
		var container plexMediaContainer
		if err := dec.DecodeElement(&container, &start); err != nil {
			return invalid(err)
		}
		sections := make([]Section, 0, len(container.Directories))
		for _, dir := range container.Directories {
			if dir.Type != "artist" || !plexSectionKeyPattern.MatchString(dir.Key) {
				continue
			}
			paths := make([]string, 0, len(dir.Locations))
			for _, loc := range dir.Locations {
				paths = append(paths, loc.Path)
			}
			sections = append(sections, Section{ID: dir.Key, Name: dir.Title, Paths: paths})
		}
		return sections, nil
	}
}
