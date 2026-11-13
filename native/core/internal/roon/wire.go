// Package roon speaks the Roon extension protocol (MOO over WebSocket).
//
// Roon has no REST API: a program connects to ws://<core>:9330/api, asks
// com.roonlabs.registry:1/info, registers as an extension, and from then on
// exchanges MOO/1 messages over the same socket. Registration is approved by a
// person in Roon's Settings → Extensions, so a client has to keep the socket
// open and retry until the core answers Success.
//
// This file holds the wire layer: the WebSocket handshake, client frames, MOO
// envelopes, and a reader that routes responses by Request-Id.
package roon

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// ErrPendingApproval means the core answered Unauthorized: a person has to
// enable the extension in Roon before the request can succeed.
var ErrPendingApproval = errors.New("roon: 等待在 Roon 的「设置 → 扩展」里启用 MusicOps")

// ErrClosed is returned once the socket is gone.
var ErrClosed = errors.New("roon: 连接已关闭")

// Message is one decoded MOO envelope.
type Message struct {
	Kind     string // REQUEST, COMPLETE, CONTINUE
	Status   string // Success, Unauthorized, ...
	Service  string
	Headers  map[string]string
	Body     string
	RequestID int
}

// Client is one connection to a Roon core.
type Client struct {
	conn   net.Conn
	reader *bufio.Reader
	writeMu sync.Mutex
	nextID atomic.Int64

	pendingMu sync.Mutex
	pending   map[int]chan Message

	// OnMessage receives every COMPLETE/CONTINUE that is not a direct
	// response (subscription pushes).
	OnMessage func(Message)

	closed atomic.Bool
}

// Dial connects and performs the WebSocket upgrade.
func Dial(address string, timeout time.Duration) (*Client, error) {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return nil, err
	}
	_, accept, err := handshake(conn, timeout)
	if err != nil {
		conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	response, err := readHTTPResponse(reader, timeout)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.HasPrefix(response, "HTTP/1.1 101") {
		conn.Close()
		return nil, fmt.Errorf("roon: 握手被拒绝（%s）", firstLine(response))
	}
	if !strings.Contains(strings.ToLower(response), "sec-websocket-accept: "+strings.ToLower(accept)) {
		conn.Close()
		return nil, errors.New("roon: 握手校验失败")
	}
	client := &Client{conn: conn, reader: reader, pending: map[int]chan Message{}}
	go client.readLoop()
	return client, nil
}

func handshake(conn net.Conn, timeout time.Duration) (string, string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", "", err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	sum := sha1.Sum([]byte(key + websocketGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	_ = conn.SetDeadline(time.Now().Add(timeout))
	request := "GET /api HTTP/1.1\r\nHost: " + conn.RemoteAddr().String() +
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: " + key +
		"\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		return "", "", err
	}
	_ = conn.SetDeadline(time.Time{})
	return key, accept, nil
}

func readHTTPResponse(reader *bufio.Reader, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var builder strings.Builder
	for {
		if time.Now().After(deadline) {
			return "", errors.New("roon: 握手超时")
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		builder.WriteString(line)
		if line == "\r\n" {
			return builder.String(), nil
		}
		if builder.Len() > 8192 {
			return "", errors.New("roon: 握手响应过长")
		}
	}
}

func firstLine(value string) string {
	if i := strings.IndexByte(value, '\n'); i >= 0 {
		return strings.TrimSpace(value[:i])
	}
	return strings.TrimSpace(value)
}

// Close shuts the socket down.
func (c *Client) Close() error {
	if c == nil || c.closed.Swap(true) {
		return nil
	}
	return c.conn.Close()
}

// Request sends one MOO request and waits for its response.
func (c *Client) Request(service, body string, timeout time.Duration) (Message, error) {
	if c == nil || c.closed.Load() {
		return Message{}, ErrClosed
	}
	id := int(c.nextID.Add(1))
	channel := make(chan Message, 4)
	c.pendingMu.Lock()
	c.pending[id] = channel
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()
	payload := "MOO/1 REQUEST " + service + "\nRequest-Id: " + strconv.Itoa(id) + "\n\n" + body
	if err := c.writeFrame([]byte(payload)); err != nil {
		return Message{}, err
	}
	deadline := time.After(timeout)
	for {
		select {
		case message := <-channel:
			if message.Status == "Unauthorized" || message.Status == "NotAuthorized" {
				return message, ErrPendingApproval
			}
			if message.Kind == "CONTINUE" {
				continue
			}
			if message.Status != "Success" {
				return message, fmt.Errorf("roon: %s 返回 %s", service, message.Status)
			}
			return message, nil
		case <-deadline:
			return Message{}, fmt.Errorf("roon: %s 超时", service)
		}
	}
}

func (c *Client) writeFrame(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	frame := encodeFrame(payload)
	_, err := c.conn.Write(frame)
	return err
}

// encodeFrame builds one masked client text frame. Roon messages stay under
// 64 KiB in practice, but the 16- and 64-bit length forms are handled so a
// large zone snapshot does not break the stream.
func encodeFrame(payload []byte) []byte {
	header := []byte{0x81}
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(0x80|n))
	case n <= 0xffff:
		header = append(header, 0x80|126, byte(n>>8), byte(n))
	default:
		header = append(header, 0x80|127)
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(n))
		header = append(header, size[:]...)
	}
	mask := [4]byte{0x1d, 0x2b, 0x3c, 0x4d}
	out := append(header, mask[:]...)
	for i, b := range payload {
		out = append(out, b^mask[i%4])
	}
	return out
}

