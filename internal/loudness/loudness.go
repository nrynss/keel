// Package loudness is the two-pass loudnorm normalisation that edl and mix
// share. The first pass runs a filter graph through loudnorm and reads the
// numbers the tool writes to its own stats file. The second pass hands
// those numbers back verbatim with linear normalisation requested. The
// numbers are never guessed: anything unreadable or not finite is refused.
//
// The options are pinned, so a change in the tool's defaults cannot move a
// render: the loudness range target is 7 LU and the true peak target is
// -2 dBTP, the loudnorm defaults on the pinned ffmpeg 9.0.1.
package loudness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"github.com/nrynss/keel/ffmpeg"
)

// DefaultTarget is the integrated loudness a zero target means, in LUFS,
// the loudnorm default.
const DefaultTarget = -24

// MinTarget is the quietest integrated loudness loudnorm accepts, in LUFS.
const MinTarget = -70

// MaxTarget is the loudest integrated loudness loudnorm accepts, in LUFS.
const MaxTarget = -5

// ValidTarget reports whether target is zero, meaning DefaultTarget, or
// inside the range loudnorm accepts.
func ValidTarget(target float64) bool {
	return target == 0 || (target >= MinTarget && target <= MaxTarget)
}

// errMissing stands in for a caller that passes no sentinel of its own.
var errMissing = errors.New("loudness: measurement missing")

// Measurement holds the four measured numbers the measure pass reports.
// The strings stay verbatim, because the apply pass hands the numbers back
// to the tool in the words it used.
type Measurement struct {
	InputI      string `json:"input_i"`
	InputLRA    string `json:"input_lra"`
	InputTP     string `json:"input_tp"`
	InputThresh string `json:"input_thresh"`
}

// Measure runs graph, which must end in the output label named label, once
// through loudnorm and returns the measured numbers. inputs come before the
// graph, as ffmpeg arguments such as "-i" and a path. The tool writes its
// stats to a file in a temporary directory that Measure reads and removes.
// The pass writes nothing else, so its output goes to null. Stats that are
// missing or unusable report missing, the caller's own sentinel, wrapped,
// as Parse does.
func Measure(ctx context.Context, tools ffmpeg.Tools, inputs []string, graph, label string, missing error) (Measurement, error) {
	dir, err := os.MkdirTemp("", "keel-loudness-")
	if err != nil {
		return Measurement{}, fmt.Errorf("stats dir: %w", err)
	}
	defer os.RemoveAll(dir)
	stats := filepath.Join(dir, "loudnorm.json")

	full := fmt.Sprintf("%s;[%s]loudnorm=print_format=json:stats_file=%s[stats]", graph, label, stats)
	args := append(append([]string(nil), inputs...), "-filter_complex", full, "-map", "[stats]", "-f", "null", "-")
	if err := ffmpeg.Run(ctx, tools, args...); err != nil {
		return Measurement{}, err
	}
	raw, err := os.ReadFile(stats)
	if err != nil {
		return Measurement{}, fmt.Errorf("%w: %v", missing, err)
	}
	return Parse(raw, missing)
}

// Parse reads the measured numbers out of the stats file the tool wrote.
// Anything unreadable, unparseable, incomplete or not a finite number
// reports missing wrapped, because a guess would defeat the two-pass
// design. Silence measures as -inf, which the apply pass cannot hand back,
// so that path reports missing too. A nil missing reports a sentinel of
// this package's own.
func Parse(raw []byte, missing error) (Measurement, error) {
	if missing == nil {
		missing = errMissing
	}
	var m Measurement
	if err := json.Unmarshal(raw, &m); err != nil {
		return Measurement{}, fmt.Errorf("%w: %v", missing, err)
	}
	if m.InputI == "" || m.InputLRA == "" || m.InputTP == "" || m.InputThresh == "" {
		return Measurement{}, fmt.Errorf("%w: a measured number is absent", missing)
	}
	if !finite(m.InputI) || !finite(m.InputLRA) || !finite(m.InputTP) || !finite(m.InputThresh) {
		return Measurement{}, fmt.Errorf("%w: a measured number is not finite", missing)
	}
	return m, nil
}

// finite reports whether s parses as a finite number.
func finite(s string) bool {
	v, err := strconv.ParseFloat(s, 64)
	return err == nil && !math.IsInf(v, 0) && !math.IsNaN(v)
}

// Options renders the loudnorm options for the apply pass: the target, the
// measured numbers verbatim, and linear normalisation requested. A zero
// target means DefaultTarget.
func Options(m Measurement, target float64) string {
	t := strconv.Itoa(DefaultTarget)
	if target != 0 {
		t = fmt.Sprintf("%g", target)
	}
	return fmt.Sprintf("I=%s:LRA=7:TP=-2:measured_I=%s:measured_LRA=%s:measured_TP=%s:measured_thresh=%s:linear=true",
		t, m.InputI, m.InputLRA, m.InputTP, m.InputThresh)
}

// Restore returns the aformat stage that brings loudnorm's output back to
// rate hertz and the named channel layout, because loudnorm works at
// 192 kHz inside. An empty layout leaves the channels as they are.
func Restore(rate int, layout string) string {
	s := "aformat=sample_rates=" + strconv.Itoa(rate)
	if layout != "" {
		s += ":channel_layouts=" + layout
	}
	return s
}
