package edl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nrynss/keel/ffmpeg"
)

// loudnormMeasurement holds the four measured numbers the measure pass
// reports. The strings stay verbatim, because the apply pass hands the
// numbers back to the tool in the words it used.
type loudnormMeasurement struct {
	InputI      string `json:"input_i"`
	InputLRA    string `json:"input_lra"`
	InputTP     string `json:"input_tp"`
	InputThresh string `json:"input_thresh"`
}

// measureLoudness runs the cut through loudnorm once and returns the
// measured numbers. The tool writes its stats to a file in a temporary
// directory, which the read below parses. The pass writes nothing else, so
// the output goes to null.
func measureLoudness(ctx context.Context, tools ffmpeg.Tools, src, cut string) (loudnormMeasurement, error) {
	dir, err := os.MkdirTemp("", "keel-edl-")
	if err != nil {
		return loudnormMeasurement{}, fmt.Errorf("stats dir: %w", err)
	}
	defer os.RemoveAll(dir)
	stats := filepath.Join(dir, "loudnorm.json")

	graph := fmt.Sprintf("%s;[cut]loudnorm=print_format=json:stats_file=%s[stats]", cut, stats)
	args := []string{"-i", src, "-filter_complex", graph, "-map", "[stats]", "-f", "null", "-"}
	if err := ffmpeg.Run(ctx, tools, args...); err != nil {
		return loudnormMeasurement{}, err
	}

	raw, err := os.ReadFile(stats)
	if err != nil {
		return loudnormMeasurement{}, fmt.Errorf("%w: %v", ErrNoMeasurement, err)
	}
	return parseLoudnorm(raw)
}

// parseLoudnorm reads the measured numbers out of the stats file the tool
// wrote. Anything unreadable, unparseable or incomplete reports
// ErrNoMeasurement, because a guess would defeat the two-pass design.
func parseLoudnorm(raw []byte) (loudnormMeasurement, error) {
	var m loudnormMeasurement
	if err := json.Unmarshal(raw, &m); err != nil {
		return loudnormMeasurement{}, fmt.Errorf("%w: %v", ErrNoMeasurement, err)
	}
	if m.InputI == "" || m.InputLRA == "" || m.InputTP == "" || m.InputThresh == "" {
		return loudnormMeasurement{}, fmt.Errorf("%w: a measured number is absent", ErrNoMeasurement)
	}
	return m, nil
}

// applyLoudness runs the cut a second time with the measured numbers and
// writes dst. The aformat stage returns the audio to the sample rate and
// channel layout of the source, because loudnorm works at 192 kHz inside.
func applyLoudness(ctx context.Context, tools ffmpeg.Tools, src, cut string, m loudnormMeasurement, format ffmpeg.Format, cfg Config, dst string) error {
	restore := fmt.Sprintf("aformat=sample_rates=%d", format.SampleRate)
	if format.ChannelLayout != "" {
		restore += ":channel_layouts=" + format.ChannelLayout
	}
	graph := fmt.Sprintf("%s;[cut]loudnorm=%s,%s[out]", cut, loudnormApply(m, cfg), restore)
	args := []string{"-y", "-i", src, "-filter_complex", graph, "-map", "[out]", dst}
	return ffmpeg.Run(ctx, tools, args...)
}

// loudnormApply renders the loudnorm options for the apply pass: the
// loudness target and the measured numbers, with linear normalisation
// requested. The loudness range and true peak targets stay pinned at the
// loudnorm defaults, so a change in the tool cannot silently move them.
func loudnormApply(m loudnormMeasurement, cfg Config) string {
	target := "-24"
	if cfg.Target != 0 {
		target = fmt.Sprintf("%g", cfg.Target)
	}
	return fmt.Sprintf("I=%s:LRA=7:TP=-2:measured_I=%s:measured_LRA=%s:measured_TP=%s:measured_thresh=%s:linear=true",
		target, m.InputI, m.InputLRA, m.InputTP, m.InputThresh)
}
