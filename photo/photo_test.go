package photo_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math/rand"
	"os"
	"testing"

	"github.com/nrynss/keel/photo"
)

// palette holds six colours a JPEG round trip keeps far apart, so a wrong
// pixel transform cannot pass a tolerance check.
var palette = []color.RGBA{
	{0xFF, 0x00, 0x00, 0xFF},
	{0x00, 0xFF, 0x00, 0xFF},
	{0x00, 0x00, 0xFF, 0xFF},
	{0xFF, 0xFF, 0x00, 0xFF},
	{0xFF, 0x00, 0xFF, 0xFF},
	{0x00, 0xFF, 0xFF, 0xFF},
}

// blockSize is the side of one solid colour block. A JPEG output runs
// through 4:2:0 chroma subsampling, so pixel level colour changes bleed
// across neighbours. Solid blocks survive the round trip and keep every
// wrong transform visible.
const blockSize = 16

// distinctImage returns a w by h image painted in solid colour blocks that
// cycle through the palette by position.
func distinctImage(w, h int) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	across := (w + blockSize - 1) / blockSize
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, palette[(y/blockSize*across+x/blockSize)%len(palette)])
		}
	}
	return img
}

// jpegEncode encodes img at the quality and fails the test on error.
func jpegEncode(t *testing.T, img image.Image, quality int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// pngEncode encodes img and fails the test on error.
func pngEncode(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// appendU16 and appendU32 build the byte-level fixtures the tests splice.
func appendU16(b []byte, order binary.ByteOrder, v uint16) []byte {
	var t [2]byte
	order.PutUint16(t[:], v)
	return append(b, t[:]...)
}

func appendU32(b []byte, order binary.ByteOrder, v uint32) []byte {
	var t [4]byte
	order.PutUint32(t[:], v)
	return append(b, t[:]...)
}

// exifTIFF builds a TIFF block with one orientation tag, and one ASCII
// description tag when description is not empty. The description stands in
// for the private metadata an upload carries.
func exifTIFF(order binary.ByteOrder, orientation int, description string) []byte {
	entries := 1
	if description != "" {
		entries = 2
	}
	var b []byte
	if order == binary.BigEndian {
		b = append(b, 'M', 'M')
	} else {
		b = append(b, 'I', 'I')
	}
	b = appendU16(b, order, 42)
	b = appendU32(b, order, 8)
	b = appendU16(b, order, uint16(entries))
	// Orientation, type SHORT, count 1, value inline.
	b = appendU16(b, order, 0x0112)
	b = appendU16(b, order, 3)
	b = appendU32(b, order, 1)
	b = appendU16(b, order, uint16(orientation))
	b = appendU16(b, order, 0)
	if description != "" {
		b = appendU16(b, order, 0x010E)
		b = appendU16(b, order, 2)
		b = appendU32(b, order, uint32(len(description)+1))
		b = appendU32(b, order, uint32(8+2+12*entries+4))
	}
	b = appendU32(b, order, 0)
	b = append(b, description...)
	return append(b, 0)
}

// exifAPP1 wraps a TIFF block in the APP1 payload shape.
func exifAPP1(tiff []byte) []byte {
	return append([]byte("Exif\x00\x00"), tiff...)
}

// spliceSegment inserts one marker segment right after the JPEG SOI.
func spliceSegment(jpegBytes []byte, marker byte, payload []byte) []byte {
	segLen := len(payload) + 2
	out := []byte{0xFF, 0xD8, 0xFF, marker, byte(segLen >> 8), byte(segLen)}
	out = append(out, payload...)
	return append(out, jpegBytes[2:]...)
}

// pngChunk builds one PNG chunk of the kind with the data.
func pngChunk(kind string, data []byte) []byte {
	out := appendU32(nil, binary.BigEndian, uint32(len(data)))
	out = append(out, kind...)
	out = append(out, data...)
	sum := crc32.ChecksumIEEE(out[4:])
	return appendU32(out, binary.BigEndian, sum)
}

// withPNGeXIf inserts an eXIf chunk after the IHDR chunk of a PNG.
func withPNGeXIf(pngBytes []byte, data []byte) []byte {
	// Signature is 8 bytes, and IHDR is 4 length, 4 type, 13 data and
	// 4 CRC.
	off := 8 + 4 + 4 + 13 + 4
	chunk := pngChunk("eXIf", data)
	out := append([]byte{}, pngBytes[:off]...)
	out = append(out, chunk...)
	return append(out, pngBytes[off:]...)
}

// heicFixture builds ISO BMFF bytes with the ftyp brands and padding.
func heicFixture(major string, compat []string, pad int) []byte {
	body := []byte(major)
	body = append(body, 0, 0, 0, 0)
	for _, c := range compat {
		body = append(body, c...)
	}
	out := appendU32(nil, binary.BigEndian, uint32(8+len(body)))
	out = append(out, "ftyp"...)
	out = append(out, body...)
	return append(out, bytes.Repeat([]byte{0xAA}, pad)...)
}

// jpegHeaderBomb crafts a JPEG whose SOF claims w by h pixels and whose
// scan is garbage. A full decode cannot succeed, so a too_many_pixels
// refusal proves the header stage decided.
func jpegHeaderBomb(w, h int) []byte {
	b := []byte{0xFF, 0xD8}
	b = append(b, 0xFF, 0xC0, 0x00, 0x0B, 0x08, byte(h>>8), byte(h), byte(w>>8), byte(w), 0x01, 0x01, 0x11, 0x00)
	b = append(b, 0xFF, 0xDA, 0x00, 0x08, 0x01, 0x01, 0x00, 0x00, 0x3F, 0x00)
	return append(b, bytes.Repeat([]byte{0x00}, 32)...)
}

// pngHeaderBomb crafts a PNG whose IHDR claims w by h pixels and which
// carries no image data at all.
func pngHeaderBomb(w, h int) []byte {
	sig := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	ihdr := appendU32(nil, binary.BigEndian, uint32(w))
	ihdr = appendU32(ihdr, binary.BigEndian, uint32(h))
	ihdr = append(ihdr, 0x08, 0x06, 0x00, 0x00, 0x00)
	out := append(sig, pngChunk("IHDR", ihdr)...)
	return append(out, pngChunk("IEND", nil)...)
}

// webpHeaderBomb crafts a WebP whose VP8X canvas claims w by h pixels.
// RIFF sizes and the uint24 canvas sides are little endian.
func webpHeaderBomb(w, h int) []byte {
	vp8x := []byte{0x00, 0x00, 0x00, 0x00}
	vp8x = append(vp8x, byte(w-1), byte((w-1)>>8), byte((w-1)>>16))
	vp8x = append(vp8x, byte(h-1), byte((h-1)>>8), byte((h-1)>>16))
	out := binary.LittleEndian.AppendUint32([]byte("RIFF"), uint32(4+8+len(vp8x)))
	out = append(out, "WEBP"...)
	out = append(out, "VP8X"...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(vp8x)))
	return append(out, vp8x...)
}

// noiseImage returns a w by h image of deterministic high entropy pixels,
// so a JPEG re-encode is large and the byte-cap ladder has work to do.
func noiseImage(w, h int) image.Image {
	rng := rand.New(rand.NewSource(7))
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i] = byte(rng.Intn(256))
		img.Pix[i+1] = byte(rng.Intn(256))
		img.Pix[i+2] = byte(rng.Intn(256))
		img.Pix[i+3] = 0xFF
	}
	return img
}

