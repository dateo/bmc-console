package bridge

import (
	"context"
	"errors"
	"fmt"
	"image"
	"log"
	"sync"

	"github.com/dateo/bmc-console/pkg/aten"
	"github.com/dateo/bmc-console/pkg/console"
	"github.com/dateo/bmc-console/pkg/kvm"
	"github.com/dateo/bmc-console/pkg/rfb"
)

// Backend is one open console session that a viewer reads frames from and
// sends input to.
//
// Frame returns an immutable snapshot that stays valid until the next call,
// plus a revision that changes whenever the picture changes. Calls from one
// viewer are serialized; a Backend need not be safe for concurrent viewers.
type Backend interface {
	Frame() (*image.RGBA, uint64, error)
	// Key reports an X11 keysym press or release.
	Key(sym uint32, down bool) error
	// Pointer reports an absolute position and the RFB button mask
	// (bit 0 left, 1 middle, 2 right, 3 wheel up, 4 wheel down).
	Pointer(x, y int, mask byte) error
	Close()
}

// Mode selects the controller family a Backend talks to.
type Mode string

const (
	// ModeDemo is a synthetic framebuffer for viewer and Guacamole tests.
	ModeDemo Mode = "demo"
	// ModeBMC detects HPE iLO 4-7 or Dell iDRAC9 and opens its console.
	ModeBMC Mode = "bmc"
	// ModeATEN opens a legacy Supermicro ATEN Hermon KVM session.
	ModeATEN Mode = "aten"
)

// Options configure Open.
type Options struct {
	Mode     Mode
	Host     string
	Username string
	Password string
	// InsecureSkipVerify disables BMC HTTPS certificate verification.
	InsecureSkipVerify bool
	// AllowPlainATEN permits the unencrypted ATEN KVM socket. ModeATEN
	// refuses to connect without it.
	AllowPlainATEN bool
	// Logger receives backend status messages. Nil uses log.Default().
	Logger *log.Logger
}

// Open connects to the controller described by o and returns a ready Backend.
func Open(ctx context.Context, o Options) (Backend, error) {
	logger := o.Logger
	if logger == nil {
		logger = log.Default()
	}
	switch o.Mode {
	case ModeDemo:
		return NewDemo(), nil
	case ModeATEN:
		return OpenATEN(ctx, o.Host, o.Username, o.Password, o.InsecureSkipVerify, o.AllowPlainATEN, logger)
	case ModeBMC:
		s, err := console.OpenBridge(ctx, console.OpenOptions{Address: o.Host, Username: o.Username, Password: o.Password, InsecureSkipVerify: o.InsecureSkipVerify})
		if err != nil {
			return nil, err
		}
		logger.Printf("controller console protocol %d", s.State().ProtocolVersion)
		return NewHardware(s), nil
	default:
		return nil, fmt.Errorf("bridge: unknown mode %q", o.Mode)
	}
}

// HIDUsage maps an X11 keysym to a USB HID keyboard usage, or 0 when the
// key has no mapping. Modifiers map to 224-231.
func HIDUsage(sym uint32) byte {
	modifiers := []uint32{rfb.KeyControlL, rfb.KeyShiftL, rfb.KeyAltL, rfb.KeySuperL, rfb.KeyControlR, rfb.KeyShiftR, rfb.KeyAltR, rfb.KeySuperR}
	for i, k := range modifiers {
		if k == sym {
			return byte(224 + i)
		}
	}
	// Prefer the US backslash/number row over equivalent non-US HID usages.
	for _, shift := range []bool{true, false} {
		for i := 4; i <= 115; i++ {
			if s, ok := rfb.KeysymForHID(byte(i), shift); ok && s == sym {
				return byte(i)
			}
		}
	}
	return 0
}

// Hardware adapts an HPE iLO or Dell iDRAC console.Session to Backend.
type Hardware struct {
	s        *console.Session
	frame    *image.RGBA
	revision uint64
	mods     byte
	keys     []byte
	x, y     int
}

// NewHardware wraps an open session. Close on the result closes s.
func NewHardware(s *console.Session) *Hardware { return &Hardware{s: s} }

// Session returns the underlying console session.
func (h *Hardware) Session() *console.Session { return h.s }

