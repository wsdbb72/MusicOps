package trackersearch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

// ReadDetail fetches one allowlisted detail document. Download paths are rejected.
func (c *Client) ReadDetail(ctx context.Context, q Query, path string, values url.Values) ([]byte, error) {
	if _, ok := Hosts[q.Site]; !ok || strings.ContainsAny(q.Cookie, "\r\n") || strings.TrimSpace(q.Cookie) == "" {
		return nil, fmt.Errorf("%w: detail", dataengine.ErrInvalidRequest)
	}
	if !allowedDetail(q.Site, path, values) {
		return nil, fmt.Errorf("%w: detail path", dataengine.ErrInvalidRequest)
	}
	endpoint, err := endpointURL(q)
	if err != nil {
		return nil, err
	}
	endpoint.Path = path
	endpoint.RawQuery = values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", q.Cookie)
	if q.Synthetic {
		req.Header.Set("X-MusicOps-Site", q.Site)
	}
	response, err := c.doer(q.Timeout).Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("detail response was not usable")
	}
	// A detail page is a read-only GET, so a browser challenge can be retried
	// once through the solver. An expired session answers 403 without the
	// challenge markers and is never replayed: a browser would only produce
	// the login form.
	if looksChallenged(response.StatusCode, body) {
		if solved, ok := c.solverGet(ctx, q, endpoint.String()); ok {
			return solved, nil
		}
		return nil, fmt.Errorf("detail page required a browser challenge that was not solved")
	}
	if response.StatusCode != http.StatusOK || len(body) == 0 {
		return nil, fmt.Errorf("detail response was not usable")
	}
	return body, nil
}

// ReadAccount fetches the Gazelle account document used for token balance.
// It rejects download parameters and does not follow redirects.
func (c *Client) ReadAccount(ctx context.Context, q Query, values url.Values) ([]byte, int, string, error) {
	if !isTokenSite(q.Site) || strings.ContainsAny(q.Cookie, "\r\n") || strings.TrimSpace(q.Cookie) == "" {
		return nil, 0, "", fmt.Errorf("%w: account", dataengine.ErrInvalidRequest)
	}
	if !allowedAccount(values) {
		return nil, 0, "", fmt.Errorf("%w: account path", dataengine.ErrInvalidRequest)
	}
	endpoint, err := endpointURL(q)
	if err != nil {
		return nil, 0, "", err
	}
	endpoint.Path = "/ajax.php"
	endpoint.RawQuery = values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, 0, "", errorsNewAccount()
	}
	req.Header.Set("Cookie", q.Cookie)
	req.Header.Set("Accept", "application/json")
	if q.Synthetic {
		req.Header.Set("X-MusicOps-Site", q.Site)
	}
	response, err := c.doer(q.Timeout).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, "", ctx.Err()
		}
		return nil, 0, "", errorsNewAccount()
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return nil, response.StatusCode, response.Header.Get("Retry-After"), errorsNewAccount()
	}
	if len(body) > maxBody {
		return nil, response.StatusCode, response.Header.Get("Retry-After"), errorsNewAccount()
	}
	if looksChallenged(response.StatusCode, body) {
		if solved, ok := c.solverGet(ctx, q, endpoint.String()); ok {
			return solved, http.StatusOK, "", nil
		}
		// The balance is unknown; the caller keeps the row unsettled rather
		// than reading a challenge page as a zero balance.
		return nil, response.StatusCode, response.Header.Get("Retry-After"), errorsNewAccount()
	}
	return body, response.StatusCode, response.Header.Get("Retry-After"), nil
}

// isTokenSite lists the sites whose account document carries a token balance.
// It mirrors tokenledger.IsTokenSite; this package stays a leaf and does not
// import the ledger.
func isTokenSite(site string) bool {
	switch site {
	case "dicmusic", "orpheus", "redacted":
		return true
	default:
		return false
	}
}

func allowedAccount(values url.Values) bool {
	action := values.Get("action")
	for key := range values {
		switch key {
		case "action", "id":
		default:
			return false
		}
	}
	switch action {
	case "index":
		return len(values) == 1
	case "user":
		return len(values) == 2 && values.Get("id") != ""
	default:
		return false
	}
}

func errorsNewAccount() error { return fmt.Errorf("account read failed") }

func allowedDetail(site, path string, values url.Values) bool {
	keys := map[string]bool{}
	for key := range values {
		keys[key] = true
	}
	switch {
	case site == "open.cd" && path == "/details.php" && keys["id"] && len(keys) == 1:
		return true
	case site != "open.cd" && path == "/ajax.php" && values.Get("action") == "torrent" && keys["id"] && len(keys) == 2:
		return true
	case site != "open.cd" && path == "/torrents.php" && keys["id"] && keys["torrentid"] && len(keys) == 2:
		return true
	// The user page is the documented fallback for a token balance: some
	// sites (Redacted) only render the count there, not in the JSON index.
	case site != "open.cd" && path == "/user.php" && keys["id"] && len(keys) == 1 && positiveID.MatchString(values.Get("id")):
		return true
	default:
		return false
	}
}