// normalizeOK runs Normalize and fails the test on any error.
func normalizeOK(t *testing.T, r io.Reader, cfg photo.Config) photo.Result {
	t.Helper()
	res, err := photo.Normalize(context.Background(), r, cfg)
	if err != nil {
		t.Fatalf("Normalize refused: %v", err)
	}
	return res
}

// refusedCode asserts the error is a photo refusal and returns its code.
func refusedCode(t *testing.T, err error) string {
	t.Helper()
	var perr *photo.Error
	if !errors.As(err, &perr) {
		t.Fatalf("error is not a photo refusal: %v", err)
	}
	return perr.Code()
}

// decodePixels decodes encoded image bytes and fails the test on error.
func decodePixels(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	return img
}

// assertPixelsClose fails the test when got and want differ by more than
// tol on any channel, or when their sizes differ.
func assertPixelsClose(t *testing.T, got, want image.Image, tol int) {
	t.Helper()
	gb, wb := got.Bounds(), want.Bounds()
	if gb.Dx() != wb.Dx() || gb.Dy() != wb.Dy() {
		t.Fatalf("output is %d by %d, want %d by %d", gb.Dx(), gb.Dy(), wb.Dx(), wb.Dy())
	}
	for y := 0; y < wb.Dy(); y++ {
		for x := 0; x < wb.Dx(); x++ {
			gr, gg, gbb, _ := got.At(x, y).RGBA()
			wr, wg, wbb, _ := want.At(x, y).RGBA()
			for ch := 0; ch < 3; ch++ {
				g := []int{int(gr >> 8), int(gg >> 8), int(gbb >> 8)}[ch]
				w := []int{int(wr >> 8), int(wg >> 8), int(wbb >> 8)}[ch]
				if d := g - w; d < -tol || d > tol {
					t.Fatalf("pixel (%d, %d) channel %d is %d, want about %d", x, y, ch, g, w)
				}
			}
		}
	}
}

