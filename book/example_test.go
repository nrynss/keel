package book_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"path/filepath"

	"github.com/nrynss/keel/book"
)

func ExampleWrite() {
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		fmt.Println("encode image:", err)
		return
	}
	font := filepath.Join("..", "film", "testdata", "LiberationSans-Regular.ttf")
	var pdf bytes.Buffer
	err := book.Write(context.Background(), &pdf, book.Config{
		FontDir:  filepath.Dir(font),
		FontFile: filepath.Base(font),
	}, []book.Page{{
		Image:     bytes.NewReader(pngBuf.Bytes()),
		ImageType: "png",
		Caption:   "The gate opens.",
	}})
	if err != nil {
		fmt.Println("write:", err)
		return
	}
	fmt.Println(bytes.HasPrefix(pdf.Bytes(), []byte("%PDF")))
	fmt.Println(pdf.Len() > 0)
	// Output:
	// true
	// true
}
