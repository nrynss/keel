package book

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func fontConfig(t *testing.T) Config {
	t.Helper()
	path := filepath.Join("..", "film", "testdata", "LiberationSans-Regular.ttf")
	return Config{FontDir: filepath.Dir(path), FontFile: filepath.Base(path)}
}

func tinyPNG(t *testing.T, w, h int) *bytes.Reader {
	t.Helper()
	return encodeImage(t, w, h, png.Encode)
}

func tinyJPEG(t *testing.T, w, h int) *bytes.Reader {
	t.Helper()
	return encodeImage(t, w, h, func(wri io.Writer, img image.Image) error {
		return jpeg.Encode(wri, img, nil)
	})
}

func encodeImage(t *testing.T, w, h int, enc func(io.Writer, image.Image) error) *bytes.Reader {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := enc(&buf, img); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

func pageCount(pdf []byte) int {
	return bytes.Count(pdf, []byte("/Type /Page")) - bytes.Count(pdf, []byte("/Type /Pages"))
}

func TestWriteImageAndCaption(t *testing.T) {
	cfg := fontConfig(t)
	var buf bytes.Buffer
	err := Write(t.Context(), &buf, cfg, []Page{
		{Image: tinyPNG(t, 8, 4), ImageType: "PNG", Caption: "The gate opens."},
		{Image: tinyJPEG(t, 4, 8), ImageType: "jpeg", Caption: "The end."},
	})
	if err != nil {
		t.Fatal(err)
	}
	pdf := buf.Bytes()
	if !bytes.HasPrefix(pdf, []byte("%PDF")) {
		t.Fatalf("pdf starts with %q, want a PDF header", pdf[:min(8, len(pdf))])
	}
	if got := pageCount(pdf); got != 2 {
		t.Fatalf("pages = %d, want 2", got)
	}
	if !bytes.Contains(pdf, []byte("/Subtype /Image")) && !bytes.Contains(pdf, []byte("/Subtype/Image")) {
		t.Fatal("pdf contains no image")
	}
}

func TestWriteImageOnlyNeedsNoFont(t *testing.T) {
	var buf bytes.Buffer
	err := Write(t.Context(), &buf, Config{}, []Page{{Image: tinyPNG(t, 2, 2), ImageType: "png"}})
	if err != nil {
		t.Fatal(err)
	}
	if pageCount(buf.Bytes()) != 1 {
		t.Fatalf("pages = %d, want 1", pageCount(buf.Bytes()))
	}
}

func TestWriteCaptionOnly(t *testing.T) {
	var buf bytes.Buffer
	err := Write(t.Context(), &buf, fontConfig(t), []Page{{Caption: "A booklet."}})
	if err != nil {
		t.Fatal(err)
	}
	if pageCount(buf.Bytes()) != 1 {
		t.Fatalf("pages = %d, want 1", pageCount(buf.Bytes()))
	}
}

func TestWritePageSizeAndOrientation(t *testing.T) {
	var buf bytes.Buffer
	cfg := fontConfig(t)
	cfg.PageSize = "Letter"
	cfg.Orientation = "l"
	err := Write(t.Context(), &buf, cfg, []Page{{Image: tinyPNG(t, 2, 2), ImageType: "png"}})
	if err != nil {
		t.Fatal(err)
	}
	// Letter is 612 by 792 points. Landscape swaps them, so the media box
	// names the longer side first.
	if !bytes.Contains(buf.Bytes(), []byte("792")) || !bytes.Contains(buf.Bytes(), []byte("612")) {
		t.Fatalf("pdf does not name a landscape letter media box:\n%s", buf.String())
	}
}

func TestWriteRejectsBadInput(t *testing.T) {
	cfg := fontConfig(t)
	cases := []struct {
		name  string
		cfg   Config
		pages []Page
	}{
		{name: "no pages", cfg: cfg},
		{name: "empty page", cfg: cfg, pages: []Page{{}}},
		{name: "caption without font", cfg: Config{}, pages: []Page{{Caption: "x"}}},
		{name: "font escapes dir", cfg: Config{FontDir: "fonts", FontFile: "../Face.ttf"}, pages: []Page{{Caption: "x"}}},
		{name: "bad orientation", cfg: Config{Orientation: "sideways"}, pages: []Page{{Image: tinyPNG(t, 1, 1), ImageType: "png"}}},
		{name: "bad image type", cfg: cfg, pages: []Page{{Image: tinyPNG(t, 1, 1), ImageType: "bmp"}}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := Write(t.Context(), &bytes.Buffer{}, tt.cfg, tt.pages)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want invalid input", err)
			}
		})
	}
}

func TestWriteBadImageIsAnError(t *testing.T) {
	err := Write(t.Context(), &bytes.Buffer{}, Config{}, []Page{{
		Image:     strings.NewReader("not a png"),
		ImageType: "png",
	}})
	if !errors.Is(err, ErrEncode) {
		t.Fatalf("err = %v, want encode failed", err)
	}
}

func TestWriteUnknownPageSize(t *testing.T) {
	err := Write(t.Context(), &bytes.Buffer{}, Config{PageSize: "Poster"}, []Page{{
		Image:     tinyPNG(t, 1, 1),
		ImageType: "png",
	}})
	if !errors.Is(err, ErrEncode) {
		t.Fatalf("err = %v, want encode failed", err)
	}
}

func TestWriteCancelledContextWritesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var buf bytes.Buffer
	err := Write(ctx, &buf, Config{}, []Page{{Image: tinyPNG(t, 1, 1), ImageType: "png"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("wrote %d bytes after cancellation", buf.Len())
	}
}

func TestWriteNilWriter(t *testing.T) {
	err := Write(t.Context(), nil, Config{}, []Page{{Image: tinyPNG(t, 1, 1), ImageType: "png"}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want invalid input", err)
	}
}
