package torrentfetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchReportsHTTPStatusAndHTMLReason(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "status", status: http.StatusServiceUnavailable, body: "unavailable", want: "HTTP 503"},
		{name: "html", status: http.StatusOK, body: "<html>login</html>", want: "HTML"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			_, err := Fetch(context.Background(), Request{
				Site: "open.cd", Resource: "11", Cookie: "session=a", Synthetic: true,
				BaseURL: server.URL, Directory: t.TempDir(),
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v want %q", err, test.want)
			}
		})
	}
}
