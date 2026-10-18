// Package fit stretches or squeezes a take into a time slot without
// changing its pitch, for dubbing, voiceover and ADR, where a synthesized
// line rarely lasts exactly as long as the slot it must fill.
//
// The ratio a take needs is its own length over the slot's length, so a
// ratio above 1 speeds the take up and a ratio below 1 slows it down.
// Bounds name the slowest and fastest ratio the caller accepts. The tempo
// change runs through ffmpeg's atempo filter, which keeps pitch and keeps
// each instance within 0.5 to 2 here, the range it works best in, so a
// larger change becomes a chain of instances that AtempoChain builds.
//
// A take longer than the slot that needs a ratio above Fastest is refused
// with a *RatioError carrying the ratio it would need, and the caller
// decides whether to resynthesize, cut the script or widen the bounds. The
// library never trims speech to make a take fit. A take shorter than the
// slot is never refused: it is slowed as far as Slowest allows and the
// remainder is padded with silence at the end, which is always safe to
// add. After the tempo change the audio is padded or trimmed to exactly the
// sample count the slot holds at the source's sample rate, so rounding in
// the filter never leaks into the length.
//
// Fit invokes ffmpeg and ffprobe only through the ffmpeg package, and every
// invocation carries the caller's context, so a cancelled fit stops its
// child. The lengths are pinned on the pinned ffmpeg 9.0.1.
package fit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nrynss/keel/ffmpeg"
)

// DefaultSlowest is the slowest ratio a zero Bounds.Slowest accepts. A take
// slowed to 0.8 of its pace still sounds like natural speech.
const DefaultSlowest = 0.8

// DefaultFastest is the fastest ratio a zero Bounds.Fastest accepts. A take
// sped up by a quarter stays intelligible, and beyond it speech starts to
// sound hurried.
const DefaultFastest = 1.25

// ErrInvalid reports arguments Fit cannot use: an empty or shared path, a
// slot that is not positive, or bounds that do not hold 1 between them.
var ErrInvalid = errors.New("fit: invalid request")

// ErrOutOfBounds reports a take that needs a ratio faster than the bounds
// allow. The error Fit returns is a *RatioError that wraps it.
var ErrOutOfBounds = errors.New("fit: ratio out of bounds")

// Bounds are the ratios a fit may apply. A zero field takes its default,
// so the zero Bounds accepts 0.8 up to 1.25. Slowest must be above 0 and
// at most 1, and Fastest at least 1. A Slowest of 1 never slows a take, so
// a short take is only padded, and a Fastest of 1 never speeds one up.
type Bounds struct {
	// Slowest is the lowest ratio applied, the most a short take is
	// slowed before silence pads the rest. Zero means DefaultSlowest.
	Slowest float64
	// Fastest is the highest ratio accepted. A take that needs more is
	// refused with a *RatioError. Zero means DefaultFastest.
	Fastest float64
}

// resolve fills the defaults and checks the bounds.
func (b Bounds) resolve() (Bounds, error) {
	if b.Slowest == 0 {
		b.Slowest = DefaultSlowest
	}
	if b.Fastest == 0 {
		b.Fastest = DefaultFastest
	}
	if !(b.Slowest > 0 && b.Slowest <= 1) || !(b.Fastest >= 1) || math.IsInf(b.Fastest, 0) {
		return Bounds{}, fmt.Errorf("%w: bounds %v to %v must hold 1, above 0 and finite", ErrInvalid, b.Slowest, b.Fastest)
	}
	return b, nil
}

// RatioError reports a take that needs a ratio outside its bounds. It
// wraps ErrOutOfBounds.
type RatioError struct {
	// Ratio is the ratio the take would need to fill the slot exactly,
	// its length over the slot's length.
	Ratio float64
	// Bounds are the resolved bounds the ratio fell outside.
	Bounds Bounds
}

// Error describes the needed ratio and the bounds.
func (e *RatioError) Error() string {
	return fmt.Sprintf("fit: take needs ratio %.4f, above the fastest %v", e.Ratio, e.Bounds.Fastest)
}

// Unwrap returns ErrOutOfBounds, so errors.Is matches it.
func (e *RatioError) Unwrap() error { return ErrOutOfBounds }

// Result is what Fit wrote.
type Result struct {
	// Ratio is the tempo ratio applied, 1 when the tempo was kept.
	Ratio float64
	// Padded is the silence added at the end of a take shorter than the
	// slot that Slowest kept from slowing all the way, computed from the
	// sample counts. It is zero when the tempo change fills the slot, where
	// only the filter's rounding is padded or trimmed.
	Padded time.Duration
	// Duration is the written file's length as ffmpeg.Duration measures
	// it, rounded to the nearest millisecond.
	Duration time.Duration
}

