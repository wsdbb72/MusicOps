// Package roon speaks the Roon extension protocol (MOO over WebSocket).
//
// Roon has no REST API: a program connects to ws://<core>:9330/api, asks
// com.roonlabs.registry:1/info, registers as an extension, and from then on
// exchanges MOO/1 messages over the same socket. Registration is approved by a
// person in Roon's Settings → Extensions, so a client keeps the socket open and
// waits for the core to answer Registered.
//
// Two details cost real time and are guarded here:
//   - Roon ignores WebSocket text frames; every message must be binary.
//   - Roon sends com.roonlabs.ping:1/ping requests to the extension and drops a
//     connection whose pings go unanswered. Every REQUEST must be answered.
package roon

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ErrPendingApproval means the core is waiting for a person to enable the
// extension in Roon.
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

	// PairedCoreID is what this extension reports to Roon's pairing service.
	// It is set right after info is read.
	PairedCoreID atomic.Value

	// transport is the transport service the core granted (see
	// TransportService).
	transport atomic.Value

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
	client.PairedCoreID.Store("")
	go client.readLoop()
	return client, nil
}

// Alive reports whether the socket is still open. A core that restarts drops
// the connection silently, so the caller has to notice.
func (c *Client) Alive() bool {
	return c != nil && !c.closed.Load()
}

// Close shuts the socket down.
func (c *Client) Close() error {
	if c == nil || c.closed.Swap(true) {
		return nil
	}
	return c.conn.Close()
}

// Request sends one MOO request and waits for a COMPLETE Success. A CONTINUE
// frame belongs to the same request and is skipped.
func (c *Client) Request(service, body string, timeout time.Duration) (Message, error) {
	return c.await(service, body, timeout, func(message Message) bool {
		return message.Kind != "CONTINUE"
	})
}

// RequestFirst returns the first reply that is not an error. Registration
// answers CONTINUE Registered and subscriptions answer CONTINUE Subscribed;
// neither ever sends COMPLETE, so waiting for one always times out.
func (c *Client) RequestFirst(service, body string, timeout time.Duration) (Message, error) {
	return c.await(service, body, timeout, func(Message) bool { return true })
}

func (c *Client) await(service, body string, timeout time.Duration, accept func(Message) bool) (Message, error) {
	if c == nil || c.closed.Load() {
		return Message{}, ErrClosed
	}
	id := int(c.nextID.Add(1))
	channel := make(chan Message, 16)
	c.pendingMu.Lock()
	c.pending[id] = channel
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()
	if err := c.send(id, service, body); err != nil {
		return Message{}, err
	}
	deadline := time.After(timeout)
	for {
		select {
		case message, open := <-channel:
			if !open {
				return Message{}, ErrClosed
			}
			if failure(message) {
				return message, statusError(service, message)
			}
			if !accept(message) {
				continue
			}
			return message, nil
		case <-deadline:
			return Message{}, fmt.Errorf("roon: %s 超时", service)
		}
	}
}

// failure reports a reply that ends the request without a result.
func failure(message Message) bool {
	switch message.Status {
	case "InvalidRequest", "Unauthorized", "NotAuthorized", "Error", "NetworkError":
		return true
	}
	return false
}

func statusError(service string, message Message) error {
	if message.Status == "Unauthorized" || message.Status == "NotAuthorized" {
		return ErrPendingApproval
	}
	detail := ""
	if message.Body != "" {
		var decoded struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(message.Body), &decoded) == nil {
			detail = decoded.Error
		}
	}
	if detail != "" {
		return fmt.Errorf("roon: %s 返回 %s（%s）", service, message.Status, detail)
	}
	return fmt.Errorf("roon: %s 返回 %s", service, message.Status)
}

// send writes one request frame. Headers follow the order the reference
// implementation uses: Content-Length before Content-Type.
func (c *Client) send(id int, service, body string) error {
	header := "MOO/1 REQUEST " + service + "\nRequest-Id: " + strconv.Itoa(id) + "\n"
	if body != "" {
		header += "Content-Length: " + strconv.Itoa(len(body)) + "\nContent-Type: application/json\n"
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteMessage(websocket.BinaryMessage, []byte(header+"\n"+body))
}

// reply answers a REQUEST the core sent to this extension.
func (c *Client) reply(requestID int, status, body string) error {
	header := "MOO/1 COMPLETE " + status + "\nRequest-Id: " + strconv.Itoa(requestID) + "\n"
	if body != "" {
		header += "Content-Length: " + strconv.Itoa(len(body)) + "\nContent-Type: application/json\n"
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteMessage(websocket.BinaryMessage, []byte(header+"\n"+body))
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
		if message.Kind == "REQUEST" {
			c.answerRequest(message)
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

// answerRequest keeps the extension well-behaved: Roon pings it, asks it to
// pair, and expects an answer to anything else it sends. A dropped ping is
// enough for the core to remove the client, which looks like "registration
// succeeded but nothing works".
func (c *Client) answerRequest(request Message) {
	switch request.Service {
	case "com.roonlabs.ping:1/ping":
		_ = c.reply(request.RequestID, "Success", "")
	case "com.roonlabs.pairing:1/get_pairing":
		paired, _ := c.PairedCoreID.Load().(string)
		body, _ := json.Marshal(map[string]string{"paired_core_id": paired})
		_ = c.reply(request.RequestID, "Success", string(body))
	case "com.roonlabs.pairing:1/pair":
		// The core is pairing this connection; the reference implementation
		// sends no reply to pair, so neither do we.
	default:
		_ = c.reply(request.RequestID, "InvalidRequest",
			`{"error":"unknown service: `+request.Service+`"}`)
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