// orientedWant builds the upright image the orientation demands, straight
// from the tag definitions. Row 0 and column 0 name where the stored image
// sits in the visual scene.
func orientedWant(src image.Image, o int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	want := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sx, sy int
			switch o {
			case 1:
				sx, sy = x, y
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			}
			want.Set(x, y, src.At(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return want
}

func TestNormalizeAppliesOrientation(t *testing.T) {
	src := distinctImage(48, 32)
	base := jpegEncode(t, src, 100)
	for o := 1; o <= 8; o++ {
		t.Run(tagName(o), func(t *testing.T) {
			input := spliceSegment(base, 0xE1, exifAPP1(exifTIFF(binary.BigEndian, o, "")))
			res := normalizeOK(t, bytes.NewReader(input), photo.Config{
				MaxLongSide: 1000,
				Quality:     100,
			})
			dw, dh := 48, 32
			if o >= 5 {
				dw, dh = 32, 48
			}
			if res.Width != dw || res.Height != dh {
				t.Fatalf("output is %d by %d, want %d by %d", res.Width, res.Height, dw, dh)
			}
			assertPixelsClose(t, decodePixels(t, res.Bytes), orientedWant(src, o), 32)
		})
	}
}

func tagName(o int) string {
	return []string{"", "one", "two", "three", "four", "five", "six", "seven", "eight"}[o]
}

func TestNormalizeStripsJPEGMetadata(t *testing.T) {
	src := distinctImage(32, 16)
	base := jpegEncode(t, src, 90)
	marker := "PRIVATE GPS 51.5000 -0.1200"
	input := spliceSegment(base, 0xE1, exifAPP1(exifTIFF(binary.BigEndian, 1, marker)))
	icc := append([]byte("ICC_PROFILE\x00"), bytes.Repeat([]byte{0x42}, 40)...)
	input = spliceSegment(input, 0xE2, icc)
	res := normalizeOK(t, bytes.NewReader(input), photo.Config{Quality: 90})
	for _, banned := range []string{"Exif\x00\x00", "ICC_PROFILE", marker} {
		if bytes.Contains(res.Bytes, []byte(banned)) {
			t.Fatalf("output still carries %q", banned)
		}
	}
	// The encoder itself may write APP0 only. Walk the segments after
	// the SOI and fail on any APP1 or APP2.
	seg := res.Bytes[2:]
	for len(seg) >= 4 && seg[0] == 0xFF && seg[1] != 0xDA {
		m := seg[1]
		if m == 0xE1 || m == 0xE2 {
			t.Fatalf("output carries an APP segment at marker 0x%02X", m)
		}
		if m == 0x01 || (m >= 0xD0 && m <= 0xD9) {
			break
		}
		l := int(seg[2])<<8 | int(seg[3])
		if l < 2 || 2+l > len(seg) {
			break
		}
		seg = seg[2+l:]
	}
	assertPixelsClose(t, decodePixels(t, res.Bytes), src, 32)
}

func TestNormalizeStripsPNGMetadata(t *testing.T) {
	src := distinctImage(8, 8)
	input := withPNGeXIf(pngEncode(t, src), bytes.Repeat([]byte("PRIVATE"), 6))
	res := normalizeOK(t, bytes.NewReader(input), photo.Config{Format: photo.PNG})
	if bytes.Contains(res.Bytes, []byte("eXIf")) {
		t.Fatal("output still carries an eXIf chunk")
	}
	if bytes.Contains(res.Bytes, []byte("PRIVATE")) {
		t.Fatal("output still carries the private marker text")
	}
	assertPixelsClose(t, decodePixels(t, res.Bytes), src, 0)
}

func TestNormalizeRefusesHeaderOverPixelCap(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
	}{
		{"jpeg", jpegHeaderBomb(40000, 40000)},
		{"png", pngHeaderBomb(20000, 20000)},
		{"webp", webpHeaderBomb(40000, 40000)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := photo.Normalize(context.Background(), bytes.NewReader(tc.input), photo.Config{})
			if err == nil {
				t.Fatal("bomb was accepted")
			}
			if code := refusedCode(t, err); code != "too_many_pixels" {
				t.Fatalf("code is %s, want too_many_pixels", code)
			}
			if !errors.Is(err, photo.ErrTooManyPixels) {
				t.Fatalf("error does not match ErrTooManyPixels: %v", err)
			}
			if res.Bytes != nil {
				t.Fatal("refusal returned bytes")
			}
		})
	}
}

