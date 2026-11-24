package browser

import (
	"context"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"
)

// TestDockerEndToEnd starts the real browser image with the Docker launcher
// and reads its screen over VNC. It runs only when BROWSER_E2E_IMAGE names a
// local build of browser/:
//
//	docker build -t bmc-browser:dev browser/
//	BROWSER_E2E_IMAGE=bmc-browser:dev BROWSER_E2E_PNG=/tmp/browser.png \
//	  go test ./pkg/browser -run DockerEndToEnd -v
//
// BROWSER_E2E_URL picks the page (default https://example.org/) and
// BROWSER_E2E_ENV adds one KEY=VALUE for the container, e.g.
// BROWSER_NO_SANDBOX=1 where seccomp blocks Chromium's sandbox.
func TestDockerEndToEnd(t *testing.T) {
	img := os.Getenv("BROWSER_E2E_IMAGE")
	if img == "" {
		t.Skip("BROWSER_E2E_IMAGE not set")
	}
	url := os.Getenv("BROWSER_E2E_URL")
	if url == "" {
		url = "https://example.org/"
	}
	d := &Docker{Image: img, Env: map[string]string{}}
	if k, v, ok := strings.Cut(os.Getenv("BROWSER_E2E_ENV"), "="); ok {
		d.Env[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	inst, err := d.Launch(ctx, Spec{URL: url, Password: "e2e-pass", Session: "e2e", Width: 1024, Height: 768, MaxLifetime: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Close()
	c, err := DialRetry(ctx, inst.Addr(), "e2e-pass", 250*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Wait until the page has painted: the screen keeps changing while
	// Chromium starts and loads, then settles.
	var last uint64
	stable := 0
	for stable < 6 {
		select {
		case <-ctx.Done():
			t.Fatal("screen never settled")
		case <-time.After(500 * time.Millisecond):
		}
		_, rev, err := c.Frame()
		if err != nil {
			t.Fatal(err)
		}
		if rev > 0 && rev == last {
			stable++
		} else {
			stable = 0
		}
		last = rev
	}
	frame, _, err := c.Frame()
	if err != nil {
		t.Fatal(err)
	}
	if b := frame.Bounds(); b.Dx() != 1024 || b.Dy() != 768 {
		t.Fatalf("screen %v, want 1024x768", b)
	}
	// A blank screen means Chromium never drew.
	first := frame.RGBAAt(0, 0)
	varied := false
	for y := 0; y < 768 && !varied; y += 7 {
		for x := 0; x < 1024; x += 7 {
			if frame.RGBAAt(x, y) != first {
				varied = true
				break
			}
		}
	}
	if !varied {
		t.Error("the browser screen is a single colour")
	}
	if out := os.Getenv("BROWSER_E2E_PNG"); out != "" {
		f, err := os.Create(out)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, frame); err != nil {
			t.Fatal(err)
		}
		t.Logf("screenshot written to %s", out)
	}
}
