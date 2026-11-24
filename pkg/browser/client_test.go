package browser

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dateo/bmc-console/pkg/bridge"
)

// upstream serves the demo screen behind classic VNC authentication, the
// way the browser image's Xvnc serves Chromium.
func upstream(t *testing.T, l net.Listener, password string) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := &bridge.Server{
		Password: password,
		Open:     func(context.Context) (bridge.Backend, error) { return bridge.NewDemo(), nil },
	}
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return cancel
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func waitFrame(t *testing.T, c *Client) (w, h int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		img, rev, err := c.Frame()
		if err != nil {
			t.Fatal(err)
		}
		if rev > 0 {
			return img.Bounds().Dx(), img.Bounds().Dy()
		}
		if time.Now().After(deadline) {
			t.Fatal("no framebuffer update")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDialAuthenticatesAndRelays(t *testing.T) {
	l := listen(t)
	upstream(t, l, "s3cretpw")
	c, err := Dial(context.Background(), l.Addr().String(), "s3cretpw")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if w, h := waitFrame(t, c); w != 640 || h != 400 {
		t.Fatalf("frame %dx%d, want the demo's 640x400", w, h)
	}
	if err := c.Key(0xffbf, true); err != nil { // F2
		t.Fatal(err)
	}
	if err := c.Key(0xffbf, false); err != nil {
		t.Fatal(err)
	}
	if err := c.Pointer(10, 20, 1); err != nil {
		t.Fatal(err)
	}
	// The demo repaints on input, so the revision moves on.
	_, before, _ := c.Frame()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, rev, err := c.Frame()
		if err != nil {
			t.Fatal(err)
		}
		if rev > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("input produced no update")
		}
		_ = c.Pointer(30, 40, 1)
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDialRejectsWrongPassword(t *testing.T) {
	l := listen(t)
	upstream(t, l, "s3cretpw")
	if c, err := Dial(context.Background(), l.Addr().String(), "wrongpw!"); err == nil {
		c.Close()
		t.Fatal("wrong password accepted")
	}
}

func TestDialWithoutPasswordRefusesVNCAuth(t *testing.T) {
	l := listen(t)
	upstream(t, l, "s3cretpw")
	if c, err := Dial(context.Background(), l.Addr().String(), ""); err == nil {
		c.Close()
		t.Fatal("connected without the password the server demands")
	}
}

func TestDialRetryWaitsForServer(t *testing.T) {
	l := listen(t)
	addr := l.Addr().String()
	l.Close() // nothing listens yet, as while the container starts
	go func() {
		time.Sleep(300 * time.Millisecond)
		l2, err := net.Listen("tcp", addr)
		if err != nil {
			t.Error(err)
			return
		}
		upstream(t, l2, "pw")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := DialRetry(ctx, addr, "pw", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFrame(t, c)
}

func TestDialRetryGivesUp(t *testing.T) {
	l := listen(t)
	addr := l.Addr().String()
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := DialRetry(ctx, addr, "pw", 20*time.Millisecond); err == nil {
		t.Fatal("DialRetry succeeded without a server")
	}
}

func TestFrameFailsAfterUpstreamEndsAndCloseRunsHookOnce(t *testing.T) {
	l := listen(t)
	stop := upstream(t, l, "pw")
	c, err := Dial(context.Background(), l.Addr().String(), "pw")
	if err != nil {
		t.Fatal(err)
	}
	var hooks atomic.Int32
	c.OnClose(func() { hooks.Add(1) })
	waitFrame(t, c)
	stop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, err := c.Frame(); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Frame kept succeeding after the upstream ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.Close()
	c.Close()
	if n := hooks.Load(); n != 1 {
		t.Fatalf("OnClose ran %d times", n)
	}
}

func TestValidateURL(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://10.1.2.3/", true},
		{"http://switch.example:8080/ui", true},
		{"ftp://10.1.2.3/", false},
		{"javascript:alert(1)", false},
		{"file:///etc/passwd", false},
		{"https://user:pw@10.1.2.3/", false},
		{"/relative", false},
		{"https://", false},
	} {
		if _, err := ValidateURL(tc.url); (err == nil) != tc.ok {
			t.Errorf("%q: err=%v, want ok=%v", tc.url, err, tc.ok)
		}
	}
}