// Fit writes the first audio stream at in to out, fitted to target. The
// source's length and sample rate are measured with ffmpeg.Duration and
// ffmpeg.AudioFormat, so the length is the container's, which for a take
// of one audio stream is the take itself. The output keeps the source's sample rate and
// channel layout, its codec follows the extension of out, and it holds
// exactly round(target × sample rate) samples before the encoder runs; a
// PCM container such as WAV keeps that count, and a lossy codec may add
// its own padding, which Result.Duration shows. A file already at out is
// replaced.
//
// A take that needs a ratio above bounds.Fastest returns a *RatioError
// and writes nothing. An empty path, out naming in, a target that is not
// positive, or invalid bounds report ErrInvalid. A missing binary reports
// ffmpeg.ErrNotFound, a run that fails reports ffmpeg.ErrFailed, and a
// cancelled context stops the child and reports the context error.
func Fit(ctx context.Context, tools ffmpeg.Tools, in, out string, target time.Duration, bounds Bounds) (Result, error) {
	if in == "" || out == "" {
		return Result{}, fmt.Errorf("%w: fit needs a source and an output path", ErrInvalid)
	}
	if target <= 0 {
		return Result{}, fmt.Errorf("%w: slot %s must be positive", ErrInvalid, target)
	}
	b, err := bounds.resolve()
	if err != nil {
		return Result{}, err
	}
	src, err := filepath.Abs(in)
	if err != nil {
		return Result{}, fmt.Errorf("fit %s: %w", in, err)
	}
	dst, err := filepath.Abs(out)
	if err != nil {
		return Result{}, fmt.Errorf("fit %s: %w", in, err)
	}
	if filepath.Clean(src) == filepath.Clean(dst) {
		return Result{}, fmt.Errorf("%w: fit would overwrite its source %s", ErrInvalid, in)
	}
	length, err := ffmpeg.Duration(ctx, tools, src)
	if err != nil {
		return Result{}, fmt.Errorf("fit %s: %w", in, err)
	}
	if length <= 0 {
		return Result{}, fmt.Errorf("%w: take %s has no length", ErrInvalid, in)
	}
	format, err := ffmpeg.AudioFormat(ctx, tools, src)
	if err != nil {
		return Result{}, fmt.Errorf("fit %s: %w", in, err)
	}
	if format.SampleRate <= 0 {
		return Result{}, fmt.Errorf("fit %s: sample rate %d is unusable", in, format.SampleRate)
	}

	plan, err := planFit(length, target, format.SampleRate, b)
	if err != nil {
		return Result{}, err
	}
	err = ffmpeg.Run(ctx, tools,
		"-nostdin", "-v", "error", "-y", "-i", src,
		"-map", "0:a:0", "-vn", "-sn", "-dn",
		"-af", plan.filter,
		"-map_metadata", "-1", "-fflags", "+bitexact", "-flags:a", "+bitexact",
		dst)
	if err != nil {
		return Result{}, fmt.Errorf("fit %s: %w", in, err)
	}
	written, err := ffmpeg.Duration(ctx, tools, dst)
	if err != nil {
		return Result{}, fmt.Errorf("fit %s: measure %s: %w", in, out, err)
	}
	return Result{Ratio: plan.ratio, Padded: plan.padded, Duration: written}, nil
}

// plan is the filter a fit runs and what it promises.
type plan struct {
	ratio  float64
	padded time.Duration
	filter string
}

// planFit decides the ratio and builds the filter for a take of length
// fitted into target at rate hertz. It is pure, so the decision is tested
// without ffmpeg.
func planFit(length, target time.Duration, rate int, b Bounds) (plan, error) {
	need := float64(length) / float64(target)
	if need > b.Fastest {
		return plan{}, &RatioError{Ratio: need, Bounds: b}
	}
	ratio := max(need, b.Slowest)
	// A take within a sample of the slot keeps its tempo, so a take that
	// already fits is copied through without the filter's resampling.
	samples := int64(math.Round(target.Seconds() * float64(rate)))
	have := int64(math.Round(length.Seconds() * float64(rate)))
	if have == samples {
		ratio = 1
	}
	chain, err := AtempoChain(ratio)
	if err != nil {
		return plan{}, err
	}
	// Silence fills the slot only when the ratio applied is slower than
	// the one the take needs, because Slowest held it back. Otherwise the
	// tempo change fills the slot and the pad and trim below only absorb
	// the filter's rounding.
	var padded time.Duration
	if ratio > need {
		stretched := int64(math.Round(float64(have) / ratio))
		if stretched < samples {
			padded = time.Duration(math.Round(float64(samples-stretched) * float64(time.Second) / float64(rate)))
		}
	}
	// apad pads with silence to the whole count, and atrim cuts the
	// filter's rounding back to it, so the stream ends exactly there.
	n := strconv.FormatInt(samples, 10)
	stages := []string{"apad=whole_len=" + n, "atrim=end_sample=" + n}
	if chain != "" {
		stages = append([]string{chain}, stages...)
	}
	return plan{ratio: ratio, padded: padded, filter: strings.Join(stages, ",")}, nil
}

// atempoMin and atempoMax bound one atempo instance in a chain. The filter
// accepts more, but it drops or doubles whole fragments beyond a factor of
// two, so a chain keeps each stage inside this range.
const (
	atempoMin = 0.5
	atempoMax = 2.0
)

// AtempoChain returns the atempo filter chain that changes tempo by ratio
// without changing pitch, such as "atempo=2,atempo=1.5" for a ratio of 3.
// Every stage but the last is exactly 2 or exactly 0.5, and the last holds
// the remainder, so each stage stays within 0.5 to 2 and the stages
// multiply to ratio. A ratio of exactly 1 returns the empty string, since
// no stage is needed. A ratio that is not positive and finite reports
// ErrInvalid.
func AtempoChain(ratio float64) (string, error) {
	if !(ratio > 0) || math.IsInf(ratio, 0) {
		return "", fmt.Errorf("%w: tempo ratio %v must be positive and finite", ErrInvalid, ratio)
	}
	if ratio == 1 {
		return "", nil
	}
	var stages []string
	for ratio > atempoMax {
		stages = append(stages, "atempo=2")
		ratio /= atempoMax
	}
	for ratio < atempoMin {
		stages = append(stages, "atempo=0.5")
		ratio /= atempoMin
	}
	stages = append(stages, "atempo="+strconv.FormatFloat(ratio, 'g', -1, 64))
	return strings.Join(stages, ","), nil
}
