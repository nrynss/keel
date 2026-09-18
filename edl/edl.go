// Package edl renders an edit decision list into one audio file.
//
// A list holds kept time ranges of one source file. Render merges ranges
// that touch or overlap, joins the merged ranges with crossfades or hard
// cuts, and normalises loudness in two passes. The first pass asks the
// loudnorm filter to measure the cut. The second pass applies the measured
// numbers and writes the output. The measured numbers are read from the
// stats file the tool itself writes, never guessed.
//
// The input is times, not words. A caller that works from words converts
// them first. The package invokes ffmpeg and ffprobe only through the
// ffmpeg package, and every invocation carries the caller's context, so a
// cancelled render stops the child process.
//
// When ffmpeg normalises linearly, the rendered duration equals the kept
// ranges minus the crossfade overlaps, and the output keeps the sample rate
// and channel layout of the source. ffmpeg parses durations at microsecond
// precision, so finer detail in a range is lost.
package edl

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nrynss/keel/ffmpeg"
)

// ErrNoSegments reports an empty cut list. A render needs at least one kept
// range.
var ErrNoSegments = errors.New("edl: no segments")

// ErrInvalidSegment reports a kept range whose start is negative or whose
// end does not come after its start.
var ErrInvalidSegment = errors.New("edl: invalid segment")

// ErrNoDestination reports an empty output path.
var ErrNoDestination = errors.New("edl: no destination")

// ErrInvalidTarget reports a loudness target outside the range loudnorm
// accepts, which is -70 to -5 LUFS.
var ErrInvalidTarget = errors.New("edl: invalid loudness target")

// ErrNoMeasurement reports that the measure pass left no readable stats.
// The tool ran but its output held nothing the package could parse.
var ErrNoMeasurement = errors.New("edl: loudness measurement missing")

// Segment is one kept time range of the source. Start is inclusive and End
// is exclusive. End must come after Start, and Start must not be negative.
type Segment struct {
	Start time.Duration
	End   time.Duration
}

// Config controls a render. The zero value renders a hard cut at every join
// and targets the loudnorm default of -24 LUFS.
type Config struct {
	// Crossfade is the fade length at a join. A join carries the fade only
	// when both neighbours are at least this long, and a shorter neighbour
	// makes the join a cut.
	Crossfade time.Duration
	// Target is the integrated loudness the render aims at, in LUFS. Zero
	// means the loudnorm default of -24. The loudness range and true peak
	// targets stay at the loudnorm defaults.
	Target float64
}

// crossfade resolves the configured fade length. Negative values mean no
// fade, like zero.
func (c Config) crossfade() time.Duration {
	if c.Crossfade < 0 {
		return 0
	}
	return c.Crossfade
}

// Render cuts the kept ranges out of the source file and writes one
// normalised audio file to dst.
//
// Ranges that touch or overlap merge before anything renders, so no zero
// length or negative range reaches the filter graph. Each join between two
// merged ranges carries the configured crossfade when both neighbours are
// long enough, and a hard cut otherwise. The output keeps the sample rate
// and channel layout of the source when ffmpeg normalises linearly.
//
// A missing binary reports ffmpeg.ErrNotFound, a failing pass reports
// ffmpeg.ErrFailed, and a cancelled context stops the child and reports the
// context error.
func Render(ctx context.Context, tools ffmpeg.Tools, src string, cfg Config, dst string, segments ...Segment) error {
	if len(segments) == 0 {
		return ErrNoSegments
	}
	for i, s := range segments {
		if s.Start < 0 || s.End <= s.Start {
			return fmt.Errorf("segment %d: %w", i, ErrInvalidSegment)
		}
	}
	if dst == "" {
		return ErrNoDestination
	}
	if cfg.Target != 0 && (cfg.Target < -70 || cfg.Target > -5) {
		return ErrInvalidTarget
	}

	format, err := ffmpeg.AudioFormat(ctx, tools, src)
	if err != nil {
		return fmt.Errorf("probe source: %w", err)
	}

	merged := merge(segments)
	cut := cutGraph(merged, joinFades(cfg.crossfade(), merged))

	measured, err := measureLoudness(ctx, tools, src, cut)
	if err != nil {
		return fmt.Errorf("measure loudness: %w", err)
	}
	if err := applyLoudness(ctx, tools, src, cut, measured, format, cfg, dst); err != nil {
		return fmt.Errorf("render cut: %w", err)
	}
	return nil
}
