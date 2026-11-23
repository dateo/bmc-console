package bridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/dateo/bmc-console/pkg/console"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestILO4ConsoleSession(t *testing.T) {
	const token = "00112233445566778899aabbccddeeff"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	negotiated := make(chan uint16, 2)
	for i := 0; i < 2; i++ {
		go func() {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			_, _ = c.Write([]byte{0x50})
			var req [34]byte
			if _, e := io.ReadFull(c, req[:]); e != nil || string(req[2:]) != token {
				return
			}
			negotiated <- binary.LittleEndian.Uint16(req[:2])
			_, _ = c.Write([]byte{0x52})
			_, _ = io.Copy(io.Discard, c)
		}()
	}
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json/login_session":
			fmt.Fprintf(w, `{"session_key":"%s"}`, token)
		case "/json/rc_info":
			cookie, e := r.Cookie("sessionKey")
			if e != nil || cookie.Value != token {
				http.Error(w, "invalid session", 403)
				return
			}
			fmt.Fprintf(w, `{"protocol_version":"1.1","enc_key":"%s","rc_port":%d}`, token, listener.Addr().(*net.TCPAddr).Port)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, e := console.OpenBridge(ctx, console.OpenOptions{Address: strings.TrimPrefix(api.URL, "https://"), Username: "fixture", Password: "fixture", InsecureSkipVerify: true})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if st := s.State(); st.ProtocolVersion != 1 || !st.Connected {
		t.Fatalf("state %+v", st)
	}
	seen := map[uint16]bool{}
	for i := 0; i < 2; i++ {
		select {
		case n := <-negotiated:
			seen[n] = true
		case <-ctx.Done():
			t.Fatal("missing legacy channel")
		}
	}
	if !seen[0x2001] || !seen[0x2002] {
		t.Fatalf("unexpected channel commands %v", seen)
	}
}
