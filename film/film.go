// Package film assembles a captioned still film: a title card, one segment
// per page, and an end card, joined by a stream copy.
//
// Every segment is the same 1080 by 1620 frame, so the join does not
// re-encode. A page is the still on top and a caption band beneath it. The
// band is wrapped in Go and drawn from a text file, which is separate from
// the word-timed cues in caption. A page with audio is held for the duration
// the caller already measured. A page without audio is held for SilentHold.
// Render returns that sum. It does not probe the finished file.
//
// The drawtext face is the caller's font file. The package ships none,
// because a runtime image often has no fonts of its own.
package film

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nrynss/keel/ffmpeg"
	"golang.org/x/sync/errgroup"
)

// ErrInvalid reports input that cannot be assembled. A missing title, page,
// font, or output, a narrated page with no duration, and a silent page that
// carries one all match it with errors.Is.
var ErrInvalid = errors.New("film: invalid input")

// DefaultTitleHold is the title card length when Config.TitleHold is unset.
const DefaultTitleHold = 3500 * time.Millisecond

// DefaultEndHold is the end card length when Config.EndHold is unset.
const DefaultEndHold = 3 * time.Second

// DefaultConcurrency is how many segments render at once when
// Config.Concurrency is unset.
const DefaultConcurrency = 4

// Config carries the holds, the font, and the end card's words. The zero
// value is not enough to render: FontFile and EndTitle are required.
type Config struct {
	// WorkDir is where scratch files are written. Empty uses the system temp
	// directory for Render, and the output's directory for a single segment.
	WorkDir string
	// TitleHold is the title card length. Zero or negative means
	// DefaultTitleHold.
	TitleHold time.Duration
	// EndHold is the end card length. Zero or negative means DefaultEndHold.
	EndHold time.Duration
	// FontFile is the drawtext face, a TTF or OTF path. Required.
	FontFile string
	// Concurrency limits how many segments render at once. Zero or negative
	// means DefaultConcurrency.
	Concurrency int
	// EndTitle is the line drawn on the end card. Required by Render and
	// EndCard.
	EndTitle string
	// Domain is an optional second line on the end card.
	Domain string
	// Artist is optional container metadata on the joined file.
	Artist string
	// Comment is optional container metadata on the joined file.
	Comment string
}

// Page is one still, its words, and its optional narration.
type Page struct {
	// N is the page number used in errors. Zero means the position in the
	// slice, starting at one.
	N int
	// ImagePath is the still. Image is used when ImagePath is empty.
	ImagePath string
	// Image is the still bytes, written to the work directory when ImagePath
	// is empty.
	Image []byte
	// AudioPath is the narration. Audio is used when AudioPath is empty and
	// the page is narrated.
	AudioPath string
	// Audio is the narration bytes. Empty, with an empty AudioPath, makes a
	// silent page whose hold comes from Text.
	Audio []byte
	// Text is the caption. Required.
	Text string
	// Duration is the narration's length, already measured. Required when the
	// page has audio, and refused when it does not.
	Duration time.Duration
}

// Input is one film.
type Input struct {
	// Title is drawn on the title card and stored as container metadata.
	// Required.
	Title string
	// Byline is an optional second line on the title card. A value that does
	// not already start with "by " gains that prefix.
	Byline string
	// Pages is the order of the film, after the title card and before the end
	// card. At least one page is required.
	Pages []Page
	// Output is the joined file. Required.
	Output string
}

// titleHold returns the title card length, substituting the default.
func (c Config) titleHold() time.Duration {
	if c.TitleHold <= 0 {
		return DefaultTitleHold
	}
	return c.TitleHold
}

// endHold returns the end card length, substituting the default.
func (c Config) endHold() time.Duration {
	if c.EndHold <= 0 {
		return DefaultEndHold
	}
	return c.EndHold
}

// concurrency returns the segment limit, substituting the default.
func (c Config) concurrency() int {
	if c.Concurrency <= 0 {
		return DefaultConcurrency
	}
	return c.Concurrency
}

// Total is the length Render returns for these pages: the title hold, each
// page hold, and the end hold. It checks the words and the duration rules.
// It does not read the files.
func Total(cfg Config, pages []Page) (time.Duration, error) {
	if len(pages) == 0 {
		return 0, fmt.Errorf("%w: no pages", ErrInvalid)
	}
	total := cfg.titleHold() + cfg.endHold()
	for i, page := range pages {
		n := pageNumber(i, page)
		if strings.TrimSpace(page.Text) == "" {
			return 0, fmt.Errorf("%w: page %d has no words", ErrInvalid, n)
		}
		hasAudio := page.AudioPath != "" || len(page.Audio) > 0
		if hasAudio && page.Duration <= 0 {
			return 0, fmt.Errorf("%w: page %d has narration but no duration", ErrInvalid, n)
		}
		if !hasAudio && page.Duration > 0 {
			return 0, fmt.Errorf("%w: page %d has a duration %s but no narration", ErrInvalid, n, page.Duration)
		}
		if hasAudio {
			total += page.Duration
		} else {
			total += SilentHold(page.Text)
		}
	}
	return total, nil
}

