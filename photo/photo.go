// Package photo normalises an uploaded photo before its bytes reach a paid
// API or a store.
//
// Phone photos arrive over every limit at once. They carry far more pixels
// than a model accepts, they are rotated through EXIF orientation instead of
// in the pixels, and they hold GPS coordinates and device metadata that must
// never be stored or forwarded. Normalize decodes a JPEG, PNG or WebP upload,
// applies the EXIF orientation to the pixels, resizes under the configured
// limits, and re-encodes from the decoded pixels. The re-encode is what
// strips metadata, so the output carries no EXIF, XMP or ICC data beyond
// what the encoder itself writes.
//
// The result carries a SHA-256 of the normalised bytes, so an application
// can key a cache on the pixels alone. The same pixels uploaded twice, with
// different metadata, hash to the same key. A different orientation changes
// the pixels, and the hash changes with them.
//
// Every refusal is deterministic. The same bytes under the same Config give
// the same answer. Each refusal is an Error that wraps one sentinel for
// errors.Is and carries a stable snake_case code for a response envelope.
//
// # Order of the guards
//
// Normalize refuses in a fixed order, cheapest check first:
//
//   - The HEIC family sniff reads the first few kilobytes and refuses an
//     ISO BMFF image before the rest of the input is read, so a large
//     refusal never pays for its size.
//   - The input byte cap bounds the whole read.
//   - The pixel cap reads the header alone and refuses before the full
//     decode, which bounds what a decompression bomb can allocate.
//   - The byte-cap ladder refuses only after the quality floor and the
//     scale floor are both exhausted.
//
// Orientation is read only from a JPEG APP1 Exif segment, through a minimal
// TIFF walk. A PNG or WebP input is trusted to be upright. An orientation
// the walk cannot trust reads as 1.
package photo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"

	"golang.org/x/image/draw"
	"golang.org/x/image/math/f64"
	_ "golang.org/x/image/webp" // registers the WebP decoder with image
)

// Defaults for the Config fields whose zero value must stay usable.
const (
	// DefaultMaxLongSide is the long side bound Normalize applies when
	// Config leaves it at zero.
	DefaultMaxLongSide = 4096

	// DefaultQuality is the JPEG quality Normalize applies when Config
	// leaves it at zero.
	DefaultQuality = 90

	// DefaultMaxPixels is the pixel count bound Normalize applies when
	// Config leaves it at zero.
	DefaultMaxPixels = 64_000_000

	// DefaultMaxInputBytes is the input byte bound Normalize applies
	// when Config leaves it at zero.
	DefaultMaxInputBytes = 64 << 20
)

// Bounds of the byte-cap ladder. When an output stays over MaxBytes, the
// ladder first steps the JPEG quality down to the floor, then scales the
// sides down by seven eighths at a time, until the long side reaches the
// floor side. One step past both floors is a refusal.
const (
	qualityFloor   = 40
	qualityStep    = 10
	scaleStepNum   = 7
	scaleStepDen   = 8
	scaleFloorSide = 64
)

// sniffBytes is how much of the input is read before the rest. The ISO BMFF
// box that names a HEIC file sits at the very start, so this much is enough
// to refuse the family before reading further.
const sniffBytes = 4096

// Config bounds what Normalize accepts and how it writes the output. The
// zero value is usable and keeps the defaults named in this package. A
// Config that cannot be honoured refuses with an error wrapping
// ErrInvalidConfig before any input is read.
type Config struct {
	// MaxLongSide bounds the longer side of the output. Zero means
	// DefaultMaxLongSide. A negative value is refused.
	MaxLongSide int

	// MaxShortSide bounds the shorter side of the output. Zero disables
	// the bound. A negative value, or one above the long side bound, is
	// refused.
	MaxShortSide int

	// MaxBytes caps the size of the re-encoded output. Zero disables the
	// cap, and the output is encoded once at the configured quality. A
	// negative value is refused.
	MaxBytes int

	// Format names the output encoding. The zero value is JPEG.
	// FormatPNG ignores Quality.
	Format Format

	// Quality is the JPEG quality, from 1 to 100. Zero means
	// DefaultQuality. A value outside 1 to 100 is refused.
	Quality int

	// MaxPixels caps the pixel count the decoder may reach. The count is
	// read from the header, and the refusal happens before the full
	// decode. Zero means DefaultMaxPixels. A negative value is refused.
	MaxPixels int64

	// MaxInputBytes caps how many input bytes are read. Zero means
	// DefaultMaxInputBytes. A negative value is refused.
	MaxInputBytes int
}

