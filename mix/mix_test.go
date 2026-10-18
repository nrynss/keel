package mix_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nrynss/keel/ffmpeg"
	"github.com/nrynss/keel/mix"
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

// generate writes a lavfi source to a file in dir and returns its path.
func generate(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	full := append([]string{"-nostdin", "-v", "error", "-y"}, args...)
	if err := ffmpeg.Run(t.Context(), ffmpeg.Tools{}, append(full, path)...); err != nil {
		t.Fatalf("generate %s: %v", name, err)
	}
	return path
}

// tone writes a sine at freq hertz lasting d, 48 kHz mono.
func tone(t *testing.T, dir, name string, freq int, d time.Duration) string {
	return generate(t, dir, name, "-f", "lavfi",
		"-i", "sine=frequency="+strconv.Itoa(freq)+":sample_rate=48000:duration="+strconv.FormatFloat(d.Seconds(), 'f', -1, 64),
		"-c:a", "pcm_s16le")
}

// noise writes seeded pink noise lasting d at 44.1 kHz stereo, a bed in a
// rate and layout the mix must convert.
func noise(t *testing.T, dir, name string, d time.Duration) string {
	return generate(t, dir, name, "-f", "lavfi",
		"-i", "anoisesrc=color=pink:seed=7:sample_rate=44100:amplitude=0.5:duration="+strconv.FormatFloat(d.Seconds(), 'f', -1, 64),
		"-ac", "2", "-c:a", "pcm_s16le")
}

// pcm reads a 16-bit PCM WAV into samples, interleaved, with its rate and
// channel count.
func pcm(t *testing.T, path string) ([]int16, int, int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
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
			s := make([]int16, len(body)/2)
			if err := binary.Read(bytes.NewReader(body[:len(s)*2]), binary.LittleEndian, s); err != nil {
				t.Fatal(err)
			}
			return s, rate, channels
		}
		off += 8 + size + size%2
	}
	t.Fatalf("%s has no data chunk", path)
	return nil, 0, 0
}

// rms is the root mean square of the samples of one channel from start up
// to end seconds.
func rms(s []int16, rate, channels, channel int, start, end float64) float64 {
	var sum float64
	n := 0
	for i := int(start * float64(rate)); i < int(end*float64(rate)); i++ {
		v := float64(s[i*channels+channel])
		sum += v * v
		n++
	}
	return math.Sqrt(sum / float64(n))
}

// TestRenderPlacesEachClip renders two clips on two tracks and checks the
// exact length, the silence before an offset, the gain, the fades, a
// source shorter than its range padded with silence, and the conversion
// to the output format.
func TestRenderPlacesEachClip(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	long := tone(t, dir, "long.wav", 440, 3*time.Second)
	short := tone(t, dir, "short.wav", 440, 500*time.Millisecond)
	out := filepath.Join(dir, "mix.wav")
	tl := mix.Timeline{Tracks: []mix.Track{
		{Name: "loud", Clips: []mix.Clip{{Source: long, In: time.Second, Length: time.Second, Offset: 500 * time.Millisecond}}},
		{Name: "quiet", Clips: []mix.Clip{{Source: long, Length: time.Second, Offset: 2 * time.Second, Gain: -6.0206, FadeIn: 200 * time.Millisecond}}},
		{Name: "short", Clips: []mix.Clip{{Source: short, Length: time.Second, Offset: 3500 * time.Millisecond}}},
	}, Length: 5 * time.Second}
	if err := mix.Render(t.Context(), ffmpeg.Tools{}, mix.Config{SampleRate: 16000, Layout: "mono"}, tl, out); err != nil {
		t.Fatalf("Render: %v", err)
	}
	s, rate, channels := pcm(t, out)
	if rate != 16000 || channels != 1 {
		t.Fatalf("wrote %d Hz %d channels, want 16000 Hz mono", rate, channels)
	}
	if len(s) != 5*16000 {
		t.Fatalf("wrote %d samples, want exactly %d", len(s), 5*16000)
	}
	r := func(a, b float64) float64 { return rms(s, rate, 1, 0, a, b) }
	full := r(0.6, 1.4)
	for _, silent := range [][2]float64{{0, 0.49}, {1.51, 1.99}, {3.01, 3.49}, {4.01, 5}} {
		if v := r(silent[0], silent[1]); v != 0 {
			t.Errorf("%.2fs to %.2fs has level %.1f, want silence", silent[0], silent[1], v)
		}
	}
	if full < 1000 {
		t.Fatalf("the loud clip is nearly silent: %.1f", full)
	}
	if ratio := r(2.3, 2.9) / full; math.Abs(ratio-0.5) > 0.01 {
		t.Errorf("the -6 dB clip sits at %.3f of the loud one, want 0.5", ratio)
	}
	if early, late := r(2.0, 2.05), r(2.15, 2.2); !(early < late/2) {
		t.Errorf("the fade in does not rise: %.1f then %.1f", early, late)
	}
	if v := r(3.5, 3.99); v < 1000 {
		t.Errorf("the short source's half second is missing: %.1f", v)
	}
}

