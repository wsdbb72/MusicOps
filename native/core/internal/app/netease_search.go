package app

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/downloadpolicy"
)

// neteaseSearchSongs reads one NetEase search through the legacy JSON endpoint
// and falls back to the web endpoint after rate limiting or a temporary
// contract change. Both endpoints expose the same result.songs shape. An
// explicit empty songs array remains a definitive no-result; only transport,
// rate-limit and contract failures are retried through the second endpoint.
func (a *App) neteaseSearchSongs(ctx context.Context, query string, limit int) (int, []neteaseSearchItem, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return 0, nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	var bestErr error
	for _, path := range []string{"/api/search/get", "/api/search/get/web"} {
		endpoint, err := url.Parse(a.neteaseBase() + path)
		if err != nil {
			return 0, nil, err
		}
		values := endpoint.Query()
		values.Set("s", query)
		values.Set("type", "1")
		values.Set("offset", "0")
		values.Set("limit", strconv.Itoa(limit))
		endpoint.RawQuery = values.Encode()

		body, err := a.neteaseGet(ctx, endpoint.String(), a.neteaseCookie())
		if err != nil && ctx.Err() != nil {
			return 0, nil, err
		}
		if err == nil {
			code, songs, decodeErr := decodeNeteaseSearch(body)
			if decodeErr == nil && (code == 0 || code == 200) {
				return code, songs, nil
			}
			if decodeErr != nil {
				err = decodeErr
			} else {
				err = &neteaseHTTPError{operation: "搜索", status: code}
			}
		}
		if bestErr == nil || classifyNeteaseFailure(err) == downloadpolicy.ClassRateLimited {
			bestErr = err
		}
	}
	return 0, nil, bestErr
}