// Format names an output encoding.
type Format int

const (
	// JPEG writes a baseline JPEG. It is the zero value of Format.
	JPEG Format = iota

	// PNG writes a PNG. PNG has no quality, so Config.Quality does
	// nothing in this mode.
	PNG
)

// String returns the lowercase name of the format. A Format out of range
// reads as unknown.
func (f Format) String() string {
	switch f {
	case JPEG:
		return "jpeg"
	case PNG:
		return "png"
	}
	return "unknown"
}

// Result is the normalised output of one Normalize call.
type Result struct {
	// Bytes are the re-encoded image. No input metadata travels with
	// them beyond what the encoder itself writes.
	Bytes []byte

	// Width and Height are the pixel dimensions of Bytes, after the
	// orientation and any resize.
	Width int

	// Height is the pixel height of Bytes.
	Height int

	// Format names the encoding of Bytes.
	Format Format

	// SHA256 is the lowercase hex SHA-256 of Bytes. Inputs whose pixels
	// and orientation agree hash the same, whatever metadata they carry.
	SHA256 string
}

// Refusal sentinels. Every refusal Normalize returns wraps exactly one of
// these, so errors.Is tells them apart.
var (
	// ErrUnsupportedFormat reports bytes that are not a JPEG, PNG or
	// WebP image, or an image whose body is too corrupt to decode.
	ErrUnsupportedFormat = errors.New("photo: unsupported image format")

	// ErrHEIC reports an image in the ISO BMFF still image family. This
	// covers the HEIC brands and, with them, AVIF files that carry the
	// shared mif1 brand. The family is refused, not decoded.
	ErrHEIC = errors.New("photo: HEIC image")

	// ErrTooManyPixels reports an image whose header dimensions are
	// over MaxPixels. The refusal happens before the full decode.
	ErrTooManyPixels = errors.New("photo: image is over the pixel limit")

	// ErrTooManyInputBytes reports an input over MaxInputBytes. The
	// refusal happens before any decode.
	ErrTooManyInputBytes = errors.New("photo: input is over the input byte limit")

	// ErrOutputCannotFit reports an image that stays over MaxBytes after
	// the quality floor and the scale floor are both exhausted.
	ErrOutputCannotFit = errors.New("photo: output cannot fit under the byte cap")

	// ErrInvalidConfig reports a Config Normalize cannot honour.
	ErrInvalidConfig = errors.New("photo: invalid config")
)

// Stable snake_case codes a refusal carries. An application maps the code
// into its own response envelope and never branches on a message. The codes
// do not change between releases.
const (
	codeUnsupportedFormat = "unsupported_format"
	codeHEIC              = "heic"
	codeTooManyPixels     = "too_many_pixels"
	codeTooManyInputBytes = "too_many_input_bytes"
	codeOutputCannotFit   = "output_cannot_fit"
)

// Error is a refusal to normalise. Unwrap reaches the sentinel behind it,
// and Code names the refusal for a response envelope.
type Error struct {
	code string
	err  error
}

// Error returns the refusal as one lowercase line.
func (e *Error) Error() string {
	return e.err.Error()
}

// Code returns the stable snake_case code of the refusal. The codes are
// unsupported_format, heic, too_many_pixels, too_many_input_bytes and
// output_cannot_fit.
func (e *Error) Code() string {
	return e.code
}

// Unwrap returns the wrapped error, which carries the sentinel behind the
// refusal.
func (e *Error) Unwrap() error {
	return e.err
}

// refusal builds one Error of the given code. The wrapped error must carry
// one refusal sentinel.
func refusal(code string, err error) *Error {
	return &Error{code: code, err: err}
}

// limits is a Config with every zero resolved to its default. It is what
// the rest of the package reads.
type limits struct {
	maxLong       int
	maxShort      int
	maxBytes      int
	format        Format
	quality       int
	maxPixels     int64
	maxInputBytes int
}

