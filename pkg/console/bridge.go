// Adapter accessors for bmc-vnc. The caller serializes viewer input.
package console

import (
	"context"
	"image"
)

func OpenBridge(ctx context.Context, opts OpenOptions) (*Session, error) {
	connect, cancel := context.WithTimeout(ctx, DefaultConnectTimeout)
	defer cancel()
	return openSession(ctx, connect, opts, nil, nil)
}
func (s *Session) Frame(dst *image.RGBA) *image.RGBA {
	if s.remote != nil {
		return s.remote.Frame(dst)
	}
	return s.stream.CopyFrame(dst)
}
func (s *Session) Keyboard(report [10]byte) error {
	if !s.State().InputReady {
		return nil
	}
	if s.remote != nil {
		return s.remote.SendKeyboardReport(report)
	}
	c, e := s.readyConnection(false)
	if e != nil {
		return e
	}
	return c.SendKeyboardReport(report)
}
func (s *Session) Pointer(x, y, dx, dy, w, h int, wheel int8, buttons byte) error {
	if !s.State().InputReady {
		return nil
	}
	if s.remote != nil {
		return s.remote.SendMouse(x, y, dx, dy, w, h, wheel, buttons)
	}
	c, e := s.readyConnection(false)
	if e != nil {
		return e
	}
	return c.SendMouse(x, y, dx, dy, w, h, wheel, buttons)
}
