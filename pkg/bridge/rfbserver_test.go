package bridge

import (
	"image"
	"image/color"
	"testing"
)

func TestDirtyBounds(t *testing.T) {
	a, b := image.NewRGBA(image.Rect(0, 0, 8, 5)), image.NewRGBA(image.Rect(0, 0, 8, 5))
	if !changedBounds(a, b).Empty() {
		t.Fatal("identical frames marked dirty")
	}
	b.SetRGBA(7, 0, color.RGBA{R: 1})
	b.SetRGBA(2, 3, color.RGBA{G: 2})
	if got := changedBounds(a, b); got != image.Rect(2, 0, 8, 4) {
		t.Fatalf("dirty rectangle %v", got)
	}
}
func TestBootKeys(t *testing.T) {
	for sym, want := range map[uint32]byte{0xffe3: 224, 0xffe9: 226, 0xffff: 76, 0xffc7: 67, 0xff0d: 40, 0xff52: 82, '#': 32, 'A': 4} {
		if got := HIDUsage(sym); got != want {
			t.Errorf("keysym %x -> usage %d, want %d", sym, got, want)
		}
	}
}