// Render writes the film to in.Output and returns its length from Total.
// tools names ffmpeg. A missing binary reports ffmpeg.ErrNotFound and a
// failed run reports ffmpeg.ErrFailed. The context stops every segment still
// running when one fails.
func Render(ctx context.Context, tools ffmpeg.Tools, cfg Config, in Input) (time.Duration, error) {
	if strings.TrimSpace(in.Title) == "" {
		return 0, fmt.Errorf("%w: title is required", ErrInvalid)
	}
	if strings.TrimSpace(in.Output) == "" {
		return 0, fmt.Errorf("%w: output path is required", ErrInvalid)
	}
	if strings.TrimSpace(cfg.EndTitle) == "" {
		return 0, fmt.Errorf("%w: end card title is required", ErrInvalid)
	}
	total, err := Total(cfg, in.Pages)
	if err != nil {
		return 0, err
	}
	if err := requireFont(cfg); err != nil {
		return 0, err
	}
	for i, page := range in.Pages {
		n := pageNumber(i, page)
		if page.ImagePath == "" && len(page.Image) == 0 {
			return 0, fmt.Errorf("%w: page %d has no image", ErrInvalid, n)
		}
		if page.ImagePath != "" {
			if _, err := os.Stat(page.ImagePath); err != nil {
				return 0, fmt.Errorf("%w: page %d image: %v", ErrInvalid, n, err)
			}
		}
		if page.AudioPath != "" {
			if _, err := os.Stat(page.AudioPath); err != nil {
				return 0, fmt.Errorf("%w: page %d audio: %v", ErrInvalid, n, err)
			}
		}
	}

	work, err := mkdirWork(cfg.WorkDir, "keel-film-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(work)
	cfg.WorkDir = work

	images := make([]string, len(in.Pages))
	audios := make([]string, len(in.Pages))
	for i, page := range in.Pages {
		n := pageNumber(i, page)
		if page.ImagePath != "" {
			images[i] = page.ImagePath
		} else {
			path := filepath.Join(work, fmt.Sprintf("page-%02d.bin", n))
			if err := os.WriteFile(path, page.Image, 0o600); err != nil {
				return 0, fmt.Errorf("write page %d image: %w", n, err)
			}
			images[i] = path
		}
		if page.AudioPath != "" {
			audios[i] = page.AudioPath
		} else if len(page.Audio) > 0 {
			path := filepath.Join(work, fmt.Sprintf("narration-%02d.bin", n))
			if err := os.WriteFile(path, page.Audio, 0o600); err != nil {
				return 0, fmt.Errorf("write page %d audio: %w", n, err)
			}
			audios[i] = path
		}
	}

	titleSeg := filepath.Join(work, "seg-title.mp4")
	pageSegs := make([]string, len(in.Pages))
	for i := range in.Pages {
		pageSegs[i] = filepath.Join(work, fmt.Sprintf("seg-%02d.mp4", i+1))
	}
	endSeg := filepath.Join(work, "seg-end.mp4")

	fns := make([]func(context.Context) error, 0, len(in.Pages)+2)
	fns = append(fns, func(ctx context.Context) error {
		return TitleCard(ctx, tools, cfg, images[0], in.Title, in.Byline, titleSeg)
	})
	for i := range in.Pages {
		i := i
		fns = append(fns, func(ctx context.Context) error {
			return PageSegment(ctx, tools, cfg, images[i], in.Pages[i].Text, audios[i], in.Pages[i].Duration, pageSegs[i])
		})
	}
	fns = append(fns, func(ctx context.Context) error {
		return EndCard(ctx, tools, cfg, endSeg)
	})
	if err := runLimited(ctx, cfg.concurrency(), fns); err != nil {
		return 0, err
	}

	segments := make([]string, 0, len(pageSegs)+2)
	segments = append(segments, titleSeg)
	segments = append(segments, pageSegs...)
	segments = append(segments, endSeg)
	tmp := filepath.Join(work, "final.mp4")
	if err := Concat(ctx, tools, cfg, segments, in.Title, tmp); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(in.Output), 0o755); err != nil {
		return 0, fmt.Errorf("create output directory: %w", err)
	}
	if err := moveFile(tmp, in.Output); err != nil {
		return 0, err
	}
	return total, nil
}

// pageNumber returns the number an error should name.
func pageNumber(i int, page Page) int {
	if page.N > 0 {
		return page.N
	}
	return i + 1
}

// mkdirWork creates a directory under parent, or under the system temp
// directory when parent is empty.
func mkdirWork(parent, pattern string) (string, error) {
	dir, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		return "", fmt.Errorf("create work directory: %w", err)
	}
	return dir, nil
}

// runLimited runs fns with at most n in flight. The first error cancels the
// rest and is returned. A limit below one still runs one segment at a time.
func runLimited(ctx context.Context, n int, fns []func(context.Context) error) error {
	if n < 1 {
		n = 1
	}
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(n)
	for _, fn := range fns {
		fn := fn
		g.Go(func() error {
			return fn(ctx)
		})
	}
	return g.Wait()
}
