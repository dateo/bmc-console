// Command bmc-broker serves many BMC consoles to guacd behind one loopback
// VNC listener, with one-time session tickets issued over an HTTP API.
// See docs/broker.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dateo/bmc-console/pkg/broker"
	"github.com/dateo/bmc-console/pkg/browser"
)

type prefixes []netip.Prefix

func (p *prefixes) String() string { return fmt.Sprint(*p) }
func (p *prefixes) Set(v string) error {
	for s := range strings.SplitSeq(v, ",") {
		pr, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil {
			return err
		}
		*p = append(*p, pr.Masked())
	}
	return nil
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// browserFlags configure kiosk browser sessions (docs/browser.md).
type browserFlags struct {
	launcher, image, namespace, runtimeClass, seccomp, apparmor, size string
	noSandbox, ignoreCertErrors                                       bool
	maxLifetime, startTimeout                                         time.Duration
	labels, dockerArgs                                                multi
}

func (f *browserFlags) register() {
	flag.StringVar(&f.launcher, "browser-launcher", "", "enable browser tickets: kubernetes or docker (empty: off)")
	flag.StringVar(&f.image, "browser-image", "", "kiosk browser image (built from browser/)")
	flag.StringVar(&f.namespace, "browser-namespace", "", "namespace for browser pods (default: the broker's own)")
	flag.StringVar(&f.runtimeClass, "browser-runtime-class", "", "runtimeClassName for browser pods, e.g. gvisor")
	flag.StringVar(&f.seccomp, "browser-seccomp-profile", "", "node-local seccomp profile for browser pods (default RuntimeDefault)")
	flag.StringVar(&f.apparmor, "browser-apparmor-profile", "", "node-local AppArmor profile for browser pods; must allow userns where the host restricts it")
	flag.StringVar(&f.size, "browser-size", "1280x800", "browser screen size")
	flag.BoolVar(&f.noSandbox, "browser-no-sandbox", false, "disable Chromium's sandbox (only under a sandboxing runtime class)")
	flag.BoolVar(&f.ignoreCertErrors, "browser-ignore-cert-errors", false, "let the browser accept self-signed device certificates")
	flag.DurationVar(&f.maxLifetime, "browser-max-lifetime", time.Hour, "hard limit of one browser session")
	flag.DurationVar(&f.startTimeout, "browser-start-timeout", 90*time.Second, "time to start a browser and reach its VNC server")
	flag.Var(&f.labels, "browser-label", "key=value label on browser pods, e.g. for NetworkPolicy; repeatable")
	flag.Var(&f.dockerArgs, "browser-docker-arg", "extra `docker run` argument for the docker launcher; repeatable")
}

func (f *browserFlags) config() (*broker.BrowserConfig, error) {
	if f.launcher == "" {
		return nil, nil
	}
	if f.image == "" {
		return nil, errors.New("-browser-image is required with -browser-launcher")
	}
	var w, h int
	if _, err := fmt.Sscanf(f.size, "%dx%d", &w, &h); err != nil || w < 320 || h < 200 || w > 4096 || h > 4096 {
		return nil, fmt.Errorf("-browser-size %q: want WIDTHxHEIGHT", f.size)
	}
	env := map[string]string{}
	if f.noSandbox {
		env["BROWSER_NO_SANDBOX"] = "1"
	}
	if f.ignoreCertErrors {
		env["BROWSER_IGNORE_CERT_ERRORS"] = "1"
	}
	cfg := &broker.BrowserConfig{StartTimeout: f.startTimeout, MaxLifetime: f.maxLifetime, Width: w, Height: h}
	switch f.launcher {
	case "docker":
		cfg.Launcher = &browser.Docker{Image: f.image, ExtraArgs: f.dockerArgs, Env: env}
	case "kubernetes":
		k := &browser.Kubernetes{
			Namespace: f.namespace, Image: f.image, RuntimeClassName: f.runtimeClass,
			SeccompLocalhostProfile: f.seccomp, AppArmorLocalhostProfile: f.apparmor,
			Env: env, Labels: map[string]string{},
		}
		for _, l := range f.labels {
			key, v, ok := strings.Cut(l, "=")
			if !ok || key == "" {
				return nil, fmt.Errorf("-browser-label %q: want key=value", l)
			}
			k.Labels[key] = v
		}
		// The downward API names the broker's Pod; browser Pods it owns are
		// garbage collected with it.
		if name, uid := os.Getenv("POD_NAME"), os.Getenv("POD_UID"); name != "" && uid != "" {
			k.OwnerPod = &browser.OwnerRef{Name: name, UID: uid}
		}
		if err := k.InCluster(); err != nil {
			return nil, err
		}
		cfg.Launcher = k
	default:
		return nil, fmt.Errorf("-browser-launcher %q: want kubernetes or docker", f.launcher)
	}
	return cfg, nil
}

func main() {
	var nets prefixes
	vncListen := flag.String("vnc-listen", "127.0.0.1:5901", "loopback address guacd connects to")
	apiListen := flag.String("api-listen", ":8080", "address of the ticket API")
	tlsCert := flag.String("api-tls-cert", "", "TLS certificate for the ticket API (recommended)")
	tlsKey := flag.String("api-tls-key", "", "TLS key for the ticket API")
	tokenFile := flag.String("token-file", "", "file holding the API bearer token (default: $BMC_BROKER_TOKEN)")
	flag.Var(&nets, "allow-net", "CIDR the BMC addresses must be in; repeatable or comma-separated")
	anyHost := flag.Bool("allow-any-host", false, "skip the network check (labs only)")
	insecure := flag.Bool("allow-insecure-bmc-tls", false, "let tickets disable BMC certificate verification")
	plain := flag.Bool("allow-plain-aten", false, "let tickets use the unencrypted Supermicro ATEN socket")
	demo := flag.Bool("allow-demo", false, "let tickets open the demo screen instead of a BMC")
	ttl := flag.Duration("ticket-ttl", time.Minute, "lifetime of an unused ticket")
	maxSessions := flag.Int("max-sessions", 16, "concurrent console sessions")
	maxPending := flag.Int("max-pending", 64, "outstanding tickets")
	logFormat := flag.String("log-format", "json", "json or text")
	var bf browserFlags
	bf.register()
	flag.Parse()

	var handler slog.Handler = slog.NewJSONHandler(os.Stderr, nil)
	if *logFormat == "text" {
		handler = slog.NewTextHandler(os.Stderr, nil)
	}
	logger := slog.New(handler)
	fatal := func(msg string, args ...any) { logger.Error(msg, args...); os.Exit(1) }

	ip, _, err := net.SplitHostPort(*vncListen)
	if err != nil || !net.ParseIP(ip).IsLoopback() {
		fatal("-vnc-listen requires a numeric loopback address")
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		fatal("-api-tls-cert and -api-tls-key go together")
	}
	token := os.Getenv("BMC_BROKER_TOKEN")
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			fatal("read token file", "error", err)
		}
		token = strings.TrimSpace(string(b))
	}

	browserCfg, err := bf.config()
	if err != nil {
		fatal(err.Error())
	}
	brk, err := broker.New(broker.Config{
		AllowedNetworks: nets, AllowAnyHost: *anyHost,
		AllowInsecureTLS: *insecure, AllowPlainATEN: *plain, AllowDemo: *demo,
		Browser:   browserCfg,
		TicketTTL: *ttl, MaxSessions: *maxSessions, MaxPending: *maxPending,
		Logger: logger,
	})
	if err != nil {
		fatal(err.Error())
	}
	api, err := brk.Handler(token)
	if err != nil {
		fatal(err.Error())
	}
	if *anyHost {
		logger.Warn("network check disabled: any host can be targeted")
	}
	if *tlsCert == "" {
		logger.Warn("ticket API without TLS: BMC credentials cross the network in clear text")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	vl, err := net.Listen("tcp", *vncListen)
	if err != nil {
		fatal("vnc listen", "error", err)
	}
	srv := &http.Server{Addr: *apiListen, Handler: api, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- brk.ServeVNC(ctx, vl) }()
	go func() {
		var err error
		if *tlsCert != "" {
			err = srv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			err = srv.ListenAndServe()
		}
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()
	logger.Info("bmc-broker started", "vnc", vl.Addr().String(), "api", *apiListen, "networks", nets.String(), "tls", *tlsCert != "", "browser", bf.launcher)

	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil {
			logger.Error("listener failed", "error", err)
		}
		stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	logger.Info("bmc-broker stopped")
}
