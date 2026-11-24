// Package broker multiplexes BMC console sessions behind a single loopback
// VNC listener, for use as a sidecar next to Apache Guacamole's guacd.
//
// A trusted caller (for example a web application that already authorised
// the user) requests a ticket over the HTTP API with the BMC address and
// credentials. The broker answers with a one-time 8-character VNC password.
// The caller hands guacd a VNC connection to the broker's loopback listener
// with that password. When guacd authenticates, the broker finds the ticket
// whose password matches the classic VNC authentication response, consumes
// it and opens that BMC's console. BMC credentials therefore stay in the
// broker's memory and never reach Guacamole's connection records.
package broker

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"github.com/dateo/bmc-console/pkg/bridge"
	"github.com/dateo/bmc-console/pkg/browser"
)

// Errors returned by Issue. The HTTP API maps them to status codes.
var (
	ErrHostNotAllowed = errors.New("broker: host is not in an allowed network")
	ErrBusy           = errors.New("broker: a session or ticket for this BMC already exists")
	ErrTooMany        = errors.New("broker: too many pending tickets or active sessions")
	ErrNotPermitted   = errors.New("broker: option not permitted by broker configuration")
	ErrInvalid        = errors.New("broker: invalid ticket request")
)

// Config configures a Broker. The zero value is not usable: at least one
// AllowedNetworks entry or AllowAnyHost is required.
type Config struct {
	// AllowedNetworks lists the networks BMC addresses must fall into. A
	// hostname is resolved and every address it resolves to must match.
	AllowedNetworks []netip.Prefix
	// AllowAnyHost disables the network check. Only for tests and labs.
	AllowAnyHost bool
	// AllowInsecureTLS lets tickets request InsecureSkipVerify.
	AllowInsecureTLS bool
	// AllowPlainATEN lets tickets use the unencrypted Supermicro ATEN socket.
	AllowPlainATEN bool
	// AllowDemo lets tickets use bridge.ModeDemo, which needs no BMC. Useful
	// for developing and testing integrations.
	AllowDemo bool
	// Browser enables bridge.ModeBrowser tickets. Nil refuses them.
	Browser *BrowserConfig

	// TicketTTL bounds how long an unused ticket stays valid. Default 60s.
	TicketTTL time.Duration
	// MaxPending bounds outstanding tickets. Default 64.
	MaxPending int
	// MaxSessions bounds concurrent console sessions. Default 16.
	MaxSessions int

	// Open connects to a BMC. Default bridge.Open; tests substitute a fake.
	Open func(context.Context, bridge.Options) (bridge.Backend, error)
	// Resolver resolves BMC hostnames for the network check. Default
	// net.DefaultResolver.
	Resolver interface {
		LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	}
	// Logger receives the audit trail. Default slog.Default().
	Logger *slog.Logger
	// Now is the clock. Default time.Now.
	Now func() time.Time
}

// BrowserConfig configures kiosk browser sessions (bridge.ModeBrowser).
type BrowserConfig struct {
	// Launcher starts the browser containers.
	Launcher browser.Launcher
	// StartTimeout bounds starting a container and reaching its VNC
	// server. Default 90s.
	StartTimeout time.Duration
	// MaxLifetime ends a browser session and removes its container after
	// this long, however active it is. Default 1h.
	MaxLifetime time.Duration
	// Width and Height set the browser screen. Default 1280x800.
	Width, Height int
}

// Request asks for a console ticket.
type Request struct {
	Mode bridge.Mode `json:"mode"`
	// URL is the page a browser ticket opens. Its host must be in
	// AllowedNetworks, like a BMC address. Only for bridge.ModeBrowser.
	URL                string `json:"url,omitempty"`
	Host               string `json:"host"`
	Username           string `json:"username"`
	Password           string `json:"password"`
	InsecureSkipVerify bool   `json:"insecureSkipVerify,omitempty"`
	AllowPlainATEN     bool   `json:"allowPlainAten,omitempty"`
	// User and Asset are opaque audit labels supplied by the caller.
	User  string `json:"user,omitempty"`
	Asset string `json:"asset,omitempty"`
}