func (h *Hardware) Frame() (*image.RGBA, uint64, error) {
	state := h.s.State()
	if !state.Connected {
		return nil, 0, errors.New("controller disconnected")
	}
	if h.frame == nil || state.FrameRevision != h.revision {
		h.frame = h.s.Frame(h.frame)
		h.revision = state.FrameRevision
	}
	return h.frame, h.revision, nil
}

func (h *Hardware) Key(sym uint32, down bool) error {
	k := HIDUsage(sym)
	if k == 0 || !h.s.State().InputReady {
		return nil
	}
	if k >= 224 {
		bit := byte(1 << (k - 224))
		if down {
			h.mods |= bit
		} else {
			h.mods &^= bit
		}
	} else {
		index := -1
		for i, v := range h.keys {
			if v == k {
				index = i
				break
			}
		}
		if down && index < 0 && len(h.keys) < 6 {
			h.keys = append(h.keys, k)
		}
		if !down && index >= 0 {
			h.keys = append(h.keys[:index], h.keys[index+1:]...)
		}
	}
	return h.s.Keyboard(kvm.KeyboardReport(h.mods, h.keys...))
}

func (h *Hardware) Pointer(x, y int, m byte) error {
	st := h.s.State()
	buttons := m&1 | (m&2)<<1 | (m&4)>>1
	var wheel int8
	if m&8 != 0 {
		wheel = 1
	} else if m&16 != 0 {
		wheel = -1
	}
	err := h.s.Pointer(x, y, x-h.x, y-h.y, st.Width, st.Height, wheel, buttons)
	h.x, h.y = x, y
	return err
}

// Close releases held keys and buttons, then closes the session.
func (h *Hardware) Close() {
	_ = h.s.Keyboard(kvm.KeyboardReport(0))
	_ = h.Pointer(h.x, h.y, 0)
	_ = h.s.Close()
}

// ATEN adapts a legacy Supermicro ATEN Hermon session to Backend.
type ATEN struct {
	session *aten.Session
	logger  *log.Logger
	mu      sync.Mutex
	frame   *image.RGBA
	rev     uint64
	err     error
	cancel  context.CancelFunc
	done    chan struct{}
	logout  func()
	held    map[byte]bool
	x, y    int
}

// OpenATEN logs in over HTTPS, fetches the JNLP ticket and starts the Hermon
// decoder. allowPlain must be true: the adapter only speaks the plaintext
// KVM socket. A nil logger uses log.Default().
func OpenATEN(ctx context.Context, host, user, pw string, insecure, allowPlain bool, logger *log.Logger) (*ATEN, error) {
	if logger == nil {
		logger = log.Default()
	}
	s, logout, e := aten.Open(ctx, host, user, pw, insecure, allowPlain)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(ctx)
	h := &ATEN{session: s, logger: logger, cancel: cancel, done: make(chan struct{}), logout: logout, frame: image.NewRGBA(image.Rect(0, 0, 640, 480)), held: map[byte]bool{}}
	go func() { e := s.Run(ctx, h); h.mu.Lock(); h.err = e; h.mu.Unlock(); close(h.done) }()
	return h, nil
}

// UpdateScreen implements the aten display sink.
func (h *ATEN) UpdateScreen(w, height int, pix []byte) {
	f := image.NewRGBA(image.Rect(0, 0, w, height))
	copy(f.Pix, pix)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.frame = f
	h.rev++
}

// SetStatus implements the aten display sink.
func (h *ATEN) SetStatus(s string) {
	if s != "" {
		h.logger.Printf("ATEN: %s", s)
	}
}

// SetFPS implements the aten display sink.
func (h *ATEN) SetFPS(int) {}

func (h *ATEN) Frame() (*image.RGBA, uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.frame, h.rev, h.err
}

func (h *ATEN) Key(sym uint32, down bool) error {
	k := HIDUsage(sym)
	if k == 0 {
		return nil
	}
	if down {
		h.held[k] = true
	} else {
		delete(h.held, k)
	}
	return h.session.SendKeyEvent(down, uint16(k))
}

func (h *ATEN) Pointer(x, y int, mask byte) error {
	h.x, h.y = x, y
	return h.session.SendPointerEvent(mask, uint16(x), uint16(y))
}

// Close releases held input, stops the decoder and logs out.
func (h *ATEN) Close() {
	for k := range h.held {
		_ = h.session.SendKeyEvent(false, uint16(k))
	}
	_ = h.Pointer(h.x, h.y, 0)
	h.cancel()
	<-h.done
	h.logout()
}
