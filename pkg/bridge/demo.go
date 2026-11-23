package bridge

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"strings"
)

// Demo is a synthetic Backend for trying viewers and Guacamole without a BMC.
// It shows a test screen with colour bars, the last key event (keysym and the
// USB HID usage a BMC would receive), the pointer state, and a canvas that
// pointer movement draws on.
type Demo struct {
	frame  *image.RGBA
	canvas *image.RGBA
	rev    uint64

	keyLine     string
	pointerLine string
	updates     int
	drawn       bool
}

const demoW, demoH = 640, 400

var (
	demoCanvasRect = image.Rect(16, 196, demoW-16, demoH-16)
	demoBG         = color.RGBA{16, 24, 38, 255}
	demoPanel      = color.RGBA{26, 38, 58, 255}
	demoAccent     = color.RGBA{38, 132, 255, 255}
	demoText       = color.RGBA{230, 236, 245, 255}
	demoDim        = color.RGBA{120, 136, 160, 255}
	demoBars       = []color.RGBA{
		{235, 235, 235, 255}, {235, 235, 16, 255}, {16, 235, 235, 255}, {16, 235, 16, 255},
		{235, 16, 235, 255}, {235, 16, 16, 255}, {16, 16, 235, 255}, {16, 16, 16, 255},
	}
)

// NewDemo returns a fresh 640x400 demo screen.
func NewDemo() *Demo {
	d := &Demo{
		frame:       image.NewRGBA(image.Rect(0, 0, demoW, demoH)),
		canvas:      image.NewRGBA(image.Rect(0, 0, demoCanvasRect.Dx(), demoCanvasRect.Dy())),
		keyLine:     "PRESS A KEY",
		pointerLine: "MOVE THE POINTER",
	}
	draw.Draw(d.canvas, d.canvas.Bounds(), image.NewUniform(demoPanel), image.Point{}, draw.Src)
	d.render()
	return d
}

func (d *Demo) Frame() (*image.RGBA, uint64, error) { return d.frame, d.rev, nil }

func (d *Demo) Key(sym uint32, down bool) error {
	state := "UP"
	if down {
		state = "DOWN"
	}
	hid := "NONE"
	if u := HIDUsage(sym); u != 0 {
		hid = fmt.Sprintf("%02X", u)
	}
	d.keyLine = fmt.Sprintf("%-9s KEYSYM %04X  HID %-4s %s", keysymLabel(sym), sym, hid, state)
	d.render()
	return nil
}

func (d *Demo) Pointer(x, y int, mask byte) error {
	d.pointerLine = fmt.Sprintf("%d,%d  BUTTONS %d", x, y, mask&7)
	p := image.Pt(x, y)
	if p.In(demoCanvasRect) {
		r, c := 3, color.RGBA{255, 214, 10, 255}
		if mask&7 != 0 {
			r, c = 5, color.RGBA{255, 92, 92, 255}
		}
		p = p.Sub(demoCanvasRect.Min)
		d.drawn = true
		draw.Draw(d.canvas, image.Rect(p.X-r, p.Y-r, p.X+r+1, p.Y+r+1), image.NewUniform(c), image.Point{}, draw.Src)
	}
	d.render()
	return nil
}

func (d *Demo) Close() {}

