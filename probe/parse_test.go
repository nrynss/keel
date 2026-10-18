package probe

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
)

// TestParseScenesReadsTheFiltersLines pins the parser against the shape
// the metadata filter prints on the pinned ffmpeg, a garbled header, and
// repeated or backward times.
func TestParseScenesReadsTheFiltersLines(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []time.Duration
		wantErr bool
	}{
		{name: "nothing selected", raw: "", want: []time.Duration{}},
		{
			name: "two cuts",
			raw: "frame:0    pts:1000    pts_time:1\nlavfi.scene_score=0.530191\n" +
				"frame:1    pts:2000    pts_time:2\nlavfi.scene_score=0.892203\n",
			want: []time.Duration{time.Second, 2 * time.Second},
		},
		{
			name: "fractional times round to the millisecond",
			raw:  "frame:0 pts:12 pts_time:0.4804\nframe:1 pts:31 pts_time:1.2396\n",
			want: []time.Duration{480 * time.Millisecond, 1240 * time.Millisecond},
		},
		{
			name: "a repeat or a step back is dropped",
			raw:  "frame:0 pts:1 pts_time:1\nframe:1 pts:1 pts_time:1\nframe:2 pts:0 pts_time:0.5\n",
			want: []time.Duration{time.Second},
		},
		{name: "a header without a time", raw: "frame:0    pts:1000\n", wantErr: true},
		{name: "a header with a garbled time", raw: "frame:0 pts:1 pts_time:soon\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseScenes([]byte(tt.raw))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseScenes error = %v, want error %v", err, tt.wantErr)
			}
			if !tt.wantErr && (!slices.Equal(got, tt.want) || got == nil) {
				t.Errorf("parseScenes = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestFrameTimingTellsVariableFromConstant pins the gap rule: one tick of
// slack for a rate the time base cannot hold exactly, decode order sorted
// first, repeats counted once, and too few frames to judge.
func TestFrameTimingTellsVariableFromConstant(t *testing.T) {
	ms := Rate{Num: 1, Den: 1000}
	tests := []struct {
		name     string
		ts       []int64
		tb       Rate
		variable bool
		mean     Rate
	}{
		{name: "even gaps", ts: []int64{0, 40, 80, 120}, tb: ms},
		{name: "ntsc in milliseconds", ts: []int64{0, 33, 67, 100, 133, 167}, tb: ms},
		{name: "decode order", ts: []int64{0, 80, 40, 160, 120}, tb: ms},
		{name: "repeats count once", ts: []int64{0, 40, 40, 80}, tb: ms},
		{name: "two frames cannot tell", ts: []int64{0, 500}, tb: ms},
		{name: "no time base", ts: []int64{0, 10, 50}, tb: Rate{}},
		{name: "mixed gaps", ts: []int64{0, 50, 100, 200, 300}, tb: ms, variable: true, mean: Rate{Num: 40, Den: 3}},
		{name: "mean reduces exactly", ts: []int64{0, 512, 1536}, tb: Rate{Num: 1, Den: 15360}, variable: true, mean: Rate{Num: 20, Den: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := slices.Clone(tt.ts)
			mean, variable := frameTiming(tt.ts, tt.tb)
			if variable != tt.variable || mean != tt.mean {
				t.Errorf("frameTiming = %v, %v, want %v, %v", mean, variable, tt.mean, tt.variable)
			}
			if !slices.Equal(in, tt.ts) {
				t.Errorf("frameTiming changed its input to %v", tt.ts)
			}
		})
	}
}

// TestRotationReadsClockwiseDegrees pins the sign flip of the display
// matrix, the older clockwise tag, and the normalisation into 0 to 359.
func TestRotationReadsClockwiseDegrees(t *testing.T) {
	tests := []struct {
		name string
		json string
		want int
	}{
		{name: "none", json: `{}`, want: 0},
		{name: "matrix counter-clockwise 90", json: `{"side_data_list":[{"side_data_type":"Display Matrix","rotation":90}]}`, want: 270},
		{name: "matrix -90", json: `{"side_data_list":[{"side_data_type":"Display Matrix","rotation":-90}]}`, want: 90},
		{name: "matrix -180", json: `{"side_data_list":[{"side_data_type":"Display Matrix","rotation":-180}]}`, want: 180},
		{name: "matrix fractional", json: `{"side_data_list":[{"side_data_type":"Display Matrix","rotation":-44.6}]}`, want: 45},
		{name: "matrix full turn", json: `{"side_data_list":[{"side_data_type":"Display Matrix","rotation":360}]}`, want: 0},
		{name: "other side data", json: `{"side_data_list":[{"side_data_type":"Stereo 3D"}]}`, want: 0},
		{name: "matrix without rotation", json: `{"side_data_list":[{"side_data_type":"Display Matrix"}]}`, want: 0},
		{name: "rotate tag", json: `{"tags":{"rotate":"90"}}`, want: 90},
		{name: "rotate tag -90", json: `{"tags":{"rotate":"-90"}}`, want: 270},
		{name: "matrix wins over tag", json: `{"side_data_list":[{"side_data_type":"Display Matrix","rotation":-90}],"tags":{"rotate":"180"}}`, want: 90},
		{name: "garbled tag", json: `{"tags":{"rotate":"sideways"}}`, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ps probeStream
			if err := json.Unmarshal([]byte(tt.json), &ps); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := rotation(ps); got != tt.want {
				t.Errorf("rotation = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestParseRateReadsRationals pins the rational parser.
func TestParseRateReadsRationals(t *testing.T) {
	tests := map[string]Rate{
		"30000/1001": {Num: 30000, Den: 1001},
		"25/1":       {Num: 25, Den: 1},
		"0/0":        {},
		"25":         {},
		"x/1":        {},
		"25/0":       {},
		"-1/2":       {},
		"":           {},
	}
	for in, want := range tests {
		if got := parseRate(in); got != want {
			t.Errorf("parseRate(%q) = %v, want %v", in, got, want)
		}
	}
	if got := (Rate{Num: 30000, Den: 1001}).String(); got != "30000/1001" {
		t.Errorf("String = %q", got)
	}
	if got := (Rate{}).Float64(); got != 0 {
		t.Errorf("zero Rate Float64 = %v, want 0", got)
	}
}

// TestParseReportFollowsTheDurationRule pins the order the duration is
// taken in, the audio fallbacks it shares with ffmpeg.AudioFormat, and the
// refusal of a report with no stream.
func TestParseReportFollowsTheDurationRule(t *testing.T) {
	decode := func(t *testing.T, raw string) probeReport {
		t.Helper()
		var r probeReport
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return r
	}
	tests := []struct {
		name     string
		raw      string
		duration time.Duration
	}{
		{
			name:     "container wins",
			raw:      `{"format":{"duration":"3.0005"},"streams":[{"index":0,"codec_type":"audio","sample_rate":"48000","duration":"2.0"}]}`,
			duration: 3001 * time.Millisecond,
		},
		{
			name:     "audio stands in",
			raw:      `{"format":{"duration":"N/A"},"streams":[{"index":0,"codec_type":"video","duration":"5.0"},{"index":1,"codec_type":"audio","sample_rate":"48000","duration":"2.5"}]}`,
			duration: 2500 * time.Millisecond,
		},
		{
			name:     "video is the last resort",
			raw:      `{"format":{},"streams":[{"index":0,"codec_type":"video","duration":"4.25"}]}`,
			duration: 4250 * time.Millisecond,
		},
		{
			name:     "nothing knows",
			raw:      `{"format":{},"streams":[{"index":0,"codec_type":"data"}]}`,
			duration: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			media, _, err := parseReport(decode(t, tt.raw))
			if err != nil {
				t.Fatalf("parseReport: %v", err)
			}
			if media.Duration != tt.duration {
				t.Errorf("Duration = %v, want %v", media.Duration, tt.duration)
			}
		})
	}
	t.Run("audio fallbacks", func(t *testing.T) {
		media, _, err := parseReport(decode(t,
			`{"format":{"bit_rate":"64000"},"streams":[{"index":0,"codec_type":"audio","sample_rate":"16000","channels":2,"channel_layout":"unknown"}]}`))
		if err != nil {
			t.Fatalf("parseReport: %v", err)
		}
		a, _ := media.Audio()
		if a.ChannelLayout != "stereo" || a.BitRate != 64000 {
			t.Errorf("audio = %+v, want stereo at the container's 64000", a)
		}
		media, _, err = parseReport(decode(t,
			`{"format":{},"streams":[{"index":0,"codec_type":"audio","sample_rate":"16000","channels":1,"bits_per_sample":16}]}`))
		if err != nil {
			t.Fatalf("parseReport: %v", err)
		}
		if a, _ := media.Audio(); a.ChannelLayout != "mono" || a.BitRate != 256000 {
			t.Errorf("audio = %+v, want mono at 256000 from the sample layout", a)
		}
	})
	t.Run("bad sample rate", func(t *testing.T) {
		if _, _, err := parseReport(decode(t, `{"streams":[{"index":0,"codec_type":"audio","sample_rate":"fast"}]}`)); err == nil {
			t.Error("parseReport accepted an unreadable sample rate")
		}
	})
	t.Run("no stream", func(t *testing.T) {
		if _, _, err := parseReport(decode(t, `{"streams":[]}`)); !errors.Is(err, ErrNoStream) {
			t.Errorf("parseReport error = %v, want ErrNoStream", err)
		}
	})
	t.Run("only moving picture is scanned", func(t *testing.T) {
		_, timeBases, err := parseReport(decode(t, `{"streams":[
			{"index":0,"codec_type":"video","time_base":"1/12800","avg_frame_rate":"25/1"},
			{"index":1,"codec_type":"video","time_base":"1/90000","disposition":{"attached_pic":1}},
			{"index":2,"codec_type":"audio","sample_rate":"48000","time_base":"1/48000"}]}`))
		if err != nil {
			t.Fatalf("parseReport: %v", err)
		}
		if len(timeBases) != 1 || timeBases[0] != (Rate{Num: 1, Den: 12800}) {
			t.Errorf("time bases = %v, want only stream 0 at 1/12800", timeBases)
		}
	})
}

// TestPacketSinkSurvivesSplitWrites pins that lines split across writes,
// a last line without a newline, packets without a timestamp and a dts
// fallback all parse, and that a garbled line is reported once.
func TestPacketSinkSurvivesSplitWrites(t *testing.T) {
	s := &packetSink{timestamps: make(map[int][]int64)}
	for _, chunk := range []string{"0,0,0\n0,4", "0,N/A\n1,N/A,7\n", "0,N/A,N/A\n0,80"} {
		if n, err := s.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	s.flush()
	if s.err != nil {
		t.Fatalf("sink error = %v", s.err)
	}
	if got := s.timestamps[0]; !slices.Equal(got, []int64{0, 40, 80}) {
		t.Errorf("stream 0 = %v, want [0 40 80]", got)
	}
	if got := s.timestamps[1]; !slices.Equal(got, []int64{7}) {
		t.Errorf("stream 1 = %v, want the dts 7", got)
	}
	bad := &packetSink{timestamps: make(map[int][]int64)}
	_, _ = bad.Write([]byte("garbage\nx,1\n"))
	if bad.err == nil {
		t.Error("a garbled line was not reported")
	}
}

// TestSizeScaleFilter pins the filter each size asks for.
func TestSizeScaleFilter(t *testing.T) {
	tests := map[Size]string{
		{}:                        "",
		{Width: 320}:              "scale=320:-1",
		{Height: 240}:             "scale=-1:240",
		{Width: 320, Height: 240}: "scale=320:240:force_original_aspect_ratio=decrease",
	}
	for size, want := range tests {
		if got := size.scale(); got != want {
			t.Errorf("%+v.scale() = %q, want %q", size, got, want)
		}
	}
}
