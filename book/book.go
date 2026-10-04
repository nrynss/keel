// Package book assembles an illustrated PDF: one page per image, with an
// optional caption under it.
//
// The PDF library stays inside this package. Callers pass a font directory
// and a file name, page size, and margins, and Write returns an error
// instead of panicking when a page cannot be drawn. The result is image
// pages and captions. It is not a typesetter. A caption is measured as
// UTF-8, the same way it is drawn. The library does not shape complex
// scripts, so a caption that needs shaping is drawn as a sequence of glyphs.
package book

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/go-pdf/fpdf"
)

const (
	// DefaultMargin is the page margin, in millimetres, when Config.Margin
	// is unset.
	DefaultMargin = 15
	// DefaultFontSize is the caption size, in points, when Config.FontSize
	// is unset.
	DefaultFontSize = 12
	// DefaultPageSize is the fpdf page name used when Config.PageSize is empty.
	DefaultPageSize = "A4"
	// DefaultOrientation is portrait.
	DefaultOrientation = "P"

	fontFamily = "book"
	captionGap = 4
	lineFactor = 1.2
)

// ErrInvalid reports input that cannot be assembled: no pages, a page with
// neither an image nor a caption, a caption without a font file, a font
// file that is not a bare name, or a caption that does not fit the page.
var ErrInvalid = errors.New("book: invalid input")

// ErrEncode reports a failure while drawing or writing the PDF, including
// an image the decoder refused and a panic raised by the PDF library.
var ErrEncode = errors.New("book: encode failed")

// Config carries the font, the margins, and the page size. The zero value
// draws image-only pages on A4 with DefaultMargin. A caption needs FontDir
// and FontFile.
type Config struct {
	// FontDir is the directory the caption face is loaded from. Required
	// when any page has a caption.
	FontDir string
	// FontFile is the face's file name inside FontDir, a TTF or OTF.
	// A path separator is refused, so the name cannot leave FontDir.
	FontFile string
	// FontSize is the caption size in points. Zero or negative means
	// DefaultFontSize.
	FontSize float64
	// Margin is the page margin in millimetres. Zero or negative means
	// DefaultMargin.
	Margin float64
	// PageSize is an fpdf page name such as A4 or Letter. Empty means
	// DefaultPageSize.
	PageSize string
	// Orientation is P or L. Empty means DefaultOrientation.
	Orientation string
}

// Page is one sheet. Image is optional and, when set, ImageType names its
// format: png, jpg, jpeg, or gif. Caption is drawn under the image in the
// configured face. A page needs at least one of the two.
type Page struct {
	Image     io.Reader
	ImageType string
	Caption   string
}

// resolved is Config with defaults filled in.
type resolved struct {
	Config
	fontSize    float64
	margin      float64
	pageSize    string
	orientation string
}

