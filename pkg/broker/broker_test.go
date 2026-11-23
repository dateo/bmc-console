package broker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dateo/bmc-console/pkg/bridge"
)

const testToken = "0123456789abcdef-test"

type opened struct {
	mu   sync.Mutex
	opts []bridge.Options
}

func (o *opened) open(_ context.Context, opts bridge.Options) (bridge.Backend, error) {
	o.mu.Lock()
	o.opts = append(o.opts, opts)
	o.mu.Unlock()
	return bridge.NewDemo(), nil
}

func (o *opened) count() int { return len(o.all()) }

func (o *opened) all() []bridge.Options {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]bridge.Options(nil), o.opts...)
}

type fixture struct {
	t      *testing.T
	b      *Broker
	vnc    string
	api    *httptest.Server
	opened *opened
	logs   *bytes.Buffer
	now    time.Time
	mu     sync.Mutex
}

func newFixture(t *testing.T, mutate func(*Config)) *fixture {
	t.Helper()
	f := &fixture{t: t, opened: &opened{}, logs: &bytes.Buffer{}, now: time.Unix(1_800_000_000, 0)}
	cfg := Config{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		Open:            f.opened.open,
		Logger:          slog.New(slog.NewJSONHandler(&lockedWriter{w: f.logs}, nil)),
		Now:             func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.b = b
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.vnc = l.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = b.ServeVNC(ctx, l); close(done) }()
	h, err := b.Handler(testToken)
	if err != nil {
		t.Fatal(err)
	}
	f.api = httptest.NewServer(h)
	t.Cleanup(func() {
		f.api.Close()
		cancel()
		<-done
	})
	return f
}