// TestRenderDucksTheBed renders a bed under one keyed track, then the same
// bed keyed on nothing, and checks the bed drops under the voice and is
// left alone away from it.
func TestRenderDucksTheBed(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	voice := tone(t, dir, "voice.wav", 1000, time.Second)
	bed := noise(t, dir, "bed.wav", 1500*time.Millisecond) // shorter than the timeline, so it loops
	tl := func(duck []string) mix.Timeline {
		return mix.Timeline{
			Tracks: []mix.Track{{Name: "voice", Clips: []mix.Clip{{Source: voice, Length: time.Second, Offset: 2 * time.Second}}}},
			Bed:    &mix.Bed{Source: bed, Gain: -6, DuckOn: duck},
			Length: 4 * time.Second,
		}
	}
	ducked := filepath.Join(dir, "ducked.wav")
	plain := filepath.Join(dir, "plain.wav")
	if err := mix.Render(t.Context(), ffmpeg.Tools{}, mix.Config{}, tl([]string{"voice"}), ducked); err != nil {
		t.Fatalf("Render ducked: %v", err)
	}
	if err := mix.Render(t.Context(), ffmpeg.Tools{}, mix.Config{}, tl(nil), plain); err != nil {
		t.Fatalf("Render plain: %v", err)
	}
	d, rate, ch := pcm(t, ducked)
	p, _, _ := pcm(t, plain)
	if rate != 48000 || ch != 2 || len(d) != 4*48000*2 || len(p) != len(d) {
		t.Fatalf("wrote %d Hz %d channels %d samples, want 48000 Hz stereo of 4s", rate, ch, len(d))
	}
	// Away from the voice the two mixes are the same bed, looped past its
	// end at 1.5s.
	if a, b := rms(d, rate, ch, 0, 0.5, 1.8), rms(p, rate, ch, 0, 0.5, 1.8); math.Abs(a-b) > b*0.01 || b < 100 {
		t.Errorf("before the voice: ducked %.1f, plain %.1f, want the same bed", a, b)
	}
	// Under the voice, the difference between the mixes is how far the
	// bed ducked, since the voice is in both.
	diff := make([]int16, len(d))
	for i := range d {
		diff[i] = p[i] - d[i]
	}
	bedLevel := rms(p, rate, ch, 0, 0.5, 1.8)
	if pulled := rms(diff, rate, ch, 0, 2.2, 2.9); pulled < bedLevel*0.3 {
		t.Errorf("the bed only dropped by %.1f under the voice against a level of %.1f", pulled, bedLevel)
	}
}

// measured runs an independent loudnorm measurement of a file and returns
// its integrated loudness.
func measured(t *testing.T, path string) float64 {
	t.Helper()
	stats := filepath.Join(t.TempDir(), "stats.json")
	if err := ffmpeg.Run(t.Context(), ffmpeg.Tools{}, "-nostdin", "-v", "error", "-i", path,
		"-af", "loudnorm=print_format=json:stats_file="+stats, "-f", "null", "-"); err != nil {
		t.Fatalf("measure %s: %v", path, err)
	}
	raw, err := os.ReadFile(stats)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]string
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	v, err := strconv.ParseFloat(parsed["input_i"], 64)
	if err != nil {
		t.Fatalf("input_i %q: %v", parsed["input_i"], err)
	}
	return v
}

