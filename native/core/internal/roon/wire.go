// Package roon speaks the Roon extension protocol (MOO over WebSocket).
//
// Roon has no REST API: a program connects to ws://<core>:9330/api, asks
// com.roonlabs.registry:1/info, registers as an extension, and from then on
// exchanges MOO/1 messages over the same socket. Registration is approved by a
// person in Roon's Settings → Extensions, so a client keeps the socket open and
// waits for the core to answer Success.
//
// The transport is gorilla/websocket. A hand-rolled frame reader was tried
// first and Roon never answered the first request — exactly the bug a
// maintained implementation does not have. What stays custom here is the MOO
// envelope and the Request-Id routing.
package roon

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ErrPendingApproval means the core answered Unauthorized: a person has to
// enable the extension in Roon before the request can succeed.
var ErrPendingApproval = errors.New("roon: 等待在 Roon 的「设置 → 扩展」里启用 MusicOps")

// ErrClosed is returned once the socket is gone.
var ErrClosed = errors.New("roon: 连接已关闭")

// Message is one decoded MOO envelope.
type Message struct {
	Kind      string
	Status    string
	Service   string
	Headers   map[string]string
	Body      string
	RequestID int
}

// Client is one connection to a Roon core.
type Client struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	nextID  atomic.Int64

	pendingMu sync.Mutex
	pending   map[int]chan Message

	// OnMessage receives every COMPLETE/CONTINUE that is not a direct
	// response (subscription pushes).
	OnMessage func(Message)

	closed atomic.Bool
}

// Dial connects to ws://<address>/api.
func Dial(address string, timeout time.Duration) (*Client, error) {
	dialer := websocket.Dialer{HandshakeTimeout: timeout, ReadBufferSize: 64 << 10, WriteBufferSize: 64 << 10}
	conn, _, err := dialer.Dial("ws://"+address+"/api", nil)
	if err != nil {
		return nil, err
	}
	client := &Client{conn: conn, pending: map[int]chan Message{}}
	go client.readLoop()
	return client, nil
}

// Close shuts the socket down.
func (c *Client) Close() error {
	if c == nil || c.closed.Swap(true) {
		return nil
	}
	return c.conn.Close()
}

// Request sends one MOO request and waits for its response. A CONTINUE frame
// belongs to the same request and is skipped; Unauthorized surfaces as
// ErrPendingApproval so callers can show "waiting for approval" instead of a
// generic failure.
func (c *Client) Request(service, body string, timeout time.Duration) (Message, error) {
	if c == nil || c.closed.Load() {
		return Message{}, ErrClosed
	}
	id := int(c.nextID.Add(1))
	channel := make(chan Message, 8)
	c.pendingMu.Lock()
	c.pending[id] = channel
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()
	payload := "MOO/1 REQUEST " + service + "\nRequest-Id: " + strconv.Itoa(id) + "\n\n" + body
	// Roon accepts binary frames only: a text frame is silently ignored and
	// the first request times out, which is what hid this for a while.
	c.writeMu.Lock()
	err := c.conn.WriteMessage(websocket.BinaryMessage, []byte(payload))
	c.writeMu.Unlock()
	if err != nil {
		return Message{}, err
	}
	deadline := time.After(timeout)
	for {
		select {
		case message, open := <-channel:
			if !open {
				return Message{}, ErrClosed
			}
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

// RequestFirst returns the first COMPLETE or CONTINUE message for a request.
// Registration answers CONTINUE Registered (with the token) and never sends a
// COMPLETE, so a call that waits for COMPLETE times out on a successful
// registration.
func (c *Client) RequestFirst(service, body string, timeout time.Duration) (Message, error) {
	if c == nil || c.closed.Load() {
		return Message{}, ErrClosed
	}
	id := int(c.nextID.Add(1))
	channel := make(chan Message, 8)
	c.pendingMu.Lock()
	c.pending[id] = channel
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()
	header := "MOO/1 REQUEST " + service + "\nRequest-Id: " + strconv.Itoa(id) + "\n"
	if body != "" {
		header += "Content-Type: application/json\nContent-Length: " + strconv.Itoa(len(body)) + "\n"
	}
	c.writeMu.Lock()
	err := c.conn.WriteMessage(websocket.BinaryMessage, []byte(header+"\n"+body))
	c.writeMu.Unlock()
	if err != nil {
		return Message{}, err
	}
	select {
	case message, open := <-channel:
		if !open {
			return Message{}, ErrClosed
		}
		if message.Status == "Unauthorized" || message.Status == "NotAuthorized" {
			return message, ErrPendingApproval
		}
		if message.Status != "Success" && message.Status != "Registered" {
			return message, fmt.Errorf("roon: %s 返回 %s", service, message.Status)
		}
		return message, nil
	case <-time.After(timeout):
		return Message{}, fmt.Errorf("roon: %s 超时", service)
	}
}

func (c *Client) readLoop() {
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			c.closed.Store(true)
			c.failPending()
			return
		}
		message, err := ParseMessage(string(data))
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

func (c *Client) failPending() {
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
		if parts[1] == "REQUEST" {
			message.Service = parts[2]
		} else {
			message.Status = parts[2]
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