func (f *fixture) advance(d time.Duration) { f.mu.Lock(); f.now = f.now.Add(d); f.mu.Unlock() }

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (f *fixture) post(body string, token string) (*http.Response, map[string]any) {
	f.t.Helper()
	req, _ := http.NewRequest("POST", f.api.URL+"/v1/tickets", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func (f *fixture) ticket(host string) Ticket {
	f.t.Helper()
	resp, out := f.post(`{"mode":"bmc","host":"`+host+`","username":"op","password":"secret","user":"alice","asset":"dev-1"}`, testToken)
	if resp.StatusCode != http.StatusCreated {
		f.t.Fatalf("issue: %d %v", resp.StatusCode, out)
	}
	exp, _ := time.Parse(time.RFC3339, out["expiresAt"].(string))
	return Ticket{ID: out["id"].(string), VNCPassword: out["vncPassword"].(string), ExpiresAt: exp}
}

// viewer performs the RFB 3.8 handshake with VNC authentication and returns
// the connection after ServerInit, or the security result on failure.
func (f *fixture) viewer(password string) (net.Conn, image, error) {
	f.t.Helper()
	c, err := net.Dial("tcp", f.vnc)
	if err != nil {
		f.t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	read := func(n int) []byte {
		p := make([]byte, n)
		if _, err := io.ReadFull(r, p); err != nil {
			f.t.Fatalf("read: %v", err)
		}
		return p
	}
	read(12)
	c.Write([]byte("RFB 003.008\n"))
	if p := read(2); p[0] != 1 || p[1] != 2 {
		f.t.Fatalf("security types %v", p)
	}
	c.Write([]byte{2})
	var ch [16]byte
	copy(ch[:], read(16))
	resp := bridge.VNCResponse(password, ch)
	c.Write(resp[:])
	if res := binary.BigEndian.Uint32(read(4)); res != 0 {
		c.Close()
		return nil, image{}, errors.New("auth failed")
	}
	c.Write([]byte{1})
	si := read(24)
	read(int(binary.BigEndian.Uint32(si[20:])))
	return c, image{int(binary.BigEndian.Uint16(si)), int(binary.BigEndian.Uint16(si[2:]))}, nil
}

type image struct{ w, h int }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTicketOpensSessionOnce(t *testing.T) {
	f := newFixture(t, nil)
	tk := f.ticket("10.1.2.3")
	if len(tk.VNCPassword) != 8 {
		t.Fatalf("password %q", tk.VNCPassword)
	}
	c, img, err := f.viewer(tk.VNCPassword)
	if err != nil {
		t.Fatal(err)
	}
	if img != (image{640, 400}) {
		t.Fatalf("framebuffer %v", img)
	}
	if f.opened.count() != 1 {
		t.Fatalf("backend opened %d times", f.opened.count())
	}
	got := f.opened.all()[0]
	if got.Host != "10.1.2.3" || got.Username != "op" || got.Password != "secret" || got.Mode != bridge.ModeBMC {
		t.Fatalf("options %+v", got)
	}
	if s := f.b.Sessions(); len(s) != 1 || s[0].User != "alice" || s[0].Asset != "dev-1" {
		t.Fatalf("sessions %+v", s)
	}
	// The same host is busy while the session runs.
	if resp, _ := f.post(`{"mode":"bmc","host":"10.1.2.3","username":"op","password":"x"}`, testToken); resp.StatusCode != http.StatusConflict {
		t.Fatalf("second ticket for busy host: %d", resp.StatusCode)
	}
	// The ticket is single use.
	if _, _, err := f.viewer(tk.VNCPassword); err == nil {
		t.Fatal("ticket accepted twice")
	}
	c.Close()
	waitFor(t, "session end", func() bool { return len(f.b.Sessions()) == 0 })
	// Host is free again.
	f.ticket("10.1.2.3")
	if !strings.Contains(f.logs.String(), `"msg":"session ended"`) {
		t.Fatal("no session end in audit log")
	}
}

func TestWrongAndExpiredPasswordsRejected(t *testing.T) {
	f := newFixture(t, nil)
	tk := f.ticket("10.9.9.9")
	if _, _, err := f.viewer("wrongpw1"); err == nil {
		t.Fatal("wrong password accepted")
	}
	f.advance(61 * time.Second)
	if _, _, err := f.viewer(tk.VNCPassword); err == nil {
		t.Fatal("expired ticket accepted")
	}
	if f.opened.count() != 0 {
		t.Fatal("backend opened for rejected viewer")
	}
	f.ticket("10.9.9.9") // expiry released the host
}

func TestTwoConcurrentSessions(t *testing.T) {
	f := newFixture(t, nil)
	a, b := f.ticket("10.0.0.1"), f.ticket("10.0.0.2")
	cb, _, err := f.viewer(b.VNCPassword)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	ca, _, err := f.viewer(a.VNCPassword)
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	if n := len(f.b.Sessions()); n != 2 {
		t.Fatalf("%d sessions", n)
	}
	hosts := map[string]bool{}
	for _, o := range f.opened.all() {
		hosts[o.Host] = true
	}
	if !hosts["10.0.0.1"] || !hosts["10.0.0.2"] {
		t.Fatalf("opened %v", hosts)
	}
}

func TestDeleteEndsSession(t *testing.T) {
	f := newFixture(t, nil)
	tk := f.ticket("10.0.0.5")
	c, _, err := f.viewer(tk.VNCPassword)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req, _ := http.NewRequest("DELETE", f.api.URL+"/v1/sessions/"+tk.ID, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %v %v", err, resp)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, c); err != nil && !errors.Is(err, net.ErrClosed) && !strings.Contains(err.Error(), "reset") {
		t.Fatalf("connection not closed: %v", err)
	}
	waitFor(t, "session removal", func() bool { return len(f.b.Sessions()) == 0 })
}

func TestIssueValidation(t *testing.T) {
	f := newFixture(t, nil)
	for name, tc := range map[string]struct {
		body, token string
		status      int
	}{
		"no token":         {`{"mode":"bmc","host":"10.0.0.1","username":"u","password":"p"}`, "", 401},
		"bad token":        {`{"mode":"bmc","host":"10.0.0.1","username":"u","password":"p"}`, "nope", 401},
		"outside networks": {`{"mode":"bmc","host":"192.168.1.1","username":"u","password":"p"}`, testToken, 403},
		"demo mode":        {`{"mode":"demo"}`, testToken, 403},
		"unknown mode":     {`{"mode":"ipmi","host":"10.0.0.1","username":"u","password":"p"}`, testToken, 400},
		"missing password": {`{"mode":"bmc","host":"10.0.0.1","username":"u"}`, testToken, 400},
		"unknown field":    {`{"mode":"bmc","host":"10.0.0.1","username":"u","password":"p","x":1}`, testToken, 400},
		"insecure tls":     {`{"mode":"bmc","host":"10.0.0.1","username":"u","password":"p","insecureSkipVerify":true}`, testToken, 403},
		"plain aten":       {`{"mode":"aten","host":"10.0.0.1","username":"u","password":"p","allowPlainAten":true}`, testToken, 403},
	} {
		t.Run(name, func(t *testing.T) {
			if resp, out := f.post(tc.body, tc.token); resp.StatusCode != tc.status {
				t.Fatalf("status %d, want %d (%v)", resp.StatusCode, tc.status, out)
			}
		})
	}
}

func TestHostnameResolution(t *testing.T) {
	f := newFixture(t, func(c *Config) {
		c.Resolver = fakeResolver{"ilo.mgmt": {netip.MustParseAddr("10.20.0.5")}, "evil": {netip.MustParseAddr("10.20.0.6"), netip.MustParseAddr("169.254.169.254")}}
	})
	f.ticket("ilo.mgmt")
	if resp, _ := f.post(`{"mode":"bmc","host":"evil","username":"u","password":"p"}`, testToken); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("mixed resolution allowed: %d", resp.StatusCode)
	}
	if resp, _ := f.post(`{"mode":"bmc","host":"unknown","username":"u","password":"p"}`, testToken); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unresolvable allowed: %d", resp.StatusCode)
	}
}

type fakeResolver map[string][]netip.Addr

func (r fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := r[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

func TestLimitsAndConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("empty config accepted")
	}
	f := newFixture(t, func(c *Config) { c.MaxPending = 2 })
	f.ticket("10.0.0.1")
	f.ticket("10.0.0.2")
	if resp, _ := f.post(`{"mode":"bmc","host":"10.0.0.3","username":"u","password":"p"}`, testToken); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("limit not enforced: %d", resp.StatusCode)
	}
	if _, err := f.b.Handler("short"); err == nil {
		t.Fatal("short token accepted")
	}
	// Credentials never appear in the audit log.
	if strings.Contains(f.logs.String(), "secret") {
		t.Fatal("password logged")
	}
}

func TestDemoTickets(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.AllowDemo = true; c.Open = nil })
	resp, out := f.post(`{"mode":"demo","user":"dev"}`, testToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("demo ticket: %d %v", resp.StatusCode, out)
	}
	// Demo tickets hold no host, so several can be pending at once.
	if resp, _ := f.post(`{"mode":"demo"}`, testToken); resp.StatusCode != http.StatusCreated {
		t.Fatalf("second demo ticket: %d", resp.StatusCode)
	}
	c, img, err := f.viewer(out["vncPassword"].(string))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if img != (image{640, 400}) {
		t.Fatalf("framebuffer %v", img)
	}
}
