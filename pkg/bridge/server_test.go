package bridge

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// TestServerDemoSession drives an unauthenticated RFB 3.8 handshake through
// Server against the demo backend and checks ServerInit and a raw update.
func TestServerDemoSession(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	srv := &Server{Open: func(ctx context.Context) (Backend, error) { return NewDemo(), nil }}
	go func() { done <- srv.Serve(ctx, l) }()

	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	read := func(n int) []byte {
		t.Helper()
		p := make([]byte, n)
		if _, err := io.ReadFull(r, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if v := string(read(12)); v != "RFB 003.008\n" {
		t.Fatalf("version %q", v)
	}
	c.Write([]byte("RFB 003.008\n"))
	if p := read(2); p[0] != 1 || p[1] != 1 {
		t.Fatalf("security types %v", p)
	}
	c.Write([]byte{1})
	if p := read(4); binary.BigEndian.Uint32(p) != 0 {
		t.Fatalf("security result %v", p)
	}
	c.Write([]byte{1}) // ClientInit, shared
	init := read(24)
	w, h := binary.BigEndian.Uint16(init), binary.BigEndian.Uint16(init[2:])
	if w != 640 || h != 400 {
		t.Fatalf("framebuffer %dx%d", w, h)
	}
	read(int(binary.BigEndian.Uint32(init[20:])))
	// Non-incremental update of a 2x1 region at the origin.
	c.Write([]byte{3, 0, 0, 0, 0, 0, 0, 2, 0, 1})
	hdr := read(16)
	if hdr[0] != 0 || binary.BigEndian.Uint16(hdr[2:]) != 1 || binary.BigEndian.Uint16(hdr[8:]) != 2 {
		t.Fatalf("update header %v", hdr)
	}
	read(2 * 4)
	c.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

func TestServerRejectsLongPassword(t *testing.T) {
	s := &Server{Password: "123456789", Open: func(context.Context) (Backend, error) { return NewDemo(), nil }}
	if err := s.Serve(context.Background(), nil); err == nil {
		t.Fatal("expected error")
	}
}
