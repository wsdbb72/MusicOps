package app

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const roonInfoRequest = "MOO/1 REQUEST com.roonlabs.registry:1/info\nRequest-Id: 0\n\n"
const roonWebsocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// testRoon checks that a loopback Roon core answers the old read-only info
// request. It never registers an extension and never scans a library.
func (a *App) testRoon(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	host := strings.TrimSpace(body.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	if body.Port == 0 {
		body.Port = 9330
	}
	if body.Port < 1 || body.Port > 65535 || strings.ContainsAny(host, " /\r\n") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	address := net.JoinHostPort(host, strconv.Itoa(body.Port))
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reachable": false, "registered": false, "note": "没有连上 Roon，也没有注册。"})
		return
	}
	defer conn.Close()
	if !loopbackHost(host) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reachable": true, "registered": false, "note": "只对本机做只读 info 探测。没有注册，没有扫描资料库。"})
		return
	}
	name, version, authRequired := probeRoonInfo(conn)
	if authRequired {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reachable": true, "registered": false, "note": "Roon 拒绝了只读探测，没有注册。"})
		return
	}
	if name == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reachable": true, "registered": false, "note": "端口可连接。没有读到 core 信息，也没有注册。"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "reachable": true, "registered": false, "core_name": name, "core_version": version,
		"note": "只读取了 Roon 的 info。没有注册，没有扫描资料库。",
	})
}

func loopbackHost(host string) bool {
	switch strings.Trim(strings.ToLower(host), "[]") {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}

func probeRoonInfo(conn net.Conn) (string, string, bool) {
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	key := base64.StdEncoding.EncodeToString([]byte("musicops-roon-info"))
	request := "GET /api HTTP/1.1\r\nHost: 127.0.0.1\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		return "", "", false
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		return "", "", false
	}
	header := string(buf[:n])
	if !strings.Contains(header, "101") || !strings.Contains(header, acceptKey(key)) {
		return "", "", false
	}
	payload := []byte(roonInfoRequest)
	frame := []byte{0x82, byte(0x80 | len(payload)), 1, 2, 3, 4}
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ frame[2+i%4]
	}
	if _, err := conn.Write(append(frame, masked...)); err != nil {
		return "", "", false
	}
	n, err = conn.Read(buf)
	if err != nil || n < 2 {
		return "", "", false
	}
	return parseMooInfo(unmaskServerFrame(buf[:n]))
}

// parseMooInfo accepts only the old read-only COMPLETE Success envelope.
// Unauthorized names stay unauthorized. A bare JSON body is not a core.
func parseMooInfo(body string) (string, string, bool) {
	if !utf8.ValidString(body) || len(body) > 1<<20 {
		return "", "", false
	}
	header, rest, ok := strings.Cut(body, "\n\n")
	if !ok || len(header) > 8192 || header == "" {
		return "", "", false
	}
	lines := strings.Split(header, "\n")
	kind, status, ok := strings.Cut(strings.TrimPrefix(lines[0], "MOO/1 "), " ")
	if !ok || !strings.HasPrefix(lines[0], "MOO/1 ") {
		return "", "", false
	}
	switch status {
	case "Unauthorized", "NotRegistered", "AuthorizationRequired", "NotAuthorized":
		return "", "", true
	}
	if kind != "COMPLETE" || status != "Success" {
		return "", "", false
	}
	headers := map[string]string{}
	for _, line := range lines[1:] {
		name, value, found := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !found || name == "" || headers[name] != "" {
			return "", "", false
		}
		headers[name] = strings.TrimSpace(value)
	}
	if headers["Request-Id"] != "0" {
		return "", "", false
	}
	if headers["Content-Length"] == "" {
		if rest != "" || headers["Content-Type"] != "" {
			return "", "", false
		}
		return "", "", false
	}
	length, err := strconv.Atoi(headers["Content-Length"])
	if err != nil || length != len(rest) || headers["Content-Type"] != "application/json" {
		return "", "", false
	}
	var decoded struct {
		CoreID         string `json:"core_id"`
		DisplayName    string `json:"display_name"`
		DisplayVersion string `json:"display_version"`
	}
	if json.Unmarshal([]byte(rest), &decoded) != nil || !roonText(decoded.CoreID) || !roonText(decoded.DisplayName) || !roonText(decoded.DisplayVersion) {
		return "", "", false
	}
	return decoded.DisplayName, decoded.DisplayVersion, false
}

func roonText(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 500 {
		return false
	}
	for _, r := range value {
		if r < 32 {
			return false
		}
	}
	return true
}

func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + roonWebsocketGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func unmaskServerFrame(frame []byte) string {
	if len(frame) < 2 {
		return ""
	}
	length := int(frame[1] & 0x7f)
	offset := 2
	if length == 126 {
		if len(frame) < 4 {
			return ""
		}
		length = int(binary.BigEndian.Uint16(frame[2:4]))
		offset = 4
	}
	if frame[1]&0x80 != 0 {
		if len(frame) < offset+4+length {
			return ""
		}
		mask := frame[offset : offset+4]
		offset += 4
		out := make([]byte, length)
		for i := 0; i < length; i++ {
			out[i] = frame[offset+i] ^ mask[i%4]
		}
		return string(out)
	}
	if offset+length > len(frame) {
		return ""
	}
	return string(frame[offset : offset+length])
}