// resolve validates a Config and fills its zero values with the defaults.
func (c Config) resolve() (limits, error) {
	l := limits{
		maxLong:       DefaultMaxLongSide,
		format:        JPEG,
		quality:       DefaultQuality,
		maxPixels:     DefaultMaxPixels,
		maxInputBytes: DefaultMaxInputBytes,
	}
	switch {
	case c.MaxLongSide < 0:
		return l, fmt.Errorf("%w: MaxLongSide is negative", ErrInvalidConfig)
	case c.MaxLongSide > 0:
		l.maxLong = c.MaxLongSide
	}
	switch {
	case c.MaxShortSide < 0:
		return l, fmt.Errorf("%w: MaxShortSide is negative", ErrInvalidConfig)
	case c.MaxShortSide > l.maxLong:
		return l, fmt.Errorf("%w: MaxShortSide %d is above the long side bound %d", ErrInvalidConfig, c.MaxShortSide, l.maxLong)
	case c.MaxShortSide > 0:
		l.maxShort = c.MaxShortSide
	}
	switch {
	case c.MaxBytes < 0:
		return l, fmt.Errorf("%w: MaxBytes is negative", ErrInvalidConfig)
	default:
		l.maxBytes = c.MaxBytes
	}
	switch c.Format {
	case JPEG, PNG:
		l.format = c.Format
	default:
		return l, fmt.Errorf("%w: Format %d is not a format this package writes", ErrInvalidConfig, c.Format)
	}
	switch {
	case c.Quality < 0 || c.Quality > 100:
		return l, fmt.Errorf("%w: Quality is outside 1 to 100", ErrInvalidConfig)
	case c.Quality > 0:
		l.quality = c.Quality
	}
	switch {
	case c.MaxPixels < 0:
		return l, fmt.Errorf("%w: MaxPixels is negative", ErrInvalidConfig)
	case c.MaxPixels > 0:
		l.maxPixels = c.MaxPixels
	}
	switch {
	case c.MaxInputBytes < 0:
		return l, fmt.Errorf("%w: MaxInputBytes is negative", ErrInvalidConfig)
	case c.MaxInputBytes > 0:
		l.maxInputBytes = c.MaxInputBytes
	}
	return l, nil
}

