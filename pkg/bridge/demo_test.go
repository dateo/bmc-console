package bridge

import (
	"image"
	"strings"
	"testing"
)

func TestDemoReportsInput(t *testing.T) {
	d := NewDemo()
	before, rev0, _ := d.Frame()
	if before.Bounds() != image.Rect(0, 0, 640, 400) {
		t.Fatalf("bounds %v", before.Bounds())
	}
	snapshot := append([]byte(nil), before.Pix...)

	if err := d.Key('a', true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.keyLine, "HID 04") || !strings.Contains(d.keyLine, "DOWN") {
		t.Fatalf("key line %q", d.keyLine)
	}
	if err := d.Pointer(320, 300, 1); err != nil {
		t.Fatal(err)
	}
	after, rev1, _ := d.Frame()
	if rev1 <= rev0 {
		t.Fatal("revision did not advance")
	}
	if string(after.Pix) == string(snapshot) {
		t.Fatal("frame unchanged after input")
	}
	if c := after.RGBAAt(320, 300); c.R != 255 || c.G != 92 {
		t.Fatalf("no pressed-button dot under pointer: %v", c)
	}
}

func TestKeysymLabel(t *testing.T) {
	for sym, want := range map[uint32]string{'a': "'A'", ' ': "SPACE", 0xff0d: "ENTER", 0xffc9: "F12", 0x1234: "?"} {
		if got := keysymLabel(sym); got != want {
			t.Errorf("keysymLabel(%#x) = %q, want %q", sym, got, want)
		}
	}
}
