// Command bmc-vnc serves one BMC's graphical console to a VNC viewer or
// Apache Guacamole on a loopback address.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/dateo/bmc-console/pkg/bridge"
)

func main() {
	mode := flag.String("mode", "demo", "demo, bmc (HPE iLO / Dell iDRAC), or aten")
	host := flag.String("host", "", "BMC hostname or address")
	listen := flag.String("listen", "127.0.0.1:5901", "loopback address for one viewer")
	insecure := flag.Bool("insecure-bmc-tls", false, "accept an untrusted BMC HTTPS certificate")
	plain := flag.Bool("allow-plain-aten", false, "permit ATEN plaintext KVM on a trusted management LAN")
	flag.Parse()
	ip, _, e := net.SplitHostPort(*listen)
	if e != nil || !net.ParseIP(ip).IsLoopback() {
		log.Fatal("listen requires a numeric loopback address")
	}
	user, pw, vpw := os.Getenv("BMC_USER"), os.Getenv("BMC_PASSWORD"), os.Getenv("VNC_PASSWORD")
	m := bridge.Mode(*mode)
	if m != bridge.ModeDemo && m != bridge.ModeBMC && m != bridge.ModeATEN {
		log.Fatal("unknown mode")
	}
	if m != bridge.ModeDemo && (*host == "" || pw == "" || user == "" || len(vpw) != bridge.MaxPasswordLen) {
		log.Fatal("hardware mode requires -host, BMC_USER, BMC_PASSWORD and an 8-byte VNC_PASSWORD")
	}
	if len(vpw) > bridge.MaxPasswordLen {
		log.Fatal("VNC authentication accepts at most 8 password bytes")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	l, e := net.Listen("tcp", *listen)
	if e != nil {
		log.Fatal(e)
	}
	defer l.Close()
	log.Printf("listening on %s (%s)", l.Addr(), m)
	opts := bridge.Options{Mode: m, Host: *host, Username: user, Password: pw, InsecureSkipVerify: *insecure, AllowPlainATEN: *plain}
	srv := &bridge.Server{
		Password: vpw,
		Open:     func(ctx context.Context) (bridge.Backend, error) { return bridge.Open(ctx, opts) },
	}
	if e := srv.Serve(ctx, l); e != nil {
		log.Print(e)
	}
}