// TestRenderNormalizesLoudness renders at a -16 LUFS target and checks an
// independent measurement lands within 0.5 LU, keeping the format and
// length; and a silent mix, which cannot be measured, is refused.
func TestRenderNormalizesLoudness(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := tone(t, dir, "tone.wav", 440, 2*time.Second)
	out := filepath.Join(dir, "loud.wav")
	tl := mix.Timeline{Tracks: []mix.Track{{Name: "a", Clips: []mix.Clip{{Source: src, Length: 2 * time.Second, Gain: -20}}}}}
	if err := mix.Render(t.Context(), ffmpeg.Tools{}, mix.Config{Normalize: true, Target: -16}, tl, out); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := measured(t, out); math.Abs(got+16) > 0.5 {
		t.Errorf("rendered loudness %.2f LUFS, want -16 within 0.5", got)
	}
	s, rate, ch := pcm(t, out)
	if rate != 48000 || ch != 2 || len(s) != 2*48000*2 {
		t.Errorf("wrote %d Hz %d channels %d samples, want 2s of 48000 Hz stereo", rate, ch, len(s))
	}

	silence := generate(t, dir, "silence.wav", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=mono", "-t", "1", "-c:a", "pcm_s16le")
	tl = mix.Timeline{Tracks: []mix.Track{{Name: "a", Clips: []mix.Clip{{Source: silence, Length: time.Second}}}}}
	err := mix.Render(t.Context(), ffmpeg.Tools{}, mix.Config{Normalize: true}, tl, filepath.Join(dir, "silent.wav"))
	if !errors.Is(err, mix.ErrNoMeasurement) {
		t.Errorf("silent Render error = %v, want ErrNoMeasurement", err)
	}
}

// TestRenderIsByteIdentical renders one timeline four times, and a
// shuffled copy of it once, with every feature on, and compares the bytes.
func TestRenderIsByteIdentical(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	voice := tone(t, dir, "voice.wav", 700, 2*time.Second)
	hit := tone(t, dir, "hit.wav", 1200, 300*time.Millisecond)
	bed := noise(t, dir, "bed.wav", time.Second)
	lines := []mix.Clip{
		{Source: voice, Length: 1500 * time.Millisecond, Offset: 300 * time.Millisecond, FadeIn: 50 * time.Millisecond, FadeOut: 100 * time.Millisecond},
		{Source: voice, In: 500 * time.Millisecond, Length: time.Second, Offset: 2 * time.Second, Gain: -3},
	}
	hits := []mix.Clip{
		{Source: hit, Length: 300 * time.Millisecond, Offset: time.Second, Gain: -9},
		{Source: hit, Length: 300 * time.Millisecond, Offset: 1100 * time.Millisecond, Gain: -9},
	}
	cfg := mix.Config{Normalize: true, Target: -18, Duck: mix.Duck{Ratio: 4}}
	one := mix.Timeline{
		Tracks: []mix.Track{{Name: "dialogue", Clips: lines}, {Name: "fx", Clips: hits}},
		Bed:    &mix.Bed{Source: bed, Gain: -10, DuckOn: []string{"dialogue"}},
	}
	two := mix.Timeline{
		Tracks: []mix.Track{{Name: "fx", Clips: []mix.Clip{hits[1], hits[0]}}, {Name: "dialogue", Clips: []mix.Clip{lines[1], lines[0]}}},
		Bed:    one.Bed,
	}
	var outputs [][]byte
	// Four renders of one timeline give the decoder threads room to
	// schedule differently; the last render is the shuffled copy.
	for i, tl := range []mix.Timeline{one, one, one, one, two} {
		out := filepath.Join(dir, "render-"+strconv.Itoa(i)+".wav")
		if err := mix.Render(t.Context(), ffmpeg.Tools{}, cfg, tl, out); err != nil {
			t.Fatalf("Render %d: %v", i, err)
		}
		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, raw)
	}
	for i := 1; i < 4; i++ {
		if !bytes.Equal(outputs[0], outputs[i]) {
			t.Errorf("render %d of one timeline differs from the first", i)
		}
	}
	if !bytes.Equal(outputs[0], outputs[4]) {
		t.Error("a shuffled copy of the timeline renders differently")
	}
}

// probeStreams returns codec_type:codec_name for each stream of a file,
// with the number of video packets, read by ffprobe on its own.
func probeStreams(t *testing.T, path string) (string, string) {
	t.Helper()
	var out strings.Builder
	cmd := ffmpeg.Command(t.Context(), "ffprobe", "-v", "error",
		"-show_entries", "stream=codec_type,codec_name", "-of", "csv=p=0", path)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var md5 strings.Builder
	cmd = ffmpeg.Command(t.Context(), "ffmpeg", "-v", "error", "-i", path, "-map", "0:v:0", "-c", "copy", "-f", "md5", "-")
	cmd.Stdout = &md5
	if err := cmd.Run(); err != nil {
		t.Fatalf("hash picture of %s: %v", path, err)
	}
	return strings.TrimSpace(out.String()), strings.TrimSpace(md5.String())
}

