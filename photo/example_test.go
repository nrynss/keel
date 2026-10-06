package photo_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"

	"github.com/nrynss/keel/photo"
)

// ExampleNormalize takes one uploaded photo and turns it into a JPEG that
// fits a downstream limit, upright and without any metadata.
func ExampleNormalize() {
	// Stand in for the upload: a photo four times the long side limit.
	src := image.NewNRGBA(image.Rect(0, 0, 800, 600))
	for y := 0; y < 600; y++ {
		for x := 0; x < 800; x++ {
			src.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 0x40, 0xFF})
		}
	}
	var raw bytes.Buffer
	if err := jpeg.Encode(&raw, src, &jpeg.Options{Quality: 85}); err != nil {
		fmt.Println("encode failed:", err)
		return
	}

	res, err := photo.Normalize(context.Background(), &raw, photo.Config{
		MaxLongSide: 200,
		MaxBytes:    8 << 20,
		Format:      photo.JPEG,
		Quality:     85,
	})
	if err != nil {
		fmt.Println("normalize failed:", err)
		return
	}
	fmt.Println(res.Width, res.Height, res.Format, len(res.SHA256))
	// Output:
	// 200 150 jpeg 64
}
