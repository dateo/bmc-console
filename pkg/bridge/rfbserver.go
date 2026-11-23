package bridge

import (
	"bufio"
	"bytes"
	"crypto/des"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"math/bits"
	"net"
	"time"
)

var be = binary.BigEndian
var nativeFormat = [16]byte{32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0}

// changedBounds finds the smallest enclosing rectangle of modified pixels.
func changedBounds(a, b *image.RGBA) image.Rectangle {
	if a == nil || a.Rect != b.Rect {
		return b.Rect
	}
	minX, minY, maxX, maxY := b.Rect.Max.X, b.Rect.Max.Y, 0, 0
	for y := 0; y < b.Rect.Dy(); y++ {
		start, end := y*b.Stride, y*b.Stride+b.Rect.Dx()*4
		row, prev := b.Pix[start:end], a.Pix[start:end]
		if bytes.Equal(row, prev) {
			continue
		}
		left, right := 0, b.Rect.Dx()-1
		for bytes.Equal(row[left*4:left*4+4], prev[left*4:left*4+4]) {
			left++
		}
		for bytes.Equal(row[right*4:right*4+4], prev[right*4:right*4+4]) {
			right--
		}
		minX, minY = min(minX, left), min(minY, y)
		maxX, maxY = max(maxX, right+1), y+1
	}
	if minX >= maxX {
		return image.Rectangle{}
	}
	return image.Rect(minX, minY, maxX, maxY)
}

type viewer struct {
	previous *image.RGBA
	net.Conn
	in       *bufio.Reader
	format   [16]byte
	size     image.Point
	resize   bool
	revision uint64
	full     bool
}

func (v *viewer) read(n int) ([]byte, error) {
	p := make([]byte, n)
	_, err := io.ReadFull(v.in, p)
	return p, err
}
func (v *viewer) send(p []byte) error {
	v.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := v.Write(p)
	return err
}

// Authenticator checks a classic VNC authentication response against the
// 16-byte challenge sent to the viewer. On success it returns the opener for
// the viewer's backend; ok=false denies the viewer.
type Authenticator func(challenge, response [16]byte) (open OpenFunc, ok bool)

