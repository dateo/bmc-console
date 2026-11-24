package broker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dateo/bmc-console/pkg/bridge"
	"github.com/dateo/bmc-console/pkg/browser"
)

// fakeLauncher "starts a container" by serving the demo screen behind the
// VNC password the broker generated for it.
type fakeLauncher struct {
	mu     sync.Mutex
	specs  []browser.Spec
	closed int
	fail   error
}

type fakeInstance struct {
	addr   string
	stop   context.CancelFunc
	l      *fakeLauncher
	closed bool
}

func (i *fakeInstance) Addr() string { return i.addr }
func (i *fakeInstance) Close() error {
	i.l.mu.Lock()
	defer i.l.mu.Unlock()
	if !i.closed {
		i.closed = true
		i.l.closed++
		i.stop()
	}
	return nil
}

func (f *fakeLauncher) Launch(ctx context.Context, s browser.Spec) (browser.Instance, error) {
	f.mu.Lock()
	f.specs = append(f.specs, s)
	fail := f.fail
	f.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	sctx, stop := context.WithCancel(context.Background())
	srv := &bridge.Server{
		Password: s.Password,
		Open:     func(context.Context) (bridge.Backend, error) { return bridge.NewDemo(), nil },
	}
	go func() { _ = srv.Serve(sctx, l) }()
	return &fakeInstance{addr: l.Addr().String(), stop: stop, l: f}, nil
}

func (f *fakeLauncher) state() (specs []browser.Spec, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]browser.Spec(nil), f.specs...), f.closed
}

func browserFixture(t *testing.T, fl *fakeLauncher) *fixture {
	return newFixture(t, func(c *Config) {
		c.Browser = &BrowserConfig{Launcher: fl, StartTimeout: 5 * time.Second, MaxLifetime: time.Hour}
	})
}

func TestBrowserTicketOpensKiosk(t *testing.T) {
	fl := &fakeLauncher{}
	f := browserFixture(t, fl)
	resp, out := f.post(`{"mode":"browser","url":"https://10.1.2.3/ui","user":"alice","asset":"sw-1"}`, testToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue: %d %v", resp.StatusCode, out)
	}
	id := out["id"].(string)
	// The container starts while the ticket waits for its viewer.
	waitFor(t, "launch", func() bool { s, _ := fl.state(); return len(s) == 1 })
	specs, _ := fl.state()
	if s := specs[0]; s.URL != "https://10.1.2.3/ui" || s.Session != id || len(s.Password) != bridge.MaxPasswordLen {
		t.Fatalf("spec %+v", s)
	}
	if specs[0].Password == out["vncPassword"] {
		t.Fatal("the container's VNC password equals the viewer's ticket password")
	}

	c, img, err := f.viewer(out["vncPassword"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if img != (image{640, 400}) {
		t.Fatalf("framebuffer %v", img)
	}
	if s := f.b.Sessions(); len(s) != 1 || s[0].Mode != bridge.ModeBrowser || s[0].Host != "10.1.2.3" {
		t.Fatalf("sessions %+v", s)
	}
	c.Close()
	waitFor(t, "container removal", func() bool { _, n := fl.state(); return n == 1 })
	waitFor(t, "session end", func() bool { return len(f.b.Sessions()) == 0 })
	if f.opened.count() != 0 {
		t.Fatal("browser ticket went through the BMC opener")
	}
}

func TestBrowserTicketValidation(t *testing.T) {
	f := newFixture(t, nil)
	if resp, _ := f.post(`{"mode":"browser","url":"https://10.1.2.3/"}`, testToken); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("browser without launcher: %d", resp.StatusCode)
	}

	fl := &fakeLauncher{}
	f = browserFixture(t, fl)
	for body, want := range map[string]int{
		`{"mode":"browser","url":"file:///etc/passwd"}`:        http.StatusBadRequest,
		`{"mode":"browser","url":"https://user:pw@10.1.2.3/"}`: http.StatusBadRequest,
		`{"mode":"browser"}`:                                     http.StatusBadRequest,
		`{"mode":"browser","url":"https://192.168.1.1/"}`:        http.StatusForbidden,
		`{"mode":"browser","url":"https://10.9.9.9:8443/admin"}`: http.StatusCreated,
	} {
		if resp, out := f.post(body, testToken); resp.StatusCode != want {
			t.Errorf("%s: %d %v, want %d", body, resp.StatusCode, out, want)
		}
	}
	// Browser tickets share their host.
	for range 2 {
		if resp, out := f.post(`{"mode":"browser","url":"https://10.1.2.3/"}`, testToken); resp.StatusCode != http.StatusCreated {
			t.Fatalf("second ticket for the same host: %d %v", resp.StatusCode, out)
		}
	}
}

func TestBrowserTicketExpiryRemovesContainer(t *testing.T) {
	fl := &fakeLauncher{}
	f := browserFixture(t, fl)
	if resp, _ := f.post(`{"mode":"browser","url":"https://10.1.2.3/"}`, testToken); resp.StatusCode != http.StatusCreated {
		t.Fatal(resp.StatusCode)
	}
	waitFor(t, "launch", func() bool { s, _ := fl.state(); return len(s) == 1 })
	f.advance(2 * time.Minute)
	// Any ticket request purges expired tickets.
	f.post(`{"mode":"browser","url":"https://10.1.2.4/"}`, testToken)
	waitFor(t, "container removal", func() bool { _, n := fl.state(); return n == 1 })
}

func TestBrowserTicketRevokeRemovesContainer(t *testing.T) {
	fl := &fakeLauncher{}
	f := browserFixture(t, fl)
	_, out := f.post(`{"mode":"browser","url":"https://10.1.2.3/"}`, testToken)
	waitFor(t, "launch", func() bool { s, _ := fl.state(); return len(s) == 1 })
	if !f.b.Close(out["id"].(string)) {
		t.Fatal("revoke found nothing")
	}
	waitFor(t, "container removal", func() bool { _, n := fl.state(); return n == 1 })
}

func TestBrowserStartFailureEndsViewer(t *testing.T) {
	fl := &fakeLauncher{fail: errors.New("image pull backoff")}
	f := browserFixture(t, fl)
	_, out := f.post(`{"mode":"browser","url":"https://10.1.2.3/"}`, testToken)
	waitFor(t, "failure log", func() bool { return strings.Contains(f.logText(), "image pull backoff") })

	// The viewer authenticates, then the connection closes without a
	// ServerInit because there is no browser to show.
	c, err := net.Dial("tcp", f.vnc)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 16)
	read := func(n int) []byte {
		if _, err := ioReadFull(c, buf[:n]); err != nil {
			t.Fatalf("read: %v", err)
		}
		return buf[:n]
	}
	read(12)
	c.Write([]byte("RFB 003.008\n"))
	read(2)
	c.Write([]byte{2})
	var ch [16]byte
	copy(ch[:], read(16))
	resp := bridge.VNCResponse(out["vncPassword"].(string), ch)
	c.Write(resp[:])
	read(4)
	c.Write([]byte{1})
	if _, err := ioReadFull(c, buf[:1]); err == nil {
		t.Fatal("got a ServerInit without a browser")
	}
	waitFor(t, "session end", func() bool { return len(f.b.Sessions()) == 0 })
}

func (f *fixture) logText() string {
	f.lw.mu.Lock()
	defer f.lw.mu.Unlock()
	return f.logs.String()
}

func ioReadFull(c net.Conn, p []byte) (int, error) {
	n := 0
	for n < len(p) {
		m, err := c.Read(p[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