// Normalize reads one image from r and returns it normalised. It decodes
// the JPEG, PNG and WebP formats, applies the EXIF orientation to the
// pixels, resizes under the configured limits without ever enlarging, and
// re-encodes from the decoded pixels, which strips every metadata block the
// input carried.
//
// An unusable Config returns an error wrapping ErrInvalidConfig. A refusal
// of the input returns an Error, and Code maps it into a response envelope.
// Every other error is a failure of the read or the encoders, not a
// judgement of the input. A non-nil Result comes back only with a nil
// error.
//
// Cancellation is checked between stages and between attempts of the
// byte-cap ladder, not inside one decode or resize. The read itself is
// bounded by MaxInputBytes, so a reader of unknown length cannot fill
// memory.
func Normalize(ctx context.Context, r io.Reader, cfg Config) (Result, error) {
	l, err := cfg.resolve()
	if err != nil {
		return Result{}, err
	}
	if r == nil {
		return Result{}, errors.New("photo: normalize: reader is nil")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	buf, err := readInput(r, l.maxInputBytes)
	if err != nil {
		return Result{}, err
	}
	header, _, err := image.DecodeConfig(bytes.NewReader(buf))
	if err != nil {
		return Result{}, refusal(codeUnsupportedFormat, fmt.Errorf("%w: %s", ErrUnsupportedFormat, err))
	}
	if int64(header.Width)*int64(header.Height) > l.maxPixels {
		return Result{}, refusal(codeTooManyPixels, fmt.Errorf("%w: header reports %d by %d pixels, over the limit of %d", ErrTooManyPixels, header.Width, header.Height, l.maxPixels))
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	src, _, err := image.Decode(bytes.NewReader(buf))
	if err != nil {
		return Result{}, refusal(codeUnsupportedFormat, fmt.Errorf("%w: %s", ErrUnsupportedFormat, err))
	}
	if o := orientationFromJPEG(buf); o != 1 {
		src = orient(src, o)
	}
	outW, outH := fitSize(src.Bounds().Dx(), src.Bounds().Dy(), l.maxLong, l.maxShort)
	out, w, h, err := encodeUnderCap(ctx, src, outW, outH, l)
	if err != nil {
		return Result{}, err
	}
	sum := sha256Hex(out)
	return Result{
		Bytes:  out,
		Width:  w,
		Height: h,
		Format: l.format,
		SHA256: sum,
	}, nil
}

// readInput reads the whole input bounded by max. The first sniffBytes
// bytes are read first, so the HEIC family refuses before the rest of a
// large input is read. An input over max refuses with its size named.
func readInput(r io.Reader, max int) ([]byte, error) {
	head, err := io.ReadAll(io.LimitReader(r, sniffBytes))
	if err != nil {
		return nil, fmt.Errorf("photo: read input: %w", err)
	}
	if isHEIFFamily(head) {
		return nil, refusal(codeHEIC, ErrHEIC)
	}
	rest, err := io.ReadAll(io.LimitReader(r, int64(max-len(head)+1)))
	if err != nil {
		return nil, fmt.Errorf("photo: read input: %w", err)
	}
	if len(head)+len(rest) > max {
		return nil, refusal(codeTooManyInputBytes, fmt.Errorf("%w: %d bytes read, limit %d", ErrTooManyInputBytes, len(head)+len(rest), max))
	}
	return append(head, rest...), nil
}

// heifBrands are the ISO BMFF brands of the still image family this package
// refuses. mif1 and msf1 are the generic multi-image brands, the heic and
// heix brands name the still HEVC codings, and the heim, heis, hevm and
// hevs brands name the layered and multi-image variants.
var heifBrands = map[string]bool{
	"heic": true,
	"heix": true,
	"hevc": true,
	"hevx": true,
	"heim": true,
	"heis": true,
	"hevm": true,
	"hevs": true,
	"mif1": true,
	"msf1": true,
}

// isHEIFFamily reports whether b begins with an ISO BMFF ftyp box whose
// major brand or compatible brands name the refused family. The box sits at
// the very start of the file, so a prefix is enough to judge it. A file
// that does not begin with ftyp, and a JPEG, is not judged here.
func isHEIFFamily(b []byte) bool {
	if len(b) < 12 || string(b[4:8]) != "ftyp" {
		return false
	}
	if b[0] == 0xFF && b[1] == 0xD8 {
		// A JPEG begins with the SOI marker, so an ftyp inside one is
		// payload bytes, never a box header.
		return false
	}
	end := len(b)
	if boxSize := int(uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])); boxSize >= 8 {
		end = min(boxSize, len(b))
	}
	if heifBrands[string(b[8:12])] {
		return true
	}
	// Compatible brands start after the minor version at 12.
	for off := 16; off+4 <= end; off += 4 {
		if heifBrands[string(b[off:off+4])] {
			return true
		}
	}
	return false
}

// fitSize reports the largest size at or under the side bounds that keeps
// the aspect of w by h, and never enlarges. Each bound rounds to nearest
// and keeps at least one pixel.
func fitSize(w, h, maxLong, maxShort int) (int, int) {
	scale := 1.0
	if max(w, h) > maxLong {
		scale = math.Min(scale, float64(maxLong)/float64(max(w, h)))
	}
	if maxShort > 0 && min(w, h) > maxShort {
		scale = math.Min(scale, float64(maxShort)/float64(min(w, h)))
	}
	if scale >= 1 {
		return w, h
	}
	return max(1, int(math.Round(float64(w)*scale))), max(1, int(math.Round(float64(h)*scale)))
}

// size is one candidate output size in the byte-cap ladder.
type size struct {
	w int
	h int
}

// scaleLadder lists the output sizes the byte-cap ladder tries, starting at
// the fitted size. Each step multiplies both sides by seven eighths, until
// the long side reaches the floor or a step can no longer shrink.
func scaleLadder(w, h int) []size {
	steps := []size{{w, h}}
	for max(w, h) > scaleFloorSide {
		nw := max(1, w*scaleStepNum/scaleStepDen)
		nh := max(1, h*scaleStepNum/scaleStepDen)
		if nw == w && nh == h {
			break
		}
		w, h = nw, nh
		steps = append(steps, size{w, h})
	}
	return steps
}

// qualityLadder lists the JPEG qualities the byte-cap ladder tries,
// starting at q and stepping down to the floor. A q already at or under the
// floor is tried alone, because the ladder never raises a quality.
func qualityLadder(q int) []int {
	var ladder []int
	for {
		ladder = append(ladder, q)
		if q <= qualityFloor {
			return ladder
		}
		q = max(qualityFloor, q-qualityStep)
	}
}

