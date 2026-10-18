// Package mix places clips on a timeline and renders them into one
// deterministic audio file, for a sound design app laying effects over
// picture or a dubbing app placing lines into slots over a background.
//
// A Timeline holds named tracks of clips. A clip takes a range of its
// source, an in point and a length, and places it at an offset with a
// constant gain in decibels and optional fades at either end. An optional
// bed runs under the whole timeline, looping when it is shorter, and ducks
// under the tracks it is keyed on through a sidechain compressor whose
// settings live in Config. Loudness normalisation is optional and runs the
// same two passes edl runs, from the same shared code.
//
// Determinism is a contract. Build sorts and validates the timeline and
// returns the filter graph as a pure function, so the same timeline always
// asks ffmpeg for the same work: tracks in name order, clips by offset and
// then by every other field. Every position, length and fade is counted in
// samples at the output rate, so no time is rounded twice. The render
// strips metadata and asks for bitexact output, and the same timeline
// renders byte-identical audio on the pinned ffmpeg 9.0.1. Another ffmpeg
// build may resample or encode differently, so the promise holds for that
// version only.
//
// The tracks sum without normalisation, so adding a track never lowers the
// others. Headroom is the caller's gain to set, or the loudness pass's.
// Mux puts a rendered mix under a video's picture with a stream copy.
//
// Every invocation goes through the ffmpeg package and carries the
// caller's context, so a cancelled render stops the child.
package mix

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/nrynss/keel/ffmpeg"
	"github.com/nrynss/keel/internal/loudness"
)

// ErrInvalid reports a timeline, config or path a render cannot use. The
// error names the clip, track or field at fault.
var ErrInvalid = errors.New("mix: invalid timeline")

// ErrNoMeasurement reports that the loudness measure pass left no usable
// stats. Silence measures as -inf, so a silent mix reports it too.
var ErrNoMeasurement = errors.New("mix: loudness measurement missing")

// DefaultSampleRate is the output rate a zero Config.SampleRate means.
const DefaultSampleRate = 48000

// DefaultLayout is the output channel layout an empty Config.Layout means.
const DefaultLayout = "stereo"

// The duck defaults pull a music bed well under speech within a syllable
// and let it recover over a breath.
const (
	// DefaultDuckThreshold is the keyed level, in dBFS, above which the
	// bed ducks.
	DefaultDuckThreshold = -30.0
	// DefaultDuckRatio is how hard the bed is compressed above the
	// threshold.
	DefaultDuckRatio = 8.0
	// DefaultDuckAttack is how fast the bed ducks once the key speaks.
	DefaultDuckAttack = 20 * time.Millisecond
	// DefaultDuckRelease is how fast the bed recovers once the key stops.
	DefaultDuckRelease = 400 * time.Millisecond
)

// Clip is one range of a source placed on the timeline.
type Clip struct {
	// Source is the audio or video file the clip reads its first audio
	// stream from.
	Source string
	// In is where the range starts in the source. It must not be negative.
	In time.Duration
	// Length is how long the range runs. It must be positive. A source
	// that ends before In plus Length is padded with silence to Length.
	Length time.Duration
	// Offset is where the clip starts on the timeline. It must not be
	// negative.
	Offset time.Duration
	// Gain is the clip's constant gain in decibels. Zero keeps its level.
	Gain float64
	// FadeIn and FadeOut are the fade lengths at the clip's start and
	// end. They must not be negative, and together must fit in Length.
	FadeIn  time.Duration
	FadeOut time.Duration
}

// Track is a named group of clips. Clips on one track may overlap, and
// overlapping clips sum.
type Track struct {
	// Name identifies the track, so a bed can be keyed on it. Names must be
	// unique and not empty.
	Name string
	// Clips are the track's clips, in any order. A track needs at least
	// one.
	Clips []Clip
}

// Bed is a source that runs under the whole timeline, looped when it is
// shorter and cut when it is longer.
type Bed struct {
	// Source is the bed's audio or video file.
	Source string
	// Gain is the bed's constant gain in decibels. Zero keeps its level.
	Gain float64
	// DuckOn names the tracks whose sum ducks the bed. Each must name a
	// track of the timeline. Empty means the bed never ducks.
	DuckOn []string
}

// Timeline is everything one render places.
type Timeline struct {
	// Tracks hold the clips. A timeline needs at least one track.
	Tracks []Track
	// Bed, when set, runs under the tracks.
	Bed *Bed
	// Length is the rendered length. Zero means the end of the clip that
	// ends last. A clip past Length is cut at it.
	Length time.Duration
}

// Duck tunes the sidechain compressor that ducks the bed. A zero field
// takes its default.
type Duck struct {
	// Threshold is the keyed level in dBFS above which the bed ducks,
	// from -60 to 0. Zero means DefaultDuckThreshold; 0 dBFS itself
	// cannot be asked for, since nothing louder exists to duck under.
	Threshold float64
	// Ratio is the compression ratio above the threshold, from 1 to 20.
	// Zero means DefaultDuckRatio.
	Ratio float64
	// Attack is how fast the bed ducks, from 0.01 ms to 2 s. Zero means
	// DefaultDuckAttack.
	Attack time.Duration
	// Release is how fast the bed recovers, from 0.01 ms to 9 s. Zero
	// means DefaultDuckRelease.
	Release time.Duration
}

