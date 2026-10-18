package fit_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/nrynss/keel/ffmpeg"
	"github.com/nrynss/keel/fit"
)

// requireFFmpeg skips a test when ffmpeg and ffprobe are absent. CI sets
// KEEL_REQUIRE_FFMPEG so a missing binary fails there instead of skipping.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(name); err != nil {
			if os.Getenv("KEEL_REQUIRE_FFMPEG") != "" {
				t.Fatalf("%s is required when KEEL_REQUIRE_FFMPEG is set: %v", name, err)
			}
			t.Skipf("%s is not installed: %v", name, err)
		}
	}
}

// take generates a tone of length d at rate hertz with the given channel
// count, as a 16-bit WAV.
func take(t *testing.T, d time.Duration, rate, channels int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "take.wav")
	err := ffmpeg.Run(t.Context(), ffmpeg.Tools{}, "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate="+strconv.Itoa(rate)+":duration="+strconv.FormatFloat(d.Seconds(), 'f', -1, 64),
		"-ac", strconv.Itoa(channels), "-c:a", "pcm_s16le", path)
	if err != nil {
		t.Fatalf("generate take: %v", err)
	}
	return path
}

// pcm reads a 16-bit PCM WAV and returns its samples, interleaved, with
// its rate and channel count.
func pcm(t *testing.T, path string) ([]int16, int, int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 12 || string(raw[:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		t.Fatalf("%s is not a WAV", path)
	}
	var rate, channels int
	for off := 12; off+8 <= len(raw); {
		id := string(raw[off : off+4])
		size := int(binary.LittleEndian.Uint32(raw[off+4 : off+8]))
		body := raw[off+8 : min(off+8+size, len(raw))]
		switch id {
		case "fmt ":
			channels = int(binary.LittleEndian.Uint16(body[2:4]))
			rate = int(binary.LittleEndian.Uint32(body[4:8]))
		case "data":
			samples := make([]int16, len(body)/2)
			if err := binary.Read(bytes.NewReader(body[:len(samples)*2]), binary.LittleEndian, samples); err != nil {
				t.Fatal(err)
			}
			return samples, rate, channels
		}
		off += 8 + size + size%2
	}
	t.Fatalf("%s has no data chunk", path)
	return nil, 0, 0
}

// peak returns the largest absolute sample in s.
func peak(s []int16) int {
	p := 0
	for _, v := range s {
		p = max(p, int(math.Abs(float64(v))))
	}
	return p
}

// TestFitWritesExactlyTheSlot fits takes faster, slower, unchanged and
// padded, and checks the ratio, the exact sample count, the measured
// duration and, for a padded take, the silent tail.
func TestFitWritesExactlyTheSlot(t *testing.T) {
	requireFFmpeg(t)
	tests := []struct {
		name     string
		length   time.Duration
		rate     int
		channels int
		slot     time.Duration
		bounds   fit.Bounds
		ratio    float64
		padded   time.Duration
	}{
		{name: "speed up", length: 2200 * time.Millisecond, rate: 48000, channels: 1, slot: 2 * time.Second, ratio: 1.1},
		{name: "slow down", length: 1800 * time.Millisecond, rate: 48000, channels: 1, slot: 2 * time.Second, ratio: 0.9},
		{name: "stereo at 44.1k", length: 1500 * time.Millisecond, rate: 44100, channels: 2, slot: 1337 * time.Millisecond, ratio: 1500.0 / 1337},
		{name: "already fits", length: 2 * time.Second, rate: 48000, channels: 1, slot: 2 * time.Second, ratio: 1},
		{name: "slowed then padded", length: 2 * time.Second, rate: 48000, channels: 1, slot: 3 * time.Second, ratio: 0.8, padded: 500 * time.Millisecond},
		{name: "only padded", length: time.Second, rate: 16000, channels: 1, slot: 1500 * time.Millisecond, bounds: fit.Bounds{Slowest: 1}, ratio: 1, padded: 500 * time.Millisecond},
		{name: "wide bounds chain", length: 3 * time.Second, rate: 48000, channels: 1, slot: time.Second, bounds: fit.Bounds{Fastest: 4}, ratio: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := take(t, tt.length, tt.rate, tt.channels)
			out := filepath.Join(t.TempDir(), "fitted.wav")
			res, err := fit.Fit(t.Context(), ffmpeg.Tools{}, in, out, tt.slot, tt.bounds)
			if err != nil {
				t.Fatalf("Fit: %v", err)
			}
			if math.Abs(res.Ratio-tt.ratio) > 1e-9 || res.Padded != tt.padded {
				t.Errorf("ratio %v padded %v, want %v and %v", res.Ratio, res.Padded, tt.ratio, tt.padded)
			}
			samples, rate, channels := pcm(t, out)
			if rate != tt.rate || channels != tt.channels {
				t.Errorf("wrote %d Hz %d channels, want %d Hz %d channels", rate, channels, tt.rate, tt.channels)
			}
			want := int(math.Round(tt.slot.Seconds() * float64(tt.rate)))
			if got := len(samples) / channels; got != want {
				t.Errorf("wrote %d samples per channel, want exactly %d", got, want)
			}
			if res.Duration != tt.slot.Round(time.Millisecond) {
				t.Errorf("Duration = %v, want %v", res.Duration, tt.slot)
			}
			// The fitted speech must reach the end of its stretch, and a
			// padded tail must be digital silence.
			speech := int(math.Round((tt.slot-tt.padded).Seconds()*float64(tt.rate))) * channels
			margin := tt.rate / 100 * channels // 10 ms either side of the join
			if peak(samples[speech-2*margin:speech-margin]) == 0 {
				t.Errorf("the take is silent just before %v", tt.slot-tt.padded)
			}
			// The tempo change keeps pitch: the 440 Hz tone still crosses
			// zero about 880 times a second inside the stretched take.
			window := samples[:min(speech, tt.rate*channels)]
			crossings := 0
			for i := channels; i < len(window); i += channels {
				if (window[i-channels] < 0) != (window[i] < 0) {
					crossings++
				}
			}
			perSecond := float64(crossings) * float64(tt.rate*channels) / float64(len(window))
			if math.Abs(perSecond-880) > 880*0.02 {
				t.Errorf("tone crosses zero %.0f times a second, want about 880: the pitch moved", perSecond)
			}
			if tt.padded > 0 {
				if p := peak(samples[speech+margin:]); p != 0 {
					t.Errorf("padded tail peaks at %d, want silence", p)
				}
			}
		})
	}
}