// encodeUnderCap renders the output. Without a byte cap it encodes once at
// the fitted size. With one, it walks the quality ladder at the fitted
// size, then the scale ladder with the quality ladder again at each step,
// and refuses once both are exhausted. The size it returns is the size of
// the bytes it returns.
func encodeUnderCap(ctx context.Context, src image.Image, w, h int, l limits) ([]byte, int, int, error) {
	if l.maxBytes == 0 {
		out, err := render(src, w, h, l.format, l.quality)
		if err != nil {
			return nil, 0, 0, err
		}
		return out, w, h, nil
	}
	qualities := qualityLadder(l.quality)
	if l.format == PNG {
		qualities = []int{0}
	}
	for _, s := range scaleLadder(w, h) {
		img := scaled(src, s.w, s.h)
		for _, q := range qualities {
			if err := ctx.Err(); err != nil {
				return nil, 0, 0, err
			}
			out, err := encodeImage(img, l.format, q)
			if err != nil {
				return nil, 0, 0, err
			}
			if len(out) <= l.maxBytes {
				return out, s.w, s.h, nil
			}
		}
	}
	return nil, 0, 0, refusal(codeOutputCannotFit, fmt.Errorf("%w: cap is %d bytes", ErrOutputCannotFit, l.maxBytes))
}

// scaled returns src resized to w by h with the CatmullRom filter, or src
// itself when it is already that size. The copy keeps four channels, so a
// resize never loses alpha.
func scaled(src image.Image, w, h int) image.Image {
	b := src.Bounds()
	if b.Dx() == w && b.Dy() == h {
		return src
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	return dst
}

// render is one output at one size, used when no byte cap asks for a
// ladder.
func render(src image.Image, w, h int, f Format, quality int) ([]byte, error) {
	return encodeImage(scaled(src, w, h), f, quality)
}

// encodeImage writes img in the format, with the quality a JPEG output
// honours.
func encodeImage(img image.Image, f Format, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if f == PNG {
		if err := png.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("photo: encode png: %w", err)
		}
		return buf.Bytes(), nil
	}
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("photo: encode jpeg: %w", err)
	}
	return buf.Bytes(), nil
}

// orient applies an EXIF orientation to the pixels with an exact orthogonal
// transform. The matrices map source pixel centres to destination pixel
// centres, so the nearest neighbour sampler lands on one pixel every time
// and no value is blended.
func orient(src image.Image, orientation int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	var m f64.Aff3
	var dw, dh int
	switch orientation {
	case 2: // column 0 names the visual right, so mirror left right
		m = f64.Aff3{-1, 0, float64(w), 0, 1, 0}
		dw, dh = w, h
	case 3: // row 0 names the visual bottom, so turn half a turn
		m = f64.Aff3{-1, 0, float64(w), 0, -1, float64(h)}
		dw, dh = w, h
	case 4: // row 0 names the visual bottom, so mirror top bottom
		m = f64.Aff3{1, 0, 0, 0, -1, float64(h)}
		dw, dh = w, h
	case 5: // row 0 names the visual left and column 0 the visual top, so transpose
		m = f64.Aff3{0, 1, 0, 1, 0, 0}
		dw, dh = h, w
	case 6: // row 0 names the visual right and column 0 the visual top, so turn clockwise
		m = f64.Aff3{0, -1, float64(h), 1, 0, 0}
		dw, dh = h, w
	case 7: // row 0 names the visual right and column 0 the visual bottom, so transpose across the other diagonal
		m = f64.Aff3{0, -1, float64(h), -1, 0, float64(w)}
		dw, dh = h, w
	case 8: // row 0 names the visual left and column 0 the visual bottom, so turn anticlockwise
		m = f64.Aff3{0, 1, 0, -1, 0, float64(w)}
		dw, dh = h, w
	default:
		return src
	}
	m[2] += float64(b.Min.X)
	m[5] += float64(b.Min.Y)
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	draw.NearestNeighbor.Transform(dst, m, src, b, draw.Src, nil)
	return dst
}

// sha256Hex hashes out and returns the digest as lowercase hex, the same
// shape an upload digest uses.
func sha256Hex(out []byte) string {
	sum := sha256.Sum256(out)
	return hex.EncodeToString(sum[:])
}
