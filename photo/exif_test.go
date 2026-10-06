package photo_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/nrynss/keel/photo"
)

// uprightJPEG builds a 48 by 32 JPEG carrying one APP1 segment with the
// given raw TIFF block after the Exif signature.
func uprightJPEG(t *testing.T, tiff []byte) []byte {
	t.Helper()
	base := jpegEncode(t, distinctImage(48, 32), 90)
	return spliceSegment(base, 0xE1, exifAPP1(tiff))
}

// assertUpright normalises the input and asserts the pixels come out as
// stored, which is what an orientation the walk cannot trust reads as.
func assertUpright(t *testing.T, input []byte, msg string) {
	t.Helper()
	src := distinctImage(48, 32)
	res := normalizeOK(t, bytes.NewReader(input), photo.Config{Quality: 100})
	if res.Width != 48 || res.Height != 32 {
		t.Fatalf("%s: output is %d by %d, want the stored 48 by 32", msg, res.Width, res.Height)
	}
	assertPixelsClose(t, decodePixels(t, res.Bytes), src, 32)
}

func TestMalformedEXIFReadsAsUpright(t *testing.T) {
	order := binary.BigEndian
	tiffBadMagic := []byte("MM\x00\x2b")
	tiffBadOrder := []byte("XX\x00\x2a")
	tiffNoEntries := []byte("MM\x00\x2a\x00\x00\x00\x08")
	tiffOffsetPastEnd := []byte("MM\x00\x2a\x00\x01\x00\x00")
	tiffShortHeader := []byte("MM\x00\x2a")

	tiffTypeLong := func() []byte {
		var b []byte
		b = append(b, 'M', 'M')
		b = appendU16(b, order, 42)
		b = appendU32(b, order, 8)
		b = appendU16(b, order, 1)
		b = appendU16(b, order, 0x0112)
		b = appendU16(b, order, 4)
		b = appendU32(b, order, 1)
		b = appendU32(b, order, 6<<16)
		b = appendU32(b, order, 0)
		return b
	}
	tiffValueNine := func() []byte {
		var b []byte
		b = append(b, 'M', 'M')
		b = appendU16(b, order, 42)
		b = appendU32(b, order, 8)
		b = appendU16(b, order, 1)
		b = appendU16(b, order, 0x0112)
		b = appendU16(b, order, 3)
		b = appendU32(b, order, 1)
		b = appendU16(b, order, 9)
		b = appendU16(b, order, 0)
		b = appendU32(b, order, 0)
		return b
	}
	tiffCountTwo := func() []byte {
		var b []byte
		b = append(b, 'M', 'M')
		b = appendU16(b, order, 42)
		b = appendU32(b, order, 8)
		b = appendU16(b, order, 1)
		b = appendU16(b, order, 0x0112)
		b = appendU16(b, order, 3)
		b = appendU32(b, order, 2)
		b = appendU16(b, order, 6)
		b = appendU16(b, order, 0)
		b = appendU32(b, order, 0)
		return b
	}

	tests := []struct {
		name string
		tiff []byte
	}{
		{"bad magic", tiffBadMagic},
		{"bad byte order", tiffBadOrder},
		{"zero entries", tiffNoEntries},
		{"entry offset past the block", tiffOffsetPastEnd},
		{"truncated header", tiffShortHeader},
		{"type is not short", tiffTypeLong()},
		{"count is not one", tiffCountTwo()},
		{"value out of range", tiffValueNine()},
		{"empty block", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertUpright(t, uprightJPEG(t, tc.tiff), tc.name)
		})
	}

	t.Run("exif signature with no tiff block", func(t *testing.T) {
		base := jpegEncode(t, distinctImage(48, 32), 90)
		// A whole segment whose Exif payload stops after the
		// signature, so the walk finds nothing and reads no
		// orientation.
		bare := append(append([]byte{}, base[:2]...), 0xFF, 0xE1, 0x00, 0x08, 'E', 'x', 'i', 'f', 0x00, 0x00)
		bare = append(bare, base[2:]...)
		assertUpright(t, bare, "exif signature with no tiff block")
	})

	t.Run("segment length overruns the file", func(t *testing.T) {
		// A segment that claims more bytes than the file holds is not
		// a decodable image, so the refusal is the format one and not
		// a wrong orientation.
		base := jpegEncode(t, distinctImage(48, 32), 90)
		overrun := append(append([]byte{}, base[:2]...), 0xFF, 0xE1, 0xFF, 0xFF)
		overrun = append(overrun, base[2:]...)
		_, err := photo.Normalize(context.Background(), bytes.NewReader(overrun), photo.Config{})
		if err == nil {
			t.Fatal("overrunning segment was accepted")
		}
		if code := refusedCode(t, err); code != "unsupported_format" {
			t.Fatalf("code is %s, want unsupported_format", code)
		}
	})
}

func TestOrientationWalkSkipsOtherSegments(t *testing.T) {
	src := distinctImage(48, 32)
	base := jpegEncode(t, src, 90)

	t.Run("xmp before exif", func(t *testing.T) {
		xmp := append([]byte("http://ns.adobe.com/xap/1.0/\x00"), []byte("<x/>")...)
		input := spliceSegment(base, 0xE1, xmp)
		input = spliceSegment(input, 0xE1, exifAPP1(exifTIFF(binary.BigEndian, 6, "")))
		res := normalizeOK(t, bytes.NewReader(input), photo.Config{Quality: 100})
		if res.Width != 32 || res.Height != 48 {
			t.Fatalf("output is %d by %d, want the turned 32 by 48", res.Width, res.Height)
		}
		assertPixelsClose(t, decodePixels(t, res.Bytes), orientedWant(src, 6), 32)
	})

	t.Run("app0 before exif", func(t *testing.T) {
		jfif := append([]byte("JFIF\x00"), bytes.Repeat([]byte{0x01}, 9)...)
		input := spliceSegment(base, 0xE0, jfif)
		input = spliceSegment(input, 0xE1, exifAPP1(exifTIFF(binary.BigEndian, 8, "")))
		res := normalizeOK(t, bytes.NewReader(input), photo.Config{Quality: 100})
		if res.Width != 32 || res.Height != 48 {
			t.Fatalf("output is %d by %d, want the turned 32 by 48", res.Width, res.Height)
		}
		assertPixelsClose(t, decodePixels(t, res.Bytes), orientedWant(src, 8), 32)
	})
}