// readFrame reads one frame and returns its payload. Fragmented messages are
// concatenated; control frames are answered or ignored.
func (c *Client) readFrame() ([]byte, error) {
	var payload []byte
	for {
		head := make([]byte, 2)
		if _, err := io.ReadFull(c.reader, head); err != nil {
			return nil, err
		}
		fin := head[0]&0x80 != 0
		opcode := head[0] & 0x0f
		masked := head[1]&0x80 != 0
		length := int(head[1] & 0x7f)
		switch length {
		case 126:
			ext := make([]byte, 2)
			if _, err := io.ReadFull(c.reader, ext); err != nil {
				return nil, err
			}
			length = int(binary.BigEndian.Uint16(ext))
		case 127:
			ext := make([]byte, 8)
			if _, err := io.ReadFull(c.reader, ext); err != nil {
				return nil, err
			}
			length = int(binary.BigEndian.Uint64(ext))
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(c.reader, mask[:]); err != nil {
				return nil, err
			}
		}
		chunk := make([]byte, length)
		if _, err := io.ReadFull(c.reader, chunk); err != nil {
			return nil, err
		}
		if masked {
			for i := range chunk {
				chunk[i] ^= mask[i%4]
			}
		}
		switch opcode {
		case 0x8: // close
			c.closed.Store(true)
			return nil, ErrClosed
		case 0x9: // ping
			c.writeFrameControl(0xA, chunk)
			continue
		case 0xA: // pong
			continue
		}
		payload = append(payload, chunk...)
		if fin {
			return payload, nil
		}
	}
}

func (c *Client) writeFrameControl(opcode byte, payload []byte) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	mask := [4]byte{1, 2, 3, 4}
	frame := []byte{0x80 | opcode, 0x80 | byte(len(payload))}
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	_, _ = c.conn.Write(frame)
}

func (c *Client) readLoop() {
	for {
		payload, err := c.readFrame()
		if err != nil {
			c.closed.Store(true)
			c.failPending(err)
			return
		}
		message, err := ParseMessage(string(payload))
		if err != nil {
			continue
		}
		if message.RequestID > 0 {
			c.pendingMu.Lock()
			channel := c.pending[message.RequestID]
			c.pendingMu.Unlock()
			if channel != nil {
				select {
				case channel <- message:
					continue
				default:
				}
			}
		}
		if c.OnMessage != nil {
			c.OnMessage(message)
		}
	}
}

func (c *Client) failPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for id, channel := range c.pending {
		close(channel)
		delete(c.pending, id)
	}
}

// ParseMessage decodes one MOO envelope.
func ParseMessage(raw string) (Message, error) {
	header, body, ok := strings.Cut(raw, "\n\n")
	if !ok || header == "" {
		return Message{}, errors.New("roon: 消息缺少头部")
	}
	lines := strings.Split(header, "\n")
	parts := strings.SplitN(lines[0], " ", 3)
	if len(parts) < 2 || parts[0] != "MOO/1" {
		return Message{}, errors.New("roon: 不是 MOO/1")
	}
	message := Message{Kind: parts[1], Headers: map[string]string{}, Body: body}
	if len(parts) == 3 {
		message.Status = parts[2]
		if message.Kind == "REQUEST" {
			message.Service = parts[2]
			message.Status = ""
		}
	}
	for _, line := range lines[1:] {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		message.Headers[name] = value
		if name == "Request-Id" {
			message.RequestID, _ = strconv.Atoi(value)
		}
	}
	return message, nil
}