func (d *Demo) render() {
	d.rev++
	d.updates++
	f := d.frame
	fill(f, f.Bounds(), demoBG)
	fill(f, image.Rect(0, 0, demoW, 44), demoAccent)
	drawText(f, 16, 11, 3, "BMC-CONSOLE DEMO", demoText)
	drawText(f, demoW-16-textWidth(2, "640X400"), 15, 2, "640X400", demoText)

	barW := (demoW - 32) / len(demoBars)
	for i, c := range demoBars {
		fill(f, image.Rect(16+i*barW, 56, 16+(i+1)*barW, 92), c)
	}

	fill(f, image.Rect(16, 104, demoW-16, 184), demoPanel)
	drawText(f, 28, 114, 2, "KEY", demoDim)
	drawText(f, 124, 114, 2, d.keyLine, demoText)
	drawText(f, 28, 138, 2, "POINTER", demoDim)
	drawText(f, 124, 138, 2, d.pointerLine, demoText)
	drawText(f, 28, 162, 2, "UPDATES", demoDim)
	drawText(f, 124, 162, 2, fmt.Sprint(d.updates), demoText)

	draw.Draw(f, demoCanvasRect, d.canvas, image.Point{}, draw.Src)
	if !d.drawn {
		hint := "DRAW HERE"
		drawText(f, demoCanvasRect.Min.X+(demoCanvasRect.Dx()-textWidth(2, hint))/2, demoCanvasRect.Min.Y+demoCanvasRect.Dy()/2-7, 2, hint, demoDim)
	}
}

func fill(img *image.RGBA, r image.Rectangle, c color.RGBA) {
	draw.Draw(img, r, image.NewUniform(c), image.Point{}, draw.Src)
}

func keysymLabel(sym uint32) string {
	names := map[uint32]string{
		0xff08: "BACKSPACE", 0xff09: "TAB", 0xff0d: "ENTER", 0xff1b: "ESC", 0xffff: "DELETE",
		0xff50: "HOME", 0xff51: "LEFT", 0xff52: "UP", 0xff53: "RIGHT", 0xff54: "DOWN",
		0xff55: "PGUP", 0xff56: "PGDN", 0xff57: "END", 0xff63: "INSERT",
		0xffe1: "SHIFT-L", 0xffe2: "SHIFT-R", 0xffe3: "CTRL-L", 0xffe4: "CTRL-R",
		0xffe9: "ALT-L", 0xffea: "ALT-R", 0xffeb: "SUPER-L", 0xffec: "SUPER-R",
	}
	switch {
	case names[sym] != "":
		return names[sym]
	case sym == ' ':
		return "SPACE"
	case sym > ' ' && sym < 0x7f:
		return "'" + strings.ToUpper(string(rune(sym))) + "'"
	case sym >= 0xffbe && sym <= 0xffc9:
		return fmt.Sprintf("F%d", sym-0xffbe+1)
	default:
		return "?"
	}
}

