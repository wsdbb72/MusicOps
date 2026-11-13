package roon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestParseMessageReadsMooEnvelope(t *testing.T) {
	raw := "MOO/1 COMPLETE Success\nRequest-Id: 7\nContent-Type: application/json\n\n{\"zones\":[]}"
	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.Kind != "COMPLETE" || message.Status != "Success" || message.RequestID != 7 {
		t.Fatal(message)
	}
	if message.Headers["Content-Type"] != "application/json" || message.Body != `{"zones":[]}` {
		t.Fatal(message)
	}
	if _, err := ParseMessage("not a moo message"); err == nil {
		t.Fatal("a non-MOO payload was accepted")
	}
}

func TestParseRequestCarriesTheService(t *testing.T) {
	message, err := ParseMessage("MOO/1 REQUEST com.roonlabs.ping:1/ping\nRequest-Id: 3\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if message.Kind != "REQUEST" || message.Service != "com.roonlabs.ping:1/ping" || message.RequestID != 3 {
		t.Fatal(message)
	}
}

// Roon pings the extension and drops the connection when a ping goes
// unanswered; that is what made an earlier version look registered but dead.
// The fake core below pings before it answers register, so the client has to
// keep its read loop alive while a request is in flight.
func TestClientAnswersPingWhileRegistering(t *testing.T) {
	upgrader := websocket.Upgrader{}
	seen := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		send := func(payload string) {
			_ = conn.WriteMessage(websocket.BinaryMessage, []byte(payload))
		}
		_, first, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if !strings.Contains(string(first), "com.roonlabs.registry:1/info") {
			seen <- "unexpected first request: " + string(first)
			return
		}
		send("MOO/1 COMPLETE Success\nRequest-Id: 1\nContent-Type: application/json\n\n" +
			`{"core_id":"core-1","display_name":"Test Core","display_version":"2.0"}`)
		// The core pings immediately after the info answer.
		send("MOO/1 REQUEST com.roonlabs.ping:1/ping\nRequest-Id: 99\n\n")
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			text := string(raw)
			seen <- text
			if strings.Contains(text, "com.roonlabs.ping:1/ping") {
				if !strings.Contains(text, "COMPLETE Success") || !strings.Contains(text, "Request-Id: 99") {
					seen <- "bad ping answer: " + text
				}
				continue
			}
			if strings.Contains(text, "com.roonlabs.registry:1/register") {
				var body struct {
					Required []string `json:"required_services"`
					Provided []string `json:"provided_services"`
				}
				_, jsonBody, _ := strings.Cut(text, "\n\n")
				if json.Unmarshal([]byte(jsonBody), &body) != nil {
					seen <- "register body was not json"
					return
				}
				if len(body.Required) != 1 || body.Required[0] != TransportV2 {
					seen <- "required services: " + strings.Join(body.Required, ",")
				}
				joined := strings.Join(body.Provided, ",")
				if !strings.Contains(joined, "com.roonlabs.ping:1") || !strings.Contains(joined, "com.roonlabs.pairing:1") {
					seen <- "provided services: " + joined
				}
				send("MOO/1 CONTINUE Registered\nRequest-Id: 2\nContent-Type: application/json\n\n" +
					`{"core_id":"core-1","token":"token-1","provided_services":["com.roonlabs.transport:2"]}`)
				return
			}
		}
	}))
	defer server.Close()

	client, err := Dial(strings.TrimPrefix(server.URL, "http://"), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	info, err := client.Info()
	if err != nil || info.CoreID != "core-1" {
		t.Fatal(info, err)
	}
	registration, err := client.Register(RegisterOptions{
		ExtensionID: "com.musicops.desktop", DisplayName: "MusicOps", DisplayVersion: "test",
		RequiredServices: []string{TransportV2},
		ProvidedServices: []string{"com.roonlabs.ping:1", "com.roonlabs.pairing:1"},
	}, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if registration.Token != "token-1" {
		t.Fatal(registration)
	}
	close(seen)
	for message := range seen {
		if strings.HasPrefix(message, "unexpected") || strings.HasPrefix(message, "bad") ||
			strings.HasPrefix(message, "required") || strings.HasPrefix(message, "provided") ||
			strings.HasPrefix(message, "register body") {
			t.Fatal(message)
		}
	}
}
