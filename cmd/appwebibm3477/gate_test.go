package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestValidateListen(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:53477", "localhost:1", "[::1]:80"} {
		if err := validateListen(ok); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{":53477", "0.0.0.0:80", "192.168.1.5:80", "nohost"} {
		if validateListen(bad) == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// TestGateFlow: launch token -> cookie -> access; no cookie or a foreign
// Host is refused.
func TestGateFlow(t *testing.T) {
	addr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 53477}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	g := newAppGate(addr, "tok", "cookieval", "/", ok)

	do := func(path, host string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = host
		if c != nil {
			r.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, r)
		return rec
	}
	if rec := do("/", "127.0.0.1:53477", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("no cookie: got %d, want 403", rec.Code)
	}
	if rec := do("/?t=wrong", "127.0.0.1:53477", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("bad token: got %d, want 403", rec.Code)
	}
	rec := do("/?t=tok", "127.0.0.1:53477", nil)
	if rec.Code != http.StatusFound || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("good token: got %d with %d cookies", rec.Code, len(rec.Result().Cookies()))
	}
	c := rec.Result().Cookies()[0]
	if rec := do("/", "127.0.0.1:53477", c); rec.Code != 200 {
		t.Fatalf("with cookie: got %d, want 200", rec.Code)
	}
	if rec := do("/", "evil.example:53477", c); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign Host: got %d, want 403", rec.Code)
	}
}

func TestStrictSameOrigin(t *testing.T) {
	r := httptest.NewRequest("GET", "/ws", nil)
	r.Host = "127.0.0.1:53477"
	r.Header.Set("Origin", "http://127.0.0.1:53477")
	if !strictSameOrigin(r) {
		t.Fatal("own origin refused")
	}
	r.Header.Set("Origin", "http://127.0.0.1:8080") // another local page
	if strictSameOrigin(r) {
		t.Fatal("other local port accepted")
	}
	r.Header.Del("Origin")
	if strictSameOrigin(r) {
		t.Fatal("missing Origin accepted")
	}
}

func TestBrowserCommand(t *testing.T) {
	u := "http://127.0.0.1:1/?t=x"
	for goos, want := range map[string]string{"darwin": "open", "windows": "rundll32", "linux": "xdg-open"} {
		name, args := browserCommand(goos, u)
		if name != want || args[len(args)-1] != u {
			t.Errorf("%s: %s %v", goos, name, args)
		}
	}
}

func TestIdleMonitorExits(t *testing.T) {
	fired := make(chan string, 1)
	m := newIdleMonitor(20*time.Millisecond, func(r string) { fired <- r })
	m.opened()
	m.closed() // last tab closed: exit after the grace period
	select {
	case r := <-fired:
		if r != "browser tab closed" {
			t.Fatalf("reason %q", r)
		}
	case <-time.After(time.Second):
		t.Fatal("idle exit did not fire")
	}
	m2 := newIdleMonitor(20*time.Millisecond, func(r string) { fired <- r })
	m2.opened()
	m2.closed()
	m2.opened() // reload: reopened within the grace period
	select {
	case r := <-fired:
		t.Fatalf("exited despite an open tab: %s", r)
	case <-time.After(60 * time.Millisecond):
	}
}