func TestNormalizeRefusesOversizeInput(t *testing.T) {
	input := bytes.Repeat([]byte{0xFF, 0xD8, 0xFF, 0xE0}, 2500)
	_, err := photo.Normalize(context.Background(), bytes.NewReader(input), photo.Config{
		MaxInputBytes: 1024,
	})
	if err == nil {
		t.Fatal("oversize input was accepted")
	}
	if code := refusedCode(t, err); code != "too_many_input_bytes" {
		t.Fatalf("code is %s, want too_many_input_bytes", code)
	}
	if !errors.Is(err, photo.ErrTooManyInputBytes) {
		t.Fatalf("error does not match ErrTooManyInputBytes: %v", err)
	}
}

func TestHEICRefusals(t *testing.T) {
	tests := []struct {
		name      string
		input     []byte
		maxInput  int
		wantCode  string
		wantIsHEI bool
	}{
		{"major heic", heicFixture("heic", nil, 0), 1 << 20, "heic", true},
		{"mif1 compatible", heicFixture("isom", []string{"isom", "mif1", "mp42"}, 0), 1 << 20, "heic", true},
		{"avif carries mif1", heicFixture("avif", []string{"avif", "mif1", "MA1B"}, 0), 1 << 20, "heic", true},
		{"heic past the input cap", heicFixture("heic", nil, 8192), 2048, "heic", true},
		{"bmff without the family", heicFixture("isom", []string{"isom", "mp42"}, 0), 1 << 20, "unsupported_format", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := photo.Normalize(context.Background(), bytes.NewReader(tc.input), photo.Config{
				MaxInputBytes: tc.maxInput,
			})
			if err == nil {
				t.Fatal("HEIC input was accepted")
			}
			if code := refusedCode(t, err); code != tc.wantCode {
				t.Fatalf("code is %s, want %s", code, tc.wantCode)
			}
			if tc.wantIsHEI && !errors.Is(err, photo.ErrHEIC) {
				t.Fatalf("error does not match ErrHEIC: %v", err)
			}
			if !tc.wantIsHEI && !errors.Is(err, photo.ErrUnsupportedFormat) {
				t.Fatalf("error does not match ErrUnsupportedFormat: %v", err)
			}
		})
	}
}

