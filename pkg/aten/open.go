package aten

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

func Open(ctx context.Context, host, user, password string, insecure, allowPlain bool) (*Session, func(), error) {
	web := NewWebSession(host, user, password, insecure)
	if err := web.Login(); err != nil {
		return nil, nil, err
	}
	ok := false
	defer func() {
		if !ok {
			web.Logout()
		}
	}()
	name, _, err := net.SplitHostPort(host)
	if err != nil {
		name = host
	}
	ticket, err := web.FetchTicket(name)
	if err != nil {
		return nil, nil, err
	}
	if ticket.TLS || !allowPlain {
		return nil, nil, errors.New("this Hermon adapter requires an existing plaintext KVM service and explicit -allow-plain-aten")
	}
	var d net.Dialer
	d.Timeout = 5 * time.Second
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(name, strconv.Itoa(ticket.Port)))
	if err != nil {
		return nil, nil, err
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(c)
	info, err := handshake(r, c, ticket)
	if err != nil || !info.Video || !info.KeyboardMouse {
		c.Close()
		return nil, nil, fmt.Errorf("ATEN console unavailable (handshake=%v video=%v control=%v)", err, info.Video, info.KeyboardMouse)
	}
	c.SetDeadline(time.Time{})
	ok = true
	return newSession(c, r, info), web.Logout, nil
}