// VNCResponse computes the reply a viewer sends for password and challenge:
// DES-ECB with the bit-reversed, zero-padded first 8 password bytes as key.
func VNCResponse(password string, challenge [16]byte) [16]byte {
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

// PasswordAuth returns an Authenticator that accepts one fixed password.
func PasswordAuth(password string, open OpenFunc) Authenticator {
	return func(challenge, response [16]byte) (OpenFunc, bool) {
		want := VNCResponse(password, challenge)
		if subtle.ConstantTimeCompare(want[:], response[:]) != 1 {
			return nil, false
		}
		return open, true
	}
}

// handshake runs the RFB version and security exchange. With a nil auth the
// viewer gets security type None and noAuth is returned; otherwise classic VNC
// authentication decides which opener the viewer gets.
func (v *viewer) handshake(auth Authenticator, noAuth OpenFunc) (OpenFunc, error) {
	if err := v.send([]byte("RFB 003.008\n")); err != nil {
		return nil, err
	}
	p, e := v.read(12)
	if e != nil {
		return nil, e
	}
	ver := string(p)
	if ver != "RFB 003.003\n" && ver != "RFB 003.007\n" && ver != "RFB 003.008\n" {
		return nil, errors.New("unsupported RFB version")
	}
	security := byte(1)
	if auth != nil {
		security = 2
	}
	if ver == "RFB 003.003\n" {
		e = v.send([]byte{0, 0, 0, security})
	} else if e = v.send([]byte{1, security}); e == nil {
		var t byte
		t, e = v.in.ReadByte()
		if e == nil && t != security {
			return nil, errors.New("unsupported security selection")
		}
	}
	if e != nil {
		return nil, e
	}
	open := noAuth
	if security == 2 {
		var challenge, reply [16]byte
		if _, e = rand.Read(challenge[:]); e != nil {
			return nil, e
		}
		if e = v.send(challenge[:]); e != nil {
			return nil, e
		}
		if _, e = io.ReadFull(v.in, reply[:]); e != nil {
			return nil, e
		}
		var ok bool
		if open, ok = auth(challenge, reply); !ok {
			failure := []byte{0, 0, 0, 1}
			if ver == "RFB 003.008\n" {
				failure = append(failure, 0, 0, 0, 6)
				failure = append(failure, "denied"...)
			}
			_ = v.send(failure)
			return nil, ErrAuthFailed
		}
	}
	if security == 2 || ver == "RFB 003.008\n" {
		if e = v.send([]byte{0, 0, 0, 0}); e != nil {
			return nil, e
		}
	}
	_, e = v.in.ReadByte()
	return open, e
}
func (v *viewer) init(frame *image.RGBA) error {
	v.format = nativeFormat
	v.size = frame.Bounds().Size()
	name := "BMC console"
	p := make([]byte, 24)
	be.PutUint16(p, uint16(v.size.X))
	be.PutUint16(p[2:], uint16(v.size.Y))
	copy(p[4:], v.format[:])
	be.PutUint32(p[20:], uint32(len(name)))
	return v.send(append(p, name...))
}
func validFormat(p []byte) bool {
	if (p[0] != 8 && p[0] != 16 && p[0] != 32) || p[3] != 1 || p[2] > 1 || p[1] > p[0] {
		return false
	}
	var used uint64
	for i := range 3 {
		m := uint64(be.Uint16(p[4+2*i:]))
		s := p[10+i]
		if m == 0 || m&(m+1) != 0 || s >= p[0] {
			return false
		}
		mask := m << s
		if mask>>p[0] != 0 || mask&used != 0 {
			return false
		}
		used |= mask
	}
	return true
}
func (v *viewer) update(b Backend, p []byte) error {
	f, rev, e := b.Frame()
	if e != nil {
		return e
	}
	sz := f.Bounds().Size()
	if sz != v.size {
		if !v.resize {
			return errors.New("viewer must reconnect after resolution change (no DesktopSize)")
		}
		v.size = sz
		v.full = false
		packet := make([]byte, 16)
		packet[3] = 1
		be.PutUint16(packet[8:], uint16(sz.X))
		be.PutUint16(packet[10:], uint16(sz.Y))
		be.PutUint32(packet[12:], 0xffffff21)
		return v.send(packet)
	}
	x, y := int(be.Uint16(p[1:])), int(be.Uint16(p[3:]))
	r := image.Rect(x, y, x+int(be.Uint16(p[5:])), y+int(be.Uint16(p[7:]))).Intersect(f.Rect)
	if r.Empty() || (p[0] != 0 && rev == v.revision && v.full) {
		return v.send([]byte{0, 0, 0, 0})
	}
	whole := r == f.Rect
	if whole {
		dirty := r
		if p[0] != 0 && v.full {
			dirty = changedBounds(v.previous, f)
		}
		if v.previous == nil || v.previous.Rect != f.Rect {
			v.previous = image.NewRGBA(f.Rect)
		}
		copy(v.previous.Pix, f.Pix)
		r = dirty
		if r.Empty() {
			v.revision = rev
			return v.send([]byte{0, 0, 0, 0})
		}
	}
	size := int(v.format[0]) / 8
	out := make([]byte, 16+r.Dx()*r.Dy()*size)
	out[3] = 1
	for i, n := range []int{r.Min.X, r.Min.Y, r.Dx(), r.Dy()} {
		be.PutUint16(out[4+2*i:], uint16(n))
	}
	idx := 16
	for y := r.Min.Y; y < r.Max.Y; y++ {
		off := f.PixOffset(r.Min.X, y)
		for x := 0; x < r.Dx(); x++ {
			var val uint32
			for c := range 3 {
				max := uint32(be.Uint16(v.format[4+2*c:]))
				val |= (uint32(f.Pix[off+c]) * max / 255) << v.format[10+c]
			}
			for k := range size {
				shift := k
				if v.format[2] != 0 {
					shift = size - 1 - k
				}
				out[idx+k] = byte(val >> (8 * shift))
			}
			off += 4
			idx += size
		}
	}
	if rev != v.revision {
		v.full = false
	}
	v.revision = rev
	v.full = v.full || whole
	return v.send(out)
}
func (v *viewer) serve(b Backend) error {
	f, _, e := b.Frame()
	if e != nil {
		return e
	}
	if e = v.init(f); e != nil {
		return e
	}
	var next time.Time
	for {
		op, err := v.in.ReadByte()
		if err != nil {
			return err
		}
		switch op {
		case 0:
			p, e := v.read(19)
			if e != nil {
				return e
			}
			if !validFormat(p[3:]) {
				return errors.New("unsupported pixel format")
			}
			copy(v.format[:], p[3:])
			v.full = false
		case 2:
			p, e := v.read(3)
			if e != nil {
				return e
			}
			n := int(be.Uint16(p[1:]))
			if n > 512 {
				return errors.New("too many encodings")
			}
			p, e = v.read(4 * n)
			if e != nil {
				return e
			}
			v.resize = false
			for len(p) > 0 {
				if bytes.Equal(p[:4], []byte{255, 255, 255, 33}) {
					v.resize = true
				}
				p = p[4:]
			}
		case 3:
			p, e := v.read(9)
			if e != nil {
				return e
			}
			if delay := time.Until(next); delay > 0 {
				time.Sleep(delay)
			}
			next = time.Now().Add(60 * time.Millisecond)
			if e = v.update(b, p); e != nil {
				return e
			}
		case 4:
			p, e := v.read(7)
			if e != nil {
				return e
			}
			if e = b.Key(be.Uint32(p[3:]), p[0] != 0); e != nil {
				return e
			}
		case 5:
			p, e := v.read(5)
			if e != nil {
				return e
			}
			if e = b.Pointer(int(be.Uint16(p[1:])), int(be.Uint16(p[3:])), p[0]); e != nil {
				return e
			}
		case 6:
			p, e := v.read(7)
			if e != nil {
				return e
			}
			n := be.Uint32(p[3:])
			if n > 1<<20 {
				return errors.New("clipboard too large")
			}
			if _, e = io.CopyN(io.Discard, v.in, int64(n)); e != nil {
				return e
			}
		default:
			return fmt.Errorf("unsupported viewer message %d", op)
		}
	}
}