// 5x7 bitmap font: one byte per row, bit 4 is the leftmost column.
var demoFont = map[rune][7]byte{
	'A': {0x0E, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11}, 'B': {0x1E, 0x11, 0x11, 0x1E, 0x11, 0x11, 0x1E},
	'C': {0x0E, 0x11, 0x10, 0x10, 0x10, 0x11, 0x0E}, 'D': {0x1C, 0x12, 0x11, 0x11, 0x11, 0x12, 0x1C},
	'E': {0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x1F}, 'F': {0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x10},
	'G': {0x0E, 0x11, 0x10, 0x17, 0x11, 0x11, 0x0F}, 'H': {0x11, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11},
	'I': {0x0E, 0x04, 0x04, 0x04, 0x04, 0x04, 0x0E}, 'J': {0x07, 0x02, 0x02, 0x02, 0x02, 0x12, 0x0C},
	'K': {0x11, 0x12, 0x14, 0x18, 0x14, 0x12, 0x11}, 'L': {0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x1F},
	'M': {0x11, 0x1B, 0x15, 0x15, 0x11, 0x11, 0x11}, 'N': {0x11, 0x11, 0x19, 0x15, 0x13, 0x11, 0x11},
	'O': {0x0E, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E}, 'P': {0x1E, 0x11, 0x11, 0x1E, 0x10, 0x10, 0x10},
	'Q': {0x0E, 0x11, 0x11, 0x11, 0x15, 0x12, 0x0D}, 'R': {0x1E, 0x11, 0x11, 0x1E, 0x14, 0x12, 0x11},
	'S': {0x0F, 0x10, 0x10, 0x0E, 0x01, 0x01, 0x1E}, 'T': {0x1F, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04},
	'U': {0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E}, 'V': {0x11, 0x11, 0x11, 0x11, 0x11, 0x0A, 0x04},
	'W': {0x11, 0x11, 0x11, 0x15, 0x15, 0x15, 0x0A}, 'X': {0x11, 0x11, 0x0A, 0x04, 0x0A, 0x11, 0x11},
	'Y': {0x11, 0x11, 0x11, 0x0A, 0x04, 0x04, 0x04}, 'Z': {0x1F, 0x01, 0x02, 0x04, 0x08, 0x10, 0x1F},
	'0': {0x0E, 0x11, 0x13, 0x15, 0x19, 0x11, 0x0E}, '1': {0x04, 0x0C, 0x04, 0x04, 0x04, 0x04, 0x0E},
	'2': {0x0E, 0x11, 0x01, 0x02, 0x04, 0x08, 0x1F}, '3': {0x1F, 0x02, 0x04, 0x02, 0x01, 0x11, 0x0E},
	'4': {0x02, 0x06, 0x0A, 0x12, 0x1F, 0x02, 0x02}, '5': {0x1F, 0x10, 0x1E, 0x01, 0x01, 0x11, 0x0E},
	'6': {0x06, 0x08, 0x10, 0x1E, 0x11, 0x11, 0x0E}, '7': {0x1F, 0x01, 0x02, 0x04, 0x08, 0x08, 0x08},
	'8': {0x0E, 0x11, 0x11, 0x0E, 0x11, 0x11, 0x0E}, '9': {0x0E, 0x11, 0x11, 0x0F, 0x01, 0x02, 0x0C},
	' ': {}, ':': {0, 0x0C, 0x0C, 0, 0x0C, 0x0C, 0}, '.': {0, 0, 0, 0, 0, 0x0C, 0x0C},
	',': {0, 0, 0, 0, 0x0C, 0x04, 0x08}, '-': {0, 0, 0, 0x1F, 0, 0, 0},
	'(': {0x02, 0x04, 0x08, 0x08, 0x08, 0x04, 0x02}, ')': {0x08, 0x04, 0x02, 0x02, 0x02, 0x04, 0x08},
	'/': {0, 0x01, 0x02, 0x04, 0x08, 0x10, 0}, '?': {0x0E, 0x11, 0x01, 0x02, 0x04, 0, 0x04},
	'_': {0, 0, 0, 0, 0, 0, 0x1F}, '+': {0, 0x04, 0x04, 0x1F, 0x04, 0x04, 0},
	'=': {0, 0, 0x1F, 0, 0x1F, 0, 0}, '\'': {0x04, 0x04, 0x08, 0, 0, 0, 0},
	'[': {0x0E, 0x08, 0x08, 0x08, 0x08, 0x08, 0x0E}, ']': {0x0E, 0x02, 0x02, 0x02, 0x02, 0x02, 0x0E},
	';': {0, 0x0C, 0x0C, 0, 0x0C, 0x04, 0x08}, '*': {0, 0x04, 0x15, 0x0E, 0x15, 0x04, 0},
}

func textWidth(scale int, s string) int { return len(s) * 6 * scale }

// drawText renders s in the 5x7 font, upper-casing letters and drawing
// unknown characters as '?'.
func drawText(img *image.RGBA, x, y, scale int, s string, c color.RGBA) {
	u := image.NewUniform(c)
	for _, ch := range strings.ToUpper(s) {
		g, ok := demoFont[ch]
		if !ok {
			g = demoFont['?']
		}
		for row, bitsRow := range g {
			for col := range 5 {
				if bitsRow&(0x10>>col) != 0 {
					px, py := x+col*scale, y+row*scale
					draw.Draw(img, image.Rect(px, py, px+scale, py+scale), u, image.Point{}, draw.Src)
				}
			}
		}
		x += 6 * scale
	}
}