func (cfg Config) resolve() (resolved, error) {
	out := resolved{Config: cfg, fontSize: cfg.FontSize, margin: cfg.Margin, pageSize: cfg.PageSize, orientation: cfg.Orientation}
	if out.fontSize <= 0 {
		out.fontSize = DefaultFontSize
	}
	if out.margin <= 0 {
		out.margin = DefaultMargin
	}
	if out.pageSize == "" {
		out.pageSize = DefaultPageSize
	}
	if out.orientation == "" {
		out.orientation = DefaultOrientation
	}
	out.orientation = strings.ToUpper(out.orientation)
	if out.orientation != "P" && out.orientation != "L" {
		return resolved{}, fmt.Errorf("book: write: %w: orientation %q", ErrInvalid, cfg.Orientation)
	}
	if strings.ContainsAny(cfg.FontFile, `/\`) {
		return resolved{}, fmt.Errorf("book: write: %w: font file must be a name inside FontDir", ErrInvalid)
	}
	return out, nil
}

// Write draws pages into w as a PDF. A cancelled ctx stops the draw before
// any byte is written. A nil ctx is background.
func Write(ctx context.Context, w io.Writer, cfg Config, pages []Page) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("book: write: %w: %v", ErrEncode, rec)
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil {
		return fmt.Errorf("book: write: %w: writer must not be nil", ErrInvalid)
	}
	if len(pages) == 0 {
		return fmt.Errorf("book: write: %w: no pages", ErrInvalid)
	}
	resolved, err := cfg.resolve()
	if err != nil {
		return err
	}
	needsFont := false
	for _, page := range pages {
		if page.Image == nil && page.Caption == "" {
			return fmt.Errorf("book: write: %w: a page needs an image or a caption", ErrInvalid)
		}
		if page.Image != nil && imageType(page.ImageType) == "" {
			return fmt.Errorf("book: write: %w: image type %q", ErrInvalid, page.ImageType)
		}
		if page.Caption != "" {
			needsFont = true
		}
	}
	if needsFont && (resolved.FontDir == "" || resolved.FontFile == "") {
		return fmt.Errorf("book: write: %w: a caption needs FontDir and FontFile", ErrInvalid)
	}

	pdf := fpdf.New(resolved.orientation, "mm", resolved.pageSize, resolved.FontDir)
	pdf.SetMargins(resolved.margin, resolved.margin, resolved.margin)
	pdf.SetAutoPageBreak(false, resolved.margin)
	if needsFont {
		pdf.AddUTF8Font(fontFamily, "", resolved.FontFile)
		if err := encodeErr(pdf); err != nil {
			return err
		}
		pdf.SetFont(fontFamily, "", resolved.fontSize)
		if err := encodeErr(pdf); err != nil {
			return err
		}
	}

	for i, page := range pages {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := drawPage(pdf, resolved, i, page); err != nil {
			return err
		}
	}
	if err := pdf.Output(w); err != nil {
		return fmt.Errorf("book: write: %w: %w", ErrEncode, err)
	}
	return encodeErr(pdf)
}

// drawPage adds one sheet. The image is scaled to fit the content box
// above the caption, keeping its aspect ratio.
func drawPage(pdf *fpdf.Fpdf, cfg resolved, index int, page Page) error {
	pdf.AddPage()
	if err := encodeErr(pdf); err != nil {
		return err
	}
	pageW, pageH := pdf.GetPageSize()
	contentW := pageW - 2*cfg.margin
	contentH := pageH - 2*cfg.margin
	if contentW <= 0 || contentH <= 0 {
		return fmt.Errorf("book: write: %w: margin leaves no page", ErrInvalid)
	}

	var captionH, lineH float64
	if page.Caption != "" {
		pdf.SetFont(fontFamily, "", cfg.fontSize)
		_, unit := pdf.GetFontSize()
		lineH = unit * lineFactor
		// SplitText walks runes, which is what MultiCell uses for a UTF-8
		// face. SplitLines walks bytes and both under- and over-measures
		// a caption that is not ASCII.
		lines := pdf.SplitText(page.Caption, contentW)
		captionH = float64(len(lines)) * lineH
		if captionH > contentH {
			return fmt.Errorf("book: write: %w: caption is taller than the page", ErrInvalid)
		}
	}
	budget := contentH - captionH
	if page.Image != nil && page.Caption != "" {
		budget -= captionGap
	}
	if page.Image != nil && budget <= 0 {
		return fmt.Errorf("book: write: %w: caption leaves no room for the image", ErrInvalid)
	}

	y := cfg.margin
	if page.Image != nil {
		name := fmt.Sprintf("page-%d", index)
		kind := imageType(page.ImageType)
		info := pdf.RegisterImageOptionsReader(name, fpdf.ImageOptions{ImageType: kind, ReadDpi: true}, page.Image)
		if err := encodeErr(pdf); err != nil {
			return err
		}
		if info == nil {
			return fmt.Errorf("book: write: %w: image was not registered", ErrEncode)
		}
		iw, ih := info.Width(), info.Height()
		if iw <= 0 || ih <= 0 {
			return fmt.Errorf("book: write: %w: image has no extent", ErrInvalid)
		}
		scale := contentW / iw
		if ih*scale > budget {
			scale = budget / ih
		}
		dw, dh := iw*scale, ih*scale
		x := cfg.margin + (contentW-dw)/2
		pdf.ImageOptions(name, x, y, dw, dh, false, fpdf.ImageOptions{ImageType: kind}, 0, "")
		if err := encodeErr(pdf); err != nil {
			return err
		}
		y += dh
		if page.Caption != "" {
			y += captionGap
		}
	}
	if page.Caption != "" {
		pdf.SetXY(cfg.margin, y)
		pdf.MultiCell(contentW, lineH, page.Caption, "", "L", false)
		if err := encodeErr(pdf); err != nil {
			return err
		}
	}
	return nil
}

// imageType returns the fpdf type name, or empty when the type is not one
// this package accepts. jpeg is spelled jpg, which is what the decoder expects.
func imageType(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "png":
		return "png"
	case "jpg", "jpeg":
		return "jpg"
	case "gif":
		return "gif"
	default:
		return ""
	}
}

// encodeErr wraps the PDF library's sticky error.
func encodeErr(pdf *fpdf.Fpdf) error {
	if err := pdf.Error(); err != nil {
		return fmt.Errorf("book: write: %w: %w", ErrEncode, err)
	}
	return nil
}
