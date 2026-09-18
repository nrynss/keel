package edl

import (
	"errors"
	"testing"
)

// statsOutput is real loudnorm output, captured from ffmpeg 9.0.1 on a cut
// of the sine fixture with a crossfade. The parser must read exactly this
// shape and hand the numbers on verbatim.
const statsOutput = `{
	"input_i" : "-21.75",
	"input_tp" : "-18.06",
	"input_lra" : "0.00",
	"input_thresh" : "-31.75",
	"output_i" : "-24.05",
	"output_tp" : "-20.31",
	"output_lra" : "0.00",
	"output_thresh" : "-34.05",
	"normalization_type" : "linear",
	"target_offset" : "0.05"
}`

// TestParseLoudnormReadsMeasuredNumbers checks that the parser picks the
// four measured numbers out of the real stats shape and leaves them
// verbatim.
func TestParseLoudnormReadsMeasuredNumbers(t *testing.T) {
	got, err := parseLoudnorm([]byte(statsOutput))
	if err != nil {
		t.Fatalf("parseLoudnorm: %v", err)
	}
	want := loudnormMeasurement{InputI: "-21.75", InputLRA: "0.00", InputTP: "-18.06", InputThresh: "-31.75"}
	if got != want {
		t.Fatalf("parseLoudnorm = %+v, want %+v", got, want)
	}
}

// TestParseLoudnormRejectsUnusableStats checks that unreadable, unparseable,
// incomplete or non-finite stats report ErrNoMeasurement, because the apply
// pass must never run on guessed numbers.
func TestParseLoudnormRejectsUnusableStats(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"not json", "no stats here"},
		{"wrong json", `{"input_i": 5}`},
		{"missing number", `{"input_i" : "-21.75", "input_lra" : "0.00", "input_tp" : "-18.06"}`},
		{"silence", `{"input_i" : "-inf", "input_tp" : "-inf", "input_lra" : "0.00", "input_thresh" : "-70.00"}`},
		{"infinite peak", `{"input_i" : "-21.75", "input_tp" : "inf", "input_lra" : "0.00", "input_thresh" : "-31.75"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseLoudnorm([]byte(tt.raw)); !errors.Is(err, ErrNoMeasurement) {
				t.Fatalf("parseLoudnorm error = %v, want ErrNoMeasurement", err)
			}
		})
	}
}

// TestLoudnormApplyOptions pins the options the apply pass hands to
// loudnorm: the target, the measured numbers verbatim, and linear
// normalisation requested.
func TestLoudnormApplyOptions(t *testing.T) {
	measured := loudnormMeasurement{InputI: "-21.75", InputLRA: "0.00", InputTP: "-18.06", InputThresh: "-31.75"}
	got := loudnormApply(measured, Config{Target: -16})
	want := "I=-16:LRA=7:TP=-2:measured_I=-21.75:measured_LRA=0.00:measured_TP=-18.06:measured_thresh=-31.75:linear=true"
	if got != want {
		t.Fatalf("loudnormApply = %s, want %s", got, want)
	}
	if def := loudnormApply(measured, Config{}); def[:5] != "I=-24" {
		t.Fatalf("zero target = %s, want the loudnorm default -24", def)
	}
}
