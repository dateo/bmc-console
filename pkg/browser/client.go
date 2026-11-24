// Package browser serves a web browser as a console: a short-lived container
// runs a kiosk-mode browser on its own VNC server, and the broker relays that
// VNC session to the viewer like any other console backend.
//
// The package has three parts:
//
//   - Client, a bridge.Backend that is a VNC client to the container;
//   - Launcher, which starts and removes the container (Docker for local
//     development, Kubernetes for clusters);
//   - the container image itself, built from browser/ in this repository.
package browser

import (
	"context"
	"errors"
	"fmt"
	"image"
	"net"
	"sync"
	"time"

	"github.com/dateo/bmc-console/pkg/rfb"
)

// TrueColor32 is the pixel format the client asks for: 8 bits per channel in
// a little-endian 32-bit word, what Xvnc produces natively at depth 24.
var TrueColor32 = rfb.PixelFormat{
	BitsPerPixel: 32,
	Depth:        24,
	TrueColor:    true,
	RedMax:       255,
	GreenMax:     255,
	BlueMax:      255,
	RedShift:     16,
	GreenShift:   8,
	BlueShift:    0,
}

// encodings are offered most-preferred first. CopyRect and Hextile keep the
// upstream link small; the link is cluster-internal, so nothing heavier
// (Tight, ZRLE) is needed for a proof of concept.
var encodings = []int32{
	rfb.EncodingCopyRect,
	rfb.EncodingHextile,
	rfb.EncodingRRE,
	rfb.EncodingRaw,
	rfb.EncodingDesktop,
	rfb.EncodingLastRect,
}

// Client relays one upstream VNC session. It implements bridge.Backend.
type Client struct {
	conn net.Conn
	rfb  *rfb.Conn

	done chan struct{}
	err  error // set before done closes

	// frame caches the last snapshot so an unchanged screen is not copied
	// again on every poll.
	frame    *image.RGBA
	revision uint64

	closeOnce sync.Once
	onClose   func()
}

// Dial connects to the VNC server at addr and authenticates with password
// (classic VNC authentication; empty accepts only security type None). The
// handshake must finish within 10 seconds or before ctx ends.
func Dial(ctx context.Context, addr, password string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	r := rfb.New(conn)
	if password != "" {
		r.SetPassword(password)
	}
	init, err := r.Handshake(true)
	if err == nil {
		err = r.SetPixelFormat(TrueColor32)
	}
	if err == nil {
		err = r.SetEncodings(encodings)
	}
	if err == nil {
		err = r.RequestUpdate(false, 0, 0, init.Width, init.Height)
	}
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("browser: VNC handshake with %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Time{})
	c := &Client{conn: conn, rfb: r, done: make(chan struct{})}
	go c.loop()
	return c, nil
}

// DialRetry calls Dial until it succeeds or ctx ends, for a server that is
// still starting. It waits interval between attempts.
func DialRetry(ctx context.Context, addr, password string, interval time.Duration) (*Client, error) {
	for {
		c, err := Dial(ctx, addr, password)
		if err == nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("browser: %s not ready: %w (last error: %v)", addr, ctx.Err(), err)
		case <-time.After(interval):
		}
	}
}

// OnClose registers f to run once after the connection closes, for example
// to remove the container behind it.
func (c *Client) OnClose(f func()) { c.onClose = f }

// loop applies server messages and asks for the next incremental update
// after each framebuffer update, the usual RFB flow control.
func (c *Client) loop() {
	defer close(c.done)
	for {
		u, err := c.rfb.ReadMessage()
		if err != nil {
			c.err = err
			return
		}
		if u == nil {
			continue
		}
		b := c.rfb.Screen().Bounds()
		if err := c.rfb.RequestUpdate(!u.Resized, 0, 0, b.Dx(), b.Dy()); err != nil {
			c.err = err
			return
		}
	}
}

// Frame returns the current picture and its revision.
func (c *Client) Frame() (*image.RGBA, uint64, error) {
	select {
	case <-c.done:
		if c.err == nil {
			return nil, 0, errors.New("browser: upstream VNC session ended")
		}
		return nil, 0, fmt.Errorf("browser: upstream VNC session ended: %w", c.err)
	default:
	}
	screen := c.rfb.Screen()
	rev := screen.Revision()
	if c.frame == nil || rev != c.revision || c.frame.Bounds() != screen.Bounds() {
		c.frame = screen.CopyFrame(c.frame)
		c.revision = rev
	}
	return c.frame, c.revision, nil
}

// Key forwards an X11 keysym unchanged: the upstream server speaks keysyms
// too, so no HID translation is involved.
func (c *Client) Key(sym uint32, down bool) error { return c.rfb.KeyEvent(down, sym) }

// Pointer forwards the position and the RFB button mask unchanged.
func (c *Client) Pointer(x, y int, mask byte) error { return c.rfb.PointerEvent(mask, x, y) }

// Close ends the upstream session and runs the OnClose hook once.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.conn.Close()
		<-c.done
		if c.onClose != nil {
			c.onClose()
		}
	})
}
