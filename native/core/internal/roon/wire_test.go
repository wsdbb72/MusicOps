package roon

import (
	"bufio"
	"bytes"
	"testing"
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

func TestEncodeFrameMasksAndSizes(t *testing.T) {
	small := encodeFrame([]byte("hi"))
	if small[0] != 0x81 || small[1] != 0x82 {
		t.Fatal(small[:2])
	}
	// A 300-byte payload uses the 16-bit length form.
	large := encodeFrame(bytes.Repeat([]byte("a"), 300))
	if large[1] != 0x80|126 || int(large[2])<<8|int(large[3]) != 300 {
		t.Fatal(large[:4])
	}
	// The mask is applied: unmasking gives the payload back.
	mask := large[4:8]
	body := large[8:]
	for i := range body {
		body[i] ^= mask[i%4]
	}
	if string(body) != string(bytes.Repeat([]byte("a"), 300)) {
		t.Fatal("mask round trip failed")
	}
}

func TestUnmaskedServerFrameIsRead(t *testing.T) {
	// A server frame is not masked; the reader must accept it.
	payload := []byte("MOO/1 COMPLETE Success\nRequest-Id: 1\n\n{}")
	frame := append([]byte{0x81, byte(len(payload))}, payload...)
	client := &Client{reader: bufio.NewReader(bytes.NewReader(frame))}
	got, err := client.readFrame()
	if err != nil || string(got) != string(payload) {
		t.Fatal(err, string(got))
	}
}