func TestJPEGWithFtypPayloadIsNotJudgedAsHEIC(t *testing.T) {
	// A well-formed JPEG whose APP1 payload spells ftyp is a JPEG, so
	// the sniff must leave it to the JPEG decoder instead of refusing
	// the family.
	base := jpegEncode(t, distinctImage(16, 16), 90)
	payload := append([]byte("ftyp heic"), bytes.Repeat([]byte{0x00}, 8)...)
	input := spliceSegment(base, 0xE1, payload)
	res, err := photo.Normalize(context.Background(), bytes.NewReader(input), photo.Config{})
	if err != nil {
		t.Fatalf("jpeg with an ftyp payload was refused: %v", err)
	}
	if res.Bytes == nil {
		t.Fatal("accepted input returned no bytes")
	}
}

func TestByteCapStepsQualityThenScale(t *testing.T) {
	src := noiseImage(256, 256)
	uncapped := normalizeOK(t, bytes.NewReader(pngEncode(t, src)), photo.Config{
		Quality: 100,
	})
	// A cap one byte under the full size output forces the quality
	// ladder to step without scaling.
	qCap := len(uncapped.Bytes) - 1
	res := normalizeOK(t, bytes.NewReader(pngEncode(t, src)), photo.Config{
		Quality:  100,
		MaxBytes: qCap,
	})
	if res.Width != 256 || res.Height != 256 {
		t.Fatalf("quality step changed the size to %d by %d", res.Width, res.Height)
	}
	if len(res.Bytes) > qCap {
		t.Fatalf("output is %d bytes, over the cap of %d", len(res.Bytes), qCap)
	}

	// A cap one byte under the floor quality output forces the scale
	// ladder.
	floored := normalizeOK(t, bytes.NewReader(pngEncode(t, src)), photo.Config{
		Quality: 40,
	})
	sCap := len(floored.Bytes) - 1
	res = normalizeOK(t, bytes.NewReader(pngEncode(t, src)), photo.Config{
		Quality:  100,
		MaxBytes: sCap,
	})
	if res.Width >= 256 {
		t.Fatalf("scale ladder did not shrink, width is %d", res.Width)
	}
	if len(res.Bytes) > sCap {
		t.Fatalf("output is %d bytes, over the cap of %d", len(res.Bytes), sCap)
	}

	// A cap nothing can meet refuses once both floors are exhausted.
	_, err := photo.Normalize(context.Background(), bytes.NewReader(pngEncode(t, src)), photo.Config{
		Quality:  100,
		MaxBytes: 60,
	})
	if err == nil {
		t.Fatal("absurd cap was accepted")
	}
	if code := refusedCode(t, err); code != "output_cannot_fit" {
		t.Fatalf("code is %s, want output_cannot_fit", code)
	}
	if !errors.Is(err, photo.ErrOutputCannotFit) {
		t.Fatalf("error does not match ErrOutputCannotFit: %v", err)
	}

	// The ladder is deterministic.
	same := normalizeOK(t, bytes.NewReader(pngEncode(t, src)), photo.Config{
		Quality:  100,
		MaxBytes: sCap,
	})
	if same.SHA256 != res.SHA256 {
		t.Fatal("same input under the same cap hashed differently")
	}
}

