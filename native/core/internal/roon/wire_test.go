package roon

import "testing"

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