// TestFitIsDeterministic fits the same take twice and compares the bytes.
func TestFitIsDeterministic(t *testing.T) {
	requireFFmpeg(t)
	in := take(t, 2200*time.Millisecond, 48000, 1)
	dir := t.TempDir()
	var outputs [2][]byte
	for i := range outputs {
		out := filepath.Join(dir, "fitted-"+strconv.Itoa(i)+".wav")
		if _, err := fit.Fit(t.Context(), ffmpeg.Tools{}, in, out, 2*time.Second, fit.Bounds{}); err != nil {
			t.Fatalf("Fit: %v", err)
		}
		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		outputs[i] = raw
	}
	if !bytes.Equal(outputs[0], outputs[1]) {
		t.Error("two fits of one take wrote different bytes")
	}
}

// TestFitRefusesWhatItCannotFit checks the typed refusal of a take that is
// too long, which writes nothing, and the refusals of bad arguments, a
// missing binary and a cancelled context.
func TestFitRefusesWhatItCannotFit(t *testing.T) {
	requireFFmpeg(t)
	in := take(t, 3*time.Second, 48000, 1)
	dir := t.TempDir()
	out := filepath.Join(dir, "fitted.wav")

	_, err := fit.Fit(t.Context(), ffmpeg.Tools{}, in, out, 2*time.Second, fit.Bounds{})
	var re *fit.RatioError
	if !errors.As(err, &re) || !errors.Is(err, fit.ErrOutOfBounds) {
		t.Fatalf("Fit error = %v, want a *RatioError", err)
	}
	if re.Ratio != 1.5 || re.Bounds.Fastest != fit.DefaultFastest {
		t.Errorf("RatioError = %+v, want ratio 1.5 above %v", re, fit.DefaultFastest)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused fit left %s: %v", out, err)
	}

	invalid := []struct {
		name    string
		in, out string
		slot    time.Duration
		bounds  fit.Bounds
	}{
		{name: "no source", out: out, slot: time.Second},
		{name: "no output", in: in, slot: time.Second},
		{name: "output is source", in: in, out: in, slot: time.Second},
		{name: "zero slot", in: in, out: out},
		{name: "negative slot", in: in, out: out, slot: -time.Second},
		{name: "bounds above 1", in: in, out: out, slot: time.Second, bounds: fit.Bounds{Slowest: 1.1}},
		{name: "bounds below 1", in: in, out: out, slot: time.Second, bounds: fit.Bounds{Fastest: 0.9}},
	}
	for _, tt := range invalid {
		if _, err := fit.Fit(t.Context(), ffmpeg.Tools{}, tt.in, tt.out, tt.slot, tt.bounds); !errors.Is(err, fit.ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", tt.name, err)
		}
	}

	missing := filepath.Join(dir, "no-such-binary")
	if _, err := fit.Fit(t.Context(), ffmpeg.Tools{FFprobe: missing}, in, out, 3*time.Second, fit.Bounds{}); !errors.Is(err, ffmpeg.ErrNotFound) {
		t.Errorf("missing ffprobe error = %v, want ffmpeg.ErrNotFound", err)
	}
	if _, err := fit.Fit(t.Context(), ffmpeg.Tools{FFmpeg: missing}, in, out, 3*time.Second, fit.Bounds{}); !errors.Is(err, ffmpeg.ErrNotFound) {
		t.Errorf("missing ffmpeg error = %v, want ffmpeg.ErrNotFound", err)
	}
	if _, err := fit.Fit(t.Context(), ffmpeg.Tools{}, filepath.Join(dir, "absent.wav"), out, time.Second, fit.Bounds{}); !errors.Is(err, ffmpeg.ErrFailed) {
		t.Errorf("absent source error = %v, want ffmpeg.ErrFailed", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := fit.Fit(ctx, ffmpeg.Tools{}, in, out, 3*time.Second, fit.Bounds{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled fit error = %v, want context.Canceled", err)
	}
}