func TestHashStableAcrossMetadata(t *testing.T) {
	src := distinctImage(32, 16)
	base := jpegEncode(t, src, 90)
	plain := base
	withTag := spliceSegment(base, 0xE1, exifAPP1(exifTIFF(binary.BigEndian, 1, "PRIVATE GPS 51.5 -0.12")))
	littleTag := spliceSegment(base, 0xE1, exifAPP1(exifTIFF(binary.LittleEndian, 1, "another private note")))
	cfg := photo.Config{Quality: 90}
	h1 := normalizeOK(t, bytes.NewReader(plain), cfg).SHA256
	h2 := normalizeOK(t, bytes.NewReader(withTag), cfg).SHA256
	h3 := normalizeOK(t, bytes.NewReader(littleTag), cfg).SHA256
	if h1 != h2 || h2 != h3 {
		t.Fatalf("metadata changed the digest: %s %s %s", h1, h2, h3)
	}
	if len(h1) != 64 {
		t.Fatalf("digest is %d hex characters, want 64", len(h1))
	}
	rotated := normalizeOK(t, bytes.NewReader(spliceSegment(base, 0xE1, exifAPP1(exifTIFF(binary.BigEndian, 6, "")))), cfg)
	if rotated.SHA256 == h1 {
		t.Fatal("a different orientation hashed the same")
	}
	if rotated.Width != 16 || rotated.Height != 32 {
		t.Fatalf("rotated output is %d by %d, want 16 by 32", rotated.Width, rotated.Height)
	}
}

func TestWebPFixtureDecodes(t *testing.T) {
	raw, err := os.ReadFile("testdata/upright.webp")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	res := normalizeOK(t, bytes.NewReader(raw), photo.Config{})
	if res.Width != 48 || res.Height != 32 {
		t.Fatalf("fixture decoded to %d by %d, want 48 by 32", res.Width, res.Height)
	}
	out := decodePixels(t, res.Bytes)
	// The fixture is a blue field with a white box at 30,8 through
	// 40,24, so two samples pin the pixels came through.
	box := color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
	field := color.RGBA{0x22, 0x66, 0xAA, 0xFF}
	sampleColor(t, out, 35, 15, box, 32)
	sampleColor(t, out, 5, 5, field, 32)
}

// sampleColor fails the test when the pixel is not within tol of want.
func sampleColor(t *testing.T, img image.Image, x, y int, want color.RGBA, tol int) {
	t.Helper()
	r, g, b, _ := img.At(x, y).RGBA()
	got := []int{int(r >> 8), int(g >> 8), int(b >> 8)}
	for ch, w := range []int{int(want.R), int(want.G), int(want.B)} {
		if d := got[ch] - w; d < -tol || d > tol {
			t.Fatalf("pixel (%d, %d) channel %d is %d, want about %d", x, y, ch, got[ch], w)
		}
	}
}

func TestPNGOutputMode(t *testing.T) {
	src := distinctImage(32, 16)
	res := normalizeOK(t, bytes.NewReader(jpegEncode(t, src, 90)), photo.Config{Format: photo.PNG})
	if res.Format != photo.PNG {
		t.Fatalf("format is %v, want png", res.Format)
	}
	sig := []byte{0x89, 0x50, 0x4E, 0x47}
	if !bytes.HasPrefix(res.Bytes, sig) {
		t.Fatal("output does not start with the PNG signature")
	}
	if res.Width != 32 || res.Height != 16 {
		t.Fatalf("output is %d by %d, want 32 by 16", res.Width, res.Height)
	}
	assertPixelsClose(t, decodePixels(t, res.Bytes), src, 32)
}

func TestNormalizeNeverEnlarges(t *testing.T) {
	src := distinctImage(100, 50)
	res := normalizeOK(t, bytes.NewReader(jpegEncode(t, src, 90)), photo.Config{MaxLongSide: 4000})
	if res.Width != 100 || res.Height != 50 {
		t.Fatalf("output is %d by %d, want the input size", res.Width, res.Height)
	}
}

