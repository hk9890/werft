package virttest

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
)

// BootScreens is the console of a boot that asks for the disk password, as
// PNG: the firmware screen, black, then the prompt.
func BootScreens() [][]byte {
	return [][]byte{lit(0, 0), lit(-1, -1), lit(1, 1)}
}

// lit is a black 4x4 screen with one white pixel at x, y; outside it, none.
func lit(x, y int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 0xff
	}
	img.Set(x, y, color.White)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
