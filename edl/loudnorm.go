package edl

import (
	"context"
	"fmt"

	"github.com/nrynss/keel/ffmpeg"
	"github.com/nrynss/keel/internal/loudness"
)

// loudnormMeasurement holds the four measured numbers the measure pass
// reports. The two-pass logic lives in internal/loudness, which mix shares.
type loudnormMeasurement = loudness.Measurement

// measureLoudness runs the cut through loudnorm once and returns the
// measured numbers. Unusable stats report ErrNoMeasurement.
func measureLoudness(ctx context.Context, tools ffmpeg.Tools, src, cut string) (loudnormMeasurement, error) {
	return loudness.Measure(ctx, tools, []string{"-i", src}, cut, "cut", ErrNoMeasurement)
}

// parseLoudnorm reads the measured numbers out of the stats file the tool
// wrote, reporting ErrNoMeasurement for anything a guess would stand in
// for.
func parseLoudnorm(raw []byte) (loudnormMeasurement, error) {
	return loudness.Parse(raw, ErrNoMeasurement)
}

// applyLoudness runs the cut a second time with the measured numbers and
// writes dst. The aformat stage returns the audio to the sample rate and
// channel layout of the source, because loudnorm works at 192 kHz inside.
func applyLoudness(ctx context.Context, tools ffmpeg.Tools, src, cut string, m loudnormMeasurement, format ffmpeg.Format, cfg Config, dst string) error {
	restore := loudness.Restore(format.SampleRate, format.ChannelLayout)
	graph := fmt.Sprintf("%s;[cut]loudnorm=%s,%s[out]", cut, loudnormApply(m, cfg), restore)
	args := []string{"-y", "-i", src, "-filter_complex", graph, "-map", "[out]", dst}
	return ffmpeg.Run(ctx, tools, args...)
}

// loudnormApply renders the loudnorm options for the apply pass: the
// loudness target and the measured numbers, with linear normalisation
// requested. The loudness range and true peak targets stay pinned at the
// loudnorm defaults, so a change in the tool cannot silently move them.
func loudnormApply(m loudnormMeasurement, cfg Config) string {
	return loudness.Options(m, cfg.Target)
}
