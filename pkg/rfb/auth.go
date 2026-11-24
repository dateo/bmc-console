package rfb

import (
	"crypto/des"
	"math/bits"
)

// Security types this client can negotiate (RFC 6143 section 7.2).
const (
	securityNone    = 1
	securityVNCAuth = 2
)

// MaxPasswordLen is the number of password bytes classic VNC authentication
// uses. Longer passwords are silently truncated by every implementation.
const MaxPasswordLen = 8

// VNCAuthResponse computes the reply to a classic VNC authentication
// challenge: DES-ECB over the 16-byte challenge, keyed with the first 8
// password bytes, zero-padded and bit-reversed per byte (RFC 6143 section
// 7.2.2 and the historical implementations it documents).
func VNCAuthResponse(password string, challenge [16]byte) [16]byte {
	var key [8]byte
	copy(key[:], password)
	for i := range key {
		key[i] = bits.Reverse8(key[i])
	}
	c, err := des.NewCipher(key[:])
	if err != nil {
		panic(err) // unreachable: the key is always 8 bytes
	}
	var out [16]byte
	c.Encrypt(out[:8], challenge[:8])
	c.Encrypt(out[8:], challenge[8:])
	return out
}

// SetPassword enables classic VNC authentication for Handshake. Without a
// password the client only accepts servers that offer security type None.
func (c *Conn) SetPassword(password string) {
	c.password = password
	c.hasPassword = true
}

// answerVNCAuth reads the server's challenge and sends the response.
func (c *Conn) answerVNCAuth() error {
	raw, err := c.read(16)
	if err != nil {
		return err
	}
	var challenge [16]byte
	copy(challenge[:], raw)
	response := VNCAuthResponse(c.password, challenge)
	_, err = c.write(response[:])
	return err
}