// TestMuxCopiesThePicture puts a rendered mix under a video and checks
// the picture's packets are untouched and the audio is the mix.
func TestMuxCopiesThePicture(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	video := generate(t, dir, "film.mp4", "-f", "lavfi", "-i", "testsrc=size=160x90:rate=25:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=300:duration=2", "-c:v", "mpeg4", "-c:a", "aac", "-shortest")
	src := tone(t, dir, "tone.wav", 440, 2*time.Second)
	audio := filepath.Join(dir, "mix.wav")
	tl := mix.Timeline{Tracks: []mix.Track{{Name: "a", Clips: []mix.Clip{{Source: src, Length: 2 * time.Second}}}}}
	if err := mix.Render(t.Context(), ffmpeg.Tools{}, mix.Config{}, tl, audio); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := filepath.Join(dir, "muxed.mp4")
	if err := mix.Mux(t.Context(), ffmpeg.Tools{}, video, audio, out); err != nil {
		t.Fatalf("Mux: %v", err)
	}
	_, before := probeStreams(t, video)
	streams, after := probeStreams(t, out)
	if streams != "mpeg4,video\naac,audio" {
		t.Errorf("muxed streams = %q, want the copied mpeg4 and an aac mix", streams)
	}
	if before != after {
		t.Errorf("the picture changed: %s then %s", before, after)
	}
	d, err := ffmpeg.Duration(t.Context(), ffmpeg.Tools{}, out)
	if err != nil || d < 1990*time.Millisecond || d > 2100*time.Millisecond {
		t.Errorf("muxed duration = %v, %v, want about 2s", d, err)
	}
}

// TestRenderAndMuxRefuse checks the refusals of paths, a missing binary
// and a cancelled context.
func TestRenderAndMuxRefuse(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := tone(t, dir, "tone.wav", 440, time.Second)
	tl := mix.Timeline{Tracks: []mix.Track{{Name: "a", Clips: []mix.Clip{{Source: src, Length: time.Second}}}}}
	out := filepath.Join(dir, "out.wav")

	if err := mix.Render(t.Context(), ffmpeg.Tools{}, mix.Config{}, tl, ""); !errors.Is(err, mix.ErrInvalid) {
		t.Errorf("empty out error = %v, want ErrInvalid", err)
	}
	if err := mix.Render(t.Context(), ffmpeg.Tools{}, mix.Config{}, tl, src); !errors.Is(err, mix.ErrInvalid) {
		t.Errorf("out naming a source error = %v, want ErrInvalid", err)
	}
	if err := mix.Render(t.Context(), ffmpeg.Tools{}, mix.Config{}, mix.Timeline{}, out); !errors.Is(err, mix.ErrInvalid) {
		t.Errorf("empty timeline error = %v, want ErrInvalid", err)
	}
	missing := filepath.Join(dir, "no-such-binary")
	if err := mix.Render(t.Context(), ffmpeg.Tools{FFmpeg: missing}, mix.Config{}, tl, out); !errors.Is(err, ffmpeg.ErrNotFound) {
		t.Errorf("missing ffmpeg error = %v, want ffmpeg.ErrNotFound", err)
	}
	if err := mix.Render(t.Context(), ffmpeg.Tools{FFmpeg: missing}, mix.Config{Normalize: true}, tl, out); !errors.Is(err, ffmpeg.ErrNotFound) {
		t.Errorf("missing ffmpeg on the measure pass error = %v, want ffmpeg.ErrNotFound", err)
	}
	absent := mix.Timeline{Tracks: []mix.Track{{Name: "a", Clips: []mix.Clip{{Source: filepath.Join(dir, "absent.wav"), Length: time.Second}}}}}
	if err := mix.Render(t.Context(), ffmpeg.Tools{}, mix.Config{}, absent, out); !errors.Is(err, ffmpeg.ErrFailed) {
		t.Errorf("absent source error = %v, want ffmpeg.ErrFailed", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := mix.Render(ctx, ffmpeg.Tools{}, mix.Config{}, tl, out); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Render error = %v, want context.Canceled", err)
	}

	for _, args := range [][3]string{{"", src, out}, {src, "", out}, {src, src, ""}, {src, out, src}, {out, src, src}} {
		if err := mix.Mux(t.Context(), ffmpeg.Tools{}, args[0], args[1], args[2]); !errors.Is(err, mix.ErrInvalid) {
			t.Errorf("Mux%q error = %v, want ErrInvalid", args, err)
		}
	}
	if err := mix.Mux(t.Context(), ffmpeg.Tools{FFmpeg: missing}, src, src, out); !errors.Is(err, ffmpeg.ErrNotFound) {
		t.Errorf("Mux missing ffmpeg error = %v, want ffmpeg.ErrNotFound", err)
	}
	if err := mix.Mux(ctx, ffmpeg.Tools{}, src, src, out); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Mux error = %v, want context.Canceled", err)
	}
}
