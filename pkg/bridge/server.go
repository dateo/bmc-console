// Package bridge exposes a BMC graphical console to a standard VNC viewer or
// Apache Guacamole.
//
// A Backend is one open console session (HPE iLO, Dell iDRAC, Supermicro
// ATEN, or a synthetic demo). ServeConn speaks the server side of RFB
// 3.3/3.7/3.8 to one viewer on top of a Backend, and Server runs the
// single-viewer accept loop used by cmd/bmc-vnc: a successful VNC login opens
// the hardware session and disconnecting releases it.
package bridge

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"time"
)

// MaxPasswordLen is the number of password bytes classic VNC authentication
// uses. Longer passwords are rejected rather than silently truncated.
const MaxPasswordLen = 8

// OpenFunc opens a Backend after the viewer has authenticated.
type OpenFunc func(ctx context.Context) (Backend, error)

// ErrAuthFailed reports a viewer whose VNC authentication response was
// rejected.
var ErrAuthFailed = errors.New("viewer authentication failed")

// ServeConn authenticates the viewer on c with password (empty selects no
// authentication), calls open, and relays frames and input until the viewer
// disconnects, the backend fails, or ctx ends. The backend is closed before
// ServeConn returns; c is not.
func ServeConn(ctx context.Context, c net.Conn, password string, open OpenFunc) error {
	if len(password) > MaxPasswordLen {
		return errors.New("bridge: VNC authentication accepts at most 8 password bytes")
	}
	if password == "" {
		return serveConn(ctx, c, nil, open)
	}
	return serveConn(ctx, c, PasswordAuth(password, open), nil)
}

// ServeConnAuth is ServeConn with classic VNC authentication decided by auth,
// which also chooses the backend for the viewer. It returns ErrAuthFailed when
// auth rejects the viewer.
func ServeConnAuth(ctx context.Context, c net.Conn, auth Authenticator) error {
	if auth == nil {
		return errors.New("bridge: nil Authenticator")
	}
	return serveConn(ctx, c, auth, nil)
}

func serveConn(ctx context.Context, c net.Conn, auth Authenticator, noAuth OpenFunc) error {
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	v := viewer{Conn: c, in: bufio.NewReader(c)}
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	open, err := v.handshake(auth, noAuth)
	if err != nil {
		return err
	}
	_ = c.SetReadDeadline(time.Time{})
	b, err := open(ctx)
	if err != nil {
		return err
	}
	defer b.Close()
	return v.serve(b)
}

// Server accepts viewers on a listener and bridges each to a fresh Backend.
// Only one viewer is served at a time; further connections are closed until
// the active one ends.
type Server struct {
	// Password is the classic VNC password (at most 8 bytes). Empty disables
	// viewer authentication.
	Password string
	// Open creates the Backend for each authenticated viewer.
	Open OpenFunc
	// Logger receives session errors. Nil uses log.Default().
	Logger *log.Logger
}

// Serve runs the accept loop until ctx is cancelled or l fails. It closes l
// when ctx ends and waits for the active session before returning.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	if len(s.Password) > MaxPasswordLen {
		return errors.New("bridge: VNC authentication accepts at most 8 password bytes")
	}
	if s.Open == nil {
		return errors.New("bridge: Server.Open is nil")
	}
	logger := s.Logger
	if logger == nil {
		logger = log.Default()
	}
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	active := make(chan struct{}, 1)
	defer func() { active <- struct{}{} }()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case active <- struct{}{}:
		default:
			c.Close()
			continue
		}
		go func() {
			defer func() { <-active }()
			defer c.Close()
			err := ServeConn(ctx, c, s.Password, s.Open)
			if err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
				logger.Printf("session ended: %v", err)
			}
		}()
	}
}
