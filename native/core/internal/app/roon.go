package app

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// testRoon dials the Roon port and closes the connection without writing.
// A successful dial is reachability only. It does not register, scan or change a library.
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
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(body.Port)), 2*time.Second)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reachable": false, "registered": false, "note": "没有连上 Roon，也没有注册。"})
		return
	}
	_ = conn.Close()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reachable": true, "registered": false, "note": "只确认端口可连接。没有注册，没有扫描资料库。"})
}
