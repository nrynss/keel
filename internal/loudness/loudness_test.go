package loudness

import (
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nrynss/keel/ffmpeg"
)

// errTest stands in for a caller's sentinel.
var errTest = errors.New("test: measurement missing")

// TestParseReadsTheStatsFile pins the parser against the real stats shape
// and the refusals that keep a guess out of the apply pass, reported as
// the caller's sentinel.
func TestParseReadsTheStatsFile(t *testing.T) {
	stats := `{"input_i" : "-21.75", "input_tp" : "-18.06", "input_lra" : "0.00", "input_thresh" : "-31.75", "target_offset" : "0.05"}`
	got, err := Parse([]byte(stats), errTest)
	want := Measurement{InputI: "-21.75", InputLRA: "0.00", InputTP: "-18.06", InputThresh: "-31.75"}
	if err != nil || got != want {
		t.Fatalf("Parse = %+v, %v, want %+v", got, err, want)
	}
	for _, raw := range []string{
		"", "nope", `{"input_i": 5}`,
		`{"input_i" : "-21.75", "input_lra" : "0.00", "input_tp" : "-18.06"}`,
		`{"input_i" : "-inf", "input_tp" : "-inf", "input_lra" : "0.00", "input_thresh" : "-70.00"}`,
		`{"input_i" : "-21.75", "input_tp" : "nan", "input_lra" : "0.00", "input_thresh" : "-31.75"}`,
	} {
		if _, err := Parse([]byte(raw), errTest); !errors.Is(err, errTest) {
			t.Errorf("Parse(%q) error = %v, want the caller's sentinel", raw, err)
		}
	}
	if _, err := Parse(nil, nil); !errors.Is(err, errMissing) {
		t.Errorf("Parse with no sentinel error = %v, want the package's own", err)
	}
}

// TestOptionsAndRestorePinTheApplyPass pins the apply pass's options and
// its format restore.
func TestOptionsAndRestorePinTheApplyPass(t *testing.T) {
	m := Measurement{InputI: "-21.75", InputLRA: "0.00", InputTP: "-18.06", InputThresh: "-31.75"}
	if got, want := Options(m, -16), "I=-16:LRA=7:TP=-2:measured_I=-21.75:measured_LRA=0.00:measured_TP=-18.06:measured_thresh=-31.75:linear=true"; got != want {
		t.Errorf("Options = %s, want %s", got, want)
	}
	if got := Options(m, 0); !strings.HasPrefix(got, "I=-24:") {
		t.Errorf("Options with a zero target = %s, want I=-24", got)
	}
	if got := Options(m, -23.5); !strings.HasPrefix(got, "I=-23.5:") {
		t.Errorf("Options(-23.5) = %s", got)
	}
	if got := Restore(48000, "stereo"); got != "aformat=sample_rates=48000:channel_layouts=stereo" {
		t.Errorf("Restore = %s", got)
	}
	if got := Restore(44100, ""); got != "aformat=sample_rates=44100" {
		t.Errorf("Restore without a layout = %s", got)
	}
}

// TestValidTarget pins the accepted range.
func TestValidTarget(t *testing.T) {
	for _, v := range []float64{0, -70, -5, -24, -16.5} {
		if !ValidTarget(v) {
			t.Errorf("ValidTarget(%v) = false", v)
		}
	}
	for _, v := range []float64{-70.01, -4.99, 3, math.NaN(), math.Inf(-1)} {
		if ValidTarget(v) {
			t.Errorf("ValidTarget(%v) = true", v)
		}
	}
}

// TestMeasureReadsTheToolsOwnStats runs the measure pass on a generated
// tone and checks the numbers are finite, then checks a silent graph
// reports the caller's sentinel and a missing binary maps through.
func TestMeasureReadsTheToolsOwnStats(t *testing.T) {
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(name); err != nil {
			if os.Getenv("KEEL_REQUIRE_FFMPEG") != "" {
				t.Fatalf("%s is required when KEEL_REQUIRE_FFMPEG is set: %v", name, err)
			}
			t.Skipf("%s is not installed: %v", name, err)
		}
	}
	inputs := []string{"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=1"}
	m, err := Measure(t.Context(), ffmpeg.Tools{}, inputs, "[0:a]anull[x]", "x", errTest)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if !finite(m.InputI) || !finite(m.InputTP) {
		t.Errorf("Measure = %+v, want finite numbers", m)
	}
	silent := []string{"-f", "lavfi", "-i", "anullsrc=r=48000:cl=mono", "-t", "1"}
	if _, err := Measure(t.Context(), ffmpeg.Tools{}, silent, "[0:a]anull[x]", "x", errTest); !errors.Is(err, errTest) {
		t.Errorf("silent Measure error = %v, want the caller's sentinel", err)
	}
	missing := ffmpeg.Tools{FFmpeg: filepath.Join(t.TempDir(), "no-such-binary")}
	if _, err := Measure(t.Context(), missing, inputs, "[0:a]anull[x]", "x", errTest); !errors.Is(err, ffmpeg.ErrNotFound) {
		t.Errorf("missing binary error = %v, want ffmpeg.ErrNotFound", err)
	}
}