// Config controls a render. The zero value renders 48 kHz stereo without
// loudness normalisation and ducks with the defaults.
type Config struct {
	// SampleRate is the output rate in hertz. Zero means
	// DefaultSampleRate. Every clip is resampled to it.
	SampleRate int
	// Layout is the output channel layout as ffmpeg names it, such as
	// mono or stereo. Empty means DefaultLayout.
	Layout string
	// Duck tunes the bed's ducking.
	Duck Duck
	// Normalize runs two-pass loudness normalisation over the mix.
	Normalize bool
	// Target is the integrated loudness Normalize aims at, in LUFS, from
	// -70 to -5. Zero means -24, the loudnorm default.
	Target float64
}

// Render writes the timeline to out, its codec chosen by out's extension.
// The graph is the one Build returns; with Config.Normalize the graph runs
// twice, once to measure and once to apply. A file already at out is
// replaced.
//
// A timeline Build refuses, an empty out, or an out that names a source
// reports ErrInvalid. Stats the measure pass cannot use report
// ErrNoMeasurement. A missing binary reports ffmpeg.ErrNotFound, a run
// that fails reports ffmpeg.ErrFailed, and a cancelled context stops the
// child and reports the context error.
func Render(ctx context.Context, tools ffmpeg.Tools, cfg Config, tl Timeline, out string) error {
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("%w: render needs an output path", ErrInvalid)
	}
	g, err := Build(tl, cfg)
	if err != nil {
		return err
	}
	dst, err := filepath.Abs(out)
	if err != nil {
		return fmt.Errorf("mix %s: %w", out, err)
	}
	for _, in := range g.Inputs {
		if samePath(in.Source, dst) {
			return fmt.Errorf("%w: output %s is also a source", ErrInvalid, out)
		}
	}
	inputs := g.args()
	graph, label := g.Filter, OutputLabel
	if cfg.Normalize {
		m, err := loudness.Measure(ctx, tools, inputs, g.Filter, OutputLabel, ErrNoMeasurement)
		if err != nil {
			return fmt.Errorf("mix %s: measure loudness: %w", out, err)
		}
		rate, layout := cfg.format()
		graph = g.Filter + ";[" + OutputLabel + "]loudnorm=" + loudness.Options(m, cfg.Target) + "," +
			loudness.Restore(rate, layout) + "[normalized]"
		label = "normalized"
	}
	args := append([]string{"-nostdin", "-v", "error", "-y", "-filter_complex_threads", "1"}, inputs...)
	args = append(args, "-filter_complex", graph, "-map", "["+label+"]",
		"-map_metadata", "-1", "-fflags", "+bitexact", "-flags:a", "+bitexact", dst)
	if err := ffmpeg.Run(ctx, tools, args...); err != nil {
		return fmt.Errorf("mix %s: %w", out, err)
	}
	return nil
}

// Mux writes video's picture and audio's first audio stream to out. The
// picture is stream copied, so it is never encoded again, and the audio is
// encoded with the codec out's container takes by default, such as AAC in
// MP4. The output runs as long as the longer of the two; render the mix to
// the picture's length to keep them aligned. A file already at out is
// replaced.
//
// An empty path or an out that names an input reports ErrInvalid. A
// missing binary reports ffmpeg.ErrNotFound, a run that fails reports
// ffmpeg.ErrFailed, and a cancelled context stops the child and reports
// the context error.
func Mux(ctx context.Context, tools ffmpeg.Tools, video, audio, out string) error {
	if strings.TrimSpace(video) == "" || strings.TrimSpace(audio) == "" || strings.TrimSpace(out) == "" {
		return fmt.Errorf("%w: mux needs a video, an audio and an output path", ErrInvalid)
	}
	if samePath(video, out) || samePath(audio, out) {
		return fmt.Errorf("%w: mux would overwrite its input %s", ErrInvalid, out)
	}
	paths := make([]string, 3)
	for i, p := range []string{video, audio, out} {
		abs, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("mux %s: %w", out, err)
		}
		paths[i] = abs
	}
	err := ffmpeg.Run(ctx, tools, "-nostdin", "-v", "error", "-y",
		"-i", paths[0], "-i", paths[1],
		"-map", "0:v:0", "-map", "1:a:0", "-c:v", "copy",
		"-map_metadata", "-1", "-fflags", "+bitexact", "-flags:a", "+bitexact",
		paths[2])
	if err != nil {
		return fmt.Errorf("mux %s: %w", out, err)
	}
	return nil
}

// samePath reports whether two paths name one file once cleaned and made
// absolute.
func samePath(a, b string) bool {
	if aa, err := filepath.Abs(a); err == nil {
		a = aa
	}
	if bb, err := filepath.Abs(b); err == nil {
		b = bb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
