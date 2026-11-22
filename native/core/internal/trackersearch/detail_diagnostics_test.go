package trackersearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestReadDetailReportsTheHTTPStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := &Client{Doer: server.Client()}
	_, err := client.ReadDetail(context.Background(), Query{
		Site: "dicmusic", Cookie: "session=a", Synthetic: true, BaseURL: server.URL,
	}, "/ajax.php", url.Values{"action": {"torrent"}, "id": {"11"}})
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("err=%v", err)
	}
}
