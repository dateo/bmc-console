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

	brk, err := broker.New(broker.Config{
		AllowedNetworks: nets, AllowAnyHost: *anyHost,
		AllowInsecureTLS: *insecure, AllowPlainATEN: *plain, AllowDemo: *demo,
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
	logger.Info("bmc-broker started", "vnc", vl.Addr().String(), "api", *apiListen, "networks", nets.String(), "tls", *tlsCert != "")

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
