package browser

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"
)

// VNCPort is the port the browser image's VNC server listens on.
const VNCPort = 5900

// Spec describes one browser to start.
type Spec struct {
	// URL is the page the kiosk opens. Only http and https are accepted.
	URL string
	// Password is the VNC password the container's server requires. The
	// caller generates it per session; at most rfb.MaxPasswordLen bytes count.
	Password string
	// Width and Height set the screen size. Zero uses 1280x800.
	Width, Height int
	// Session labels the container for audit and cleanup.
	Session string
	// MaxLifetime is a hard limit after which the platform removes the
	// container even if nobody closes it. Zero uses one hour.
	MaxLifetime time.Duration
}

// Instance is a running browser container.
type Instance interface {
	// Addr is the host:port of the container's VNC server.
	Addr() string
	// Close removes the container. It is safe to call more than once.
	Close() error
}

// Launcher starts browser containers.
type Launcher interface {
	// Launch starts a container for s and returns once its address is
	// known. The VNC server inside may still be starting: callers connect
	// with DialRetry.
	Launch(ctx context.Context, s Spec) (Instance, error)
}

// ValidateURL accepts absolute http and https URLs with a host.
func ValidateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("browser: invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("browser: URL scheme must be http or https")
	}
	if u.Host == "" || u.User != nil {
		return nil, errors.New("browser: URL needs a host and must not carry credentials")
	}
	return u, nil
}

func (s Spec) withDefaults() Spec {
	if s.Width <= 0 || s.Height <= 0 {
		s.Width, s.Height = 1280, 800
	}
	if s.MaxLifetime <= 0 {
		s.MaxLifetime = time.Hour
	}
	return s
}

// env is the container environment the browser image reads.
func (s Spec) env() map[string]string {
	return map[string]string{
		"BROWSER_URL":    s.URL,
		"BROWSER_WIDTH":  fmt.Sprint(s.Width),
		"BROWSER_HEIGHT": fmt.Sprint(s.Height),
		"VNC_PASSWORD":   s.Password,
	}
}