func TestNormalizeHonoursShortSide(t *testing.T) {
	src := distinctImage(1000, 500)
	res := normalizeOK(t, bytes.NewReader(jpegEncode(t, src, 90)), photo.Config{
		MaxLongSide:  5000,
		MaxShortSide: 250,
	})
	if res.Width != 500 || res.Height != 250 {
		t.Fatalf("output is %d by %d, want 500 by 250", res.Width, res.Height)
	}
}

func TestUnsupportedFormats(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
	}{
		{"empty", nil},
		{"text", []byte("hello, this is not an image")},
		{"truncated jpeg", []byte{0xFF, 0xD8}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := photo.Normalize(context.Background(), bytes.NewReader(tc.input), photo.Config{})
			if err == nil {
				t.Fatal("input was accepted")
			}
			if code := refusedCode(t, err); code != "unsupported_format" {
				t.Fatalf("code is %s, want unsupported_format", code)
			}
			if !errors.Is(err, photo.ErrUnsupportedFormat) {
				t.Fatalf("error does not match ErrUnsupportedFormat: %v", err)
			}
			if res.Bytes != nil {
				t.Fatal("refusal returned bytes")
			}
		})
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  photo.Config
	}{
		{"negative long side", photo.Config{MaxLongSide: -1}},
		{"negative short side", photo.Config{MaxShortSide: -1}},
		{"short above long", photo.Config{MaxLongSide: 100, MaxShortSide: 200}},
		{"negative byte cap", photo.Config{MaxBytes: -1}},
		{"unknown format", photo.Config{Format: photo.Format(9)}},
		{"quality over 100", photo.Config{Quality: 101}},
		{"negative quality", photo.Config{Quality: -1}},
		{"negative pixels", photo.Config{MaxPixels: -1}},
		{"negative input bytes", photo.Config{MaxInputBytes: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := photo.Normalize(context.Background(), bytes.NewReader(nil), tc.cfg)
			if !errors.Is(err, photo.ErrInvalidConfig) {
				t.Fatalf("error does not match ErrInvalidConfig: %v", err)
			}
		})
	}
}

func TestZeroConfigIsUsable(t *testing.T) {
	src := distinctImage(100, 50)
	res := normalizeOK(t, bytes.NewReader(jpegEncode(t, src, 90)), photo.Config{})
	if res.Width != 100 || res.Height != 50 {
		t.Fatalf("output is %d by %d, want 100 by 50", res.Width, res.Height)
	}
	if res.Format != photo.JPEG {
		t.Fatalf("format is %v, want jpeg", res.Format)
	}
}

func TestCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input := jpegEncode(t, distinctImage(8, 8), 90)
	res, err := photo.Normalize(ctx, bytes.NewReader(input), photo.Config{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error does not match context.Canceled: %v", err)
	}
	if res.Bytes != nil {
		t.Fatal("cancelled call returned bytes")
	}
}

func TestNilReader(t *testing.T) {
	if _, err := photo.Normalize(context.Background(), nil, photo.Config{}); err == nil {
		t.Fatal("nil reader was accepted")
	}
}

func TestGIFRefusedWhenTheRegistryKnowsGIF(t *testing.T) {
	// Encoding the GIF links image/gif into this binary, so the process
	// wide registry knows the format. The package's own magic byte
	// check must still refuse it, because the docs promise JPEG, PNG
	// and WebP and nothing else.
	var buf bytes.Buffer
	if err := gif.Encode(&buf, distinctImage(4, 4), nil); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	res, err := photo.Normalize(context.Background(), &buf, photo.Config{})
	if err == nil {
		t.Fatal("gif was accepted although the package does not read it")
	}
	if code := refusedCode(t, err); code != "unsupported_format" {
		t.Fatalf("code is %s, want unsupported_format", code)
	}
	if !errors.Is(err, photo.ErrUnsupportedFormat) {
		t.Fatalf("error does not match ErrUnsupportedFormat: %v", err)
	}
	if res.Bytes != nil {
		t.Fatal("refusal returned bytes")
	}
}