// Ticket is the answer to a Request.
type Ticket struct {
	ID          string    `json:"id"`
	VNCPassword string    `json:"vncPassword"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// Session describes an active console session. It carries no secrets.
type Session struct {
	ID        string      `json:"id"`
	Host      string      `json:"host"`
	Mode      bridge.Mode `json:"mode"`
	User      string      `json:"user,omitempty"`
	Asset     string      `json:"asset,omitempty"`
	StartedAt time.Time   `json:"startedAt"`
}

type ticket struct {
	Ticket
	req Request
	// browser is the container a browser ticket starts while it waits for
	// its viewer.
	browser *launch
}

// launch is a browser container being started in the background.
type launch struct {
	password string
	cancel   context.CancelFunc
	done     chan struct{}
	inst     browser.Instance // set before done closes, nil on error
	err      error
}

// discard cancels the launch and removes the container once it is known.
func (l *launch) discard() {
	l.cancel()
	go func() {
		<-l.done
		if l.inst != nil {
			_ = l.inst.Close()
		}
	}()
}

type session struct {
	Session
	cancel context.CancelFunc
}

// Broker issues tickets and serves console sessions. It is safe for
// concurrent use.
type Broker struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	pending  map[string]*ticket  // by ticket ID
	sessions map[string]*session // by ticket ID
	hosts    map[string]string   // BMC host -> ticket ID holding it
}

// New validates cfg, fills in defaults and returns a Broker.
func New(cfg Config) (*Broker, error) {
	if len(cfg.AllowedNetworks) == 0 && !cfg.AllowAnyHost {
		return nil, errors.New("broker: configure AllowedNetworks or set AllowAnyHost")
	}
	if cfg.TicketTTL <= 0 {
		cfg.TicketTTL = time.Minute
	}
	if cfg.MaxPending <= 0 {
		cfg.MaxPending = 64
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 16
	}
	if cfg.Open == nil {
		cfg.Open = bridge.Open
	}
	if bc := cfg.Browser; bc != nil {
		if bc.Launcher == nil {
			return nil, errors.New("broker: Browser needs a Launcher")
		}
		if bc.StartTimeout <= 0 {
			bc.StartTimeout = 90 * time.Second
		}
		if bc.MaxLifetime <= 0 {
			bc.MaxLifetime = time.Hour
		}
	}
	if cfg.Resolver == nil {
		cfg.Resolver = net.DefaultResolver
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Broker{
		cfg:      cfg,
		log:      cfg.Logger,
		pending:  map[string]*ticket{},
		sessions: map[string]*session{},
		hosts:    map[string]string{},
	}, nil
}

// Issue validates r and returns a one-time ticket for it.
func (b *Broker) Issue(ctx context.Context, r Request) (Ticket, error) {
	demo := r.Mode == bridge.ModeDemo
	web := r.Mode == bridge.ModeBrowser
	// Like demo tickets, browser tickets do not hold their host exclusively:
	// several people may browse the same device.
	shared := demo || web
	switch {
	case demo && !b.cfg.AllowDemo:
		return Ticket{}, fmt.Errorf("%w: demo mode", ErrNotPermitted)
	case demo:
		r = Request{Mode: bridge.ModeDemo, User: r.User, Asset: r.Asset}
	case web && b.cfg.Browser == nil:
		return Ticket{}, fmt.Errorf("%w: browser mode", ErrNotPermitted)
	case web:
		u, err := browser.ValidateURL(r.URL)
		if err != nil {
			return Ticket{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		r = Request{Mode: bridge.ModeBrowser, URL: u.String(), Host: u.Host, User: r.User, Asset: r.Asset}
	case r.Mode != bridge.ModeBMC && r.Mode != bridge.ModeATEN:
		return Ticket{}, fmt.Errorf("%w: mode must be %q, %q or %q", ErrInvalid, bridge.ModeBMC, bridge.ModeATEN, bridge.ModeBrowser)
	case r.Host == "" || r.Username == "" || r.Password == "":
		return Ticket{}, fmt.Errorf("%w: host, username and password are required", ErrInvalid)
	case r.InsecureSkipVerify && !b.cfg.AllowInsecureTLS:
		return Ticket{}, fmt.Errorf("%w: insecureSkipVerify", ErrNotPermitted)
	case r.AllowPlainATEN && !b.cfg.AllowPlainATEN:
		return Ticket{}, fmt.Errorf("%w: allowPlainAten", ErrNotPermitted)
	}
	if !demo {
		if err := b.checkHost(ctx, r.Host); err != nil {
			b.log.Warn("ticket refused", "reason", err.Error(), "host", r.Host, "user", r.User, "asset", r.Asset)
			return Ticket{}, err
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.purgeLocked()
	if len(b.pending) >= b.cfg.MaxPending || len(b.sessions) >= b.cfg.MaxSessions {
		return Ticket{}, ErrTooMany
	}
	if _, busy := b.hosts[r.Host]; busy && !shared {
		b.log.Warn("ticket refused", "reason", "busy", "host", r.Host, "user", r.User, "asset", r.Asset)
		return Ticket{}, ErrBusy
	}
	t := &ticket{req: r}
	t.ID = "t_" + randomString(16)
	for {
		t.VNCPassword = randomString(bridge.MaxPasswordLen)
		if !b.passwordInUseLocked(t.VNCPassword) {
			break
		}
	}
	t.ExpiresAt = b.cfg.Now().Add(b.cfg.TicketTTL)
	b.pending[t.ID] = t
	if !shared {
		b.hosts[r.Host] = t.ID
	}
	if web {
		t.browser = b.startBrowser(t)
	}
	b.log.Info("ticket issued", "ticket", t.ID, "host", r.Host, "mode", string(r.Mode), "user", r.User, "asset", r.Asset, "expires", t.ExpiresAt)
	return t.Ticket, nil
}

// Sessions lists the active console sessions.
func (b *Broker) Sessions() []Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Session, 0, len(b.sessions))
	for _, s := range b.sessions {
		out = append(out, s.Session)
	}
	return out
}

// Close ends the session or revokes the pending ticket with the given ID.
// It reports whether anything was found.
func (b *Broker) Close(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.sessions[id]; ok {
		s.cancel()
		b.log.Info("session terminated", "ticket", id, "host", s.Host, "user", s.User, "asset", s.Asset)
		return true
	}
	if t, ok := b.pending[id]; ok {
		b.dropTicketLocked(t)
		b.log.Info("ticket revoked", "ticket", id, "host", t.req.Host)
		return true
	}
	return false
}

// ServeVNC accepts viewers (normally guacd) on l until ctx ends or l fails.
// l should be a loopback listener: classic VNC authentication does not
// encrypt the session. Active sessions end when ctx ends.
func (b *Broker) ServeVNC(ctx context.Context, l net.Listener) error {
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Go(func() {
			defer c.Close()
			b.serveViewer(ctx, c)
		})
	}
}

func (b *Broker) serveViewer(ctx context.Context, c net.Conn) {
	var claimed *ticket
	auth := func(challenge, response [16]byte) (bridge.OpenFunc, bool) {
		claimed = b.claim(challenge, response, c)
		if claimed == nil {
			return nil, false
		}
		return func(ctx context.Context) (bridge.Backend, error) {
			return b.openSession(ctx, c, claimed)
		}, true
	}
	err := bridge.ServeConnAuth(ctx, c, auth)
	if claimed == nil {
		if errors.Is(err, bridge.ErrAuthFailed) {
			b.log.Warn("viewer rejected", "remote", c.RemoteAddr().String())
		}
		return
	}
	b.endSession(claimed, err)
}

// claim finds and consumes the pending ticket whose password produced
// response. Every pending ticket is checked so the time taken does not depend
// on which one matches.
func (b *Broker) claim(challenge, response [16]byte, c net.Conn) *ticket {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.purgeLocked()
	var found *ticket
	for _, t := range b.pending {
		want := bridge.VNCResponse(t.VNCPassword, challenge)
		if subtle.ConstantTimeCompare(want[:], response[:]) == 1 {
			found = t
		}
	}
	if found == nil {
		return nil
	}
	if len(b.sessions) >= b.cfg.MaxSessions {
		b.log.Warn("session refused", "reason", "max sessions", "ticket", found.ID, "host", found.req.Host)
		return nil
	}
	delete(b.pending, found.ID)
	b.sessions[found.ID] = &session{Session: Session{
		ID: found.ID, Host: found.req.Host, Mode: found.req.Mode,
		User: found.req.User, Asset: found.req.Asset, StartedAt: b.cfg.Now(),
	}, cancel: func() { c.Close() }}
	return found
}

func (b *Broker) openSession(ctx context.Context, c net.Conn, t *ticket) (bridge.Backend, error) {
	ctx, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	if s, ok := b.sessions[t.ID]; ok {
		s.cancel = func() { cancel(); c.Close() }
	}
	b.mu.Unlock()
	b.log.Info("session starting", "ticket", t.ID, "host", t.req.Host, "mode", string(t.req.Mode), "user", t.req.User, "asset", t.req.Asset)
	if t.browser != nil {
		backend, err := b.openBrowser(ctx, c, t)
		if err != nil {
			cancel()
			return nil, err
		}
		return cancelOnClose{Backend: backend, cancel: cancel}, nil
	}
	backend, err := b.cfg.Open(ctx, bridge.Options{
		Mode: t.req.Mode, Host: t.req.Host, Username: t.req.Username, Password: t.req.Password,
		InsecureSkipVerify: t.req.InsecureSkipVerify, AllowPlainATEN: t.req.AllowPlainATEN,
		Logger: slog.NewLogLogger(b.log.Handler(), slog.LevelInfo),
	})
	if err != nil {
		cancel()
		return nil, err
	}
	return cancelOnClose{Backend: backend, cancel: cancel}, nil
}

// cancelOnClose ends the session context when the bridge closes the backend.
type cancelOnClose struct {
	bridge.Backend
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() {
	c.Backend.Close()
	c.cancel()
}

func (b *Broker) endSession(t *ticket, err error) {
	b.mu.Lock()
	s := b.sessions[t.ID]
	delete(b.sessions, t.ID)
	if b.hosts[t.req.Host] == t.ID {
		delete(b.hosts, t.req.Host)
	}
	b.mu.Unlock()
	attrs := []any{"ticket", t.ID, "host", t.req.Host, "user", t.req.User, "asset", t.req.Asset}
	if s != nil {
		attrs = append(attrs, "duration", b.cfg.Now().Sub(s.StartedAt).Round(time.Second).String())
	}
	if err != nil && !isNormalEnd(err) {
		attrs = append(attrs, "error", err.Error())
	}
	b.log.Info("session ended", attrs...)
}

func isNormalEnd(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNRESET)
}

// purgeLocked drops expired tickets.
func (b *Broker) purgeLocked() {
	now := b.cfg.Now()
	for _, t := range b.pending {
		if !now.Before(t.ExpiresAt) {
			b.dropTicketLocked(t)
			b.log.Info("ticket expired", "ticket", t.ID, "host", t.req.Host, "user", t.req.User, "asset", t.req.Asset)
		}
	}
}

func (b *Broker) dropTicketLocked(t *ticket) {
	if t.browser != nil {
		t.browser.discard()
	}
	delete(b.pending, t.ID)
	if b.hosts[t.req.Host] == t.ID {
		delete(b.hosts, t.req.Host)
	}
}

func (b *Broker) passwordInUseLocked(pw string) bool {
	for _, t := range b.pending {
		if t.VNCPassword == pw {
			return true
		}
	}
	return false
}

// checkHost enforces AllowedNetworks. Hostnames are resolved and all their
// addresses must be allowed. The BMC connection resolves the name again, so
// the check assumes DNS for management networks is trusted.
func (b *Broker) checkHost(ctx context.Context, host string) error {
	if b.cfg.AllowAnyHost {
		return nil
	}
	h := host
	if hp, _, err := net.SplitHostPort(host); err == nil {
		h = hp
	}
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(h); err == nil {
		addrs = []netip.Addr{a}
	} else {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		addrs, err = b.cfg.Resolver.LookupNetIP(ctx, "ip", h)
		if err != nil || len(addrs) == 0 {
			return fmt.Errorf("%w: cannot resolve %q", ErrHostNotAllowed, h)
		}
	}
	for _, a := range addrs {
		a = a.Unmap()
		allowed := false
		for _, p := range b.cfg.AllowedNetworks {
			if p.Contains(a) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("%w: %s", ErrHostNotAllowed, a)
		}
	}
	return nil
}

const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// randomString returns n characters from an unambiguous 57-character
// alphabet (about 5.8 bits each), drawn without modulo bias.
func randomString(n int) string {
	out := make([]byte, 0, n)
	var buf [32]byte
	for len(out) < n {
		if _, err := rand.Read(buf[:]); err != nil {
			panic(err) // crypto/rand never fails on supported platforms
		}
		for _, v := range buf {
			if int(v) < 256-256%len(alphabet) && len(out) < n {
				out = append(out, alphabet[int(v)%len(alphabet)])
			}
		}
	}
	return string(out)
}

// startBrowser starts the container for a browser ticket in the background,
// so it is usually ready by the time the viewer connects.
func (b *Broker) startBrowser(t *ticket) *launch {
	bc := b.cfg.Browser
	ctx, cancel := context.WithTimeout(context.Background(), bc.StartTimeout)
	l := &launch{password: randomString(bridge.MaxPasswordLen), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		defer cancel()
		l.inst, l.err = bc.Launcher.Launch(ctx, browser.Spec{
			URL: t.req.URL, Password: l.password, Session: t.ID,
			Width: bc.Width, Height: bc.Height,
			// The platform's own limit is a backstop: the broker ends the
			// session at MaxLifetime; this removes a container it lost.
			MaxLifetime: bc.MaxLifetime + bc.StartTimeout + b.cfg.TicketTTL,
		})
		if l.err != nil {
			b.log.Warn("browser start failed", "ticket", t.ID, "host", t.req.Host, "error", l.err.Error())
		}
	}()
	return l
}

// openBrowser waits for the ticket's container and connects to its VNC
// server. The session ends at MaxLifetime by closing the viewer connection.
func (b *Broker) openBrowser(ctx context.Context, viewer net.Conn, t *ticket) (bridge.Backend, error) {
	bc := b.cfg.Browser
	l := t.browser
	ctx, cancel := context.WithTimeout(ctx, bc.StartTimeout)
	defer cancel()
	select {
	case <-l.done:
	case <-ctx.Done():
		l.discard()
		return nil, fmt.Errorf("broker: browser did not start: %w", ctx.Err())
	}
	if l.err != nil {
		return nil, fmt.Errorf("broker: browser did not start: %w", l.err)
	}
	client, err := browser.DialRetry(ctx, l.inst.Addr(), l.password, 250*time.Millisecond)
	if err != nil {
		_ = l.inst.Close()
		return nil, err
	}
	limit := time.AfterFunc(bc.MaxLifetime, func() {
		b.log.Info("browser session reached its maximum lifetime", "ticket", t.ID, "host", t.req.Host)
		viewer.Close()
	})
	client.OnClose(func() {
		limit.Stop()
		if err := l.inst.Close(); err != nil {
			b.log.Warn("browser container removal failed", "ticket", t.ID, "error", err.Error())
		}
	})
	b.log.Info("browser connected", "ticket", t.ID, "host", t.req.Host, "addr", l.inst.Addr())
	return client, nil
}
