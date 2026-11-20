package web

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestOversizedMessageClosesSocket: a browser message above the read
// limit ends the connection instead of being buffered.
func TestOversizedMessageClosesSocket(t *testing.T) {
	srv := httptest.NewServer(New(Config{Model: "3477"}).Handler())
	defer srv.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	big := `{"type":"paste","text":"` + strings.Repeat("a", maxClientMessage) + `"}`
	ws.WriteMessage(websocket.TextMessage, []byte(big))
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("connection still open after an oversized message")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("server did not close the connection")
	}
}

// TestStalledBrowserDisconnected: a browser that stops reading is
// disconnected after the write deadline; the session (and its machine)
// ends instead of blocking forever in a write.
func TestStalledBrowserDisconnected(t *testing.T) {
	if _, err := os.Stat("../../ibm3477_part1.bin"); err != nil {
		t.Skip("3477 ROMs not present")
	}
	if testing.Short() {
		t.Skip("runs the terminal for several seconds")
	}
	old := writeWait
	writeWait = 300 * time.Millisecond
	defer func() { writeWait = old }()

	ended := make(chan struct{})
	s := New(Config{Model: "3477", RomDir: "../.."})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.WS(w, r)
		close(ended)
	}))
	defer srv.Close()
	// a client with a tiny receive buffer that never reads
	d := websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetReadBuffer(2048)
		}
		return c, err
	}}
	ws, _, err := d.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if err := ws.WriteJSON(clientMsg{Type: "power", Model: "3477"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(40 * time.Second):
		t.Fatal("session still running with a browser that does not read")
	}
}
