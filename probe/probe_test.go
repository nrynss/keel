package probe_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/ffmpeg"
	"github.com/nrynss/keel/probe"
)

// clipDir holds the clips the tests generate once per run. TestMain makes
// and removes it, so no clip outlives the run and none is committed.
var clipDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "keel-probe-clips-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "clip dir:", err)
		os.Exit(1)
	}
	clipDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir) // the run is over, so a failed removal leaves only a temp dir behind
	os.Exit(code)
}

// requireFFmpeg skips a test when ffmpeg and ffprobe are absent. CI sets
// KEEL_REQUIRE_FFMPEG so a missing binary fails there instead of skipping,
// and the suite never passes without running the real tools.
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

// clipRecipes generates each named clip from lavfi sources alone, so every
// clip is a pure function of its arguments on the pinned ffmpeg. A recipe
// may name another clip as its input with the {clip:name} form below.
var clipRecipes = map[string][]string{
	// Two seconds of 25 fps test picture under a stereo 44.1 kHz tone.
	"av.mp4": {
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=25:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=2",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-ac", "2", "-shortest",
	},
	// Picture only, no audio.
	"silent.mkv": {
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=25:duration=2",
		"-c:v", "ffv1",
	},
	// One black second, then one white second, losslessly coded.
	"blackwhite.mkv": {
		"-f", "lavfi", "-i", "color=c=black:size=64x36:rate=25:duration=1",
		"-f", "lavfi", "-i", "color=c=white:size=64x36:rate=25:duration=1",
		"-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0,format=yuv420p[v]", "-map", "[v]",
		"-c:v", "ffv1",
	},
	// Three shots a second each: test picture, a fractal, then white. The
	// cuts score 0.53 and 0.89 on the pinned ffmpeg.
	"shots.mkv": {
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=25:duration=1",
		"-f", "lavfi", "-i", "mandelbrot=size=160x90:rate=25",
		"-f", "lavfi", "-i", "color=c=white:size=160x90:rate=25:duration=1",
		"-filter_complex", "[1:v]trim=duration=1,setpts=PTS-STARTPTS[m];[0:v][m][2:v]concat=n=3:v=1:a=0,format=yuv420p[v]",
		"-map", "[v]", "-c:v", "ffv1",
	},
	// Twenty frames 50 ms apart, then ten frames 100 ms apart.
	"vfr.mkv": {
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=20:duration=2",
		"-vf", "setpts='if(lt(N,20),N*0.05,1+(N-20)*0.1)/TB'", "-fps_mode", "vfr",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
	},
	"vfr.mp4": {
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=20:duration=2",
		"-vf", "setpts='if(lt(N,20),N*0.05,1+(N-20)*0.1)/TB'", "-fps_mode", "vfr",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
	},
	// NTSC rate in a millisecond time base, so the frame gaps alternate
	// between 33 and 34 ticks and the stream is still constant.
	"ntsc.mkv": {
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=30000/1001:duration=2",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
	},
	// A tone with a cover picture, the shape of a music file.
	"cover.mp3": {
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=1",
		"-f", "lavfi", "-i", "color=c=red:size=32x32:duration=1",
		"-map", "0:a", "-map", "1:v", "-frames:v", "1",
		"-c:a", "libmp3lame", "-c:v", "png", "-disposition:v:0", "attached_pic", "-id3v2_version", "3",
	},
}

// rotations maps a rotated clip to the counter-clockwise display rotation
// its recipe stamps on a stream copy of av.mp4.
var rotations = map[string]string{
	"rot90.mp4":  "270",
	"rot180.mp4": "180",
	"rot270.mp4": "90",
}

var (
	clipMu   sync.Mutex
	clipDone = map[string]string{}
)

// clip returns the path of a generated clip, making it on first use.
func clip(t *testing.T, name string) string {
	t.Helper()
	requireFFmpeg(t)
	clipMu.Lock()
	defer clipMu.Unlock()
	if p, ok := clipDone[name]; ok {
		return p
	}
	out := filepath.Join(clipDir, name)
	var args []string
	if ccw, ok := rotations[name]; ok {
		src := filepath.Join(clipDir, "av.mp4")
		if _, ok := clipDone["av.mp4"]; !ok {
			makeClip(t, src, clipRecipes["av.mp4"])
			clipDone["av.mp4"] = src
		}
		args = []string{"-display_rotation", ccw, "-i", src, "-c", "copy"}
	} else {
		recipe, ok := clipRecipes[name]
		if !ok {
			t.Fatalf("no recipe for clip %s", name)
		}
		args = recipe
	}
	makeClip(t, out, args)
	clipDone[name] = out
	return out
}

// makeClip runs one recipe into out with bit exact flags, so the container
// carries no tool version.
func makeClip(t *testing.T, out string, args []string) {
	t.Helper()
	full := append([]string{"-nostdin", "-v", "error", "-y"}, args...)
	full = append(full, "-fflags", "+bitexact", out)
	if err := ffmpeg.Run(t.Context(), ffmpeg.Tools{}, full...); err != nil {
		t.Fatalf("make clip %s: %v", filepath.Base(out), err)
	}
}

// fixture returns a committed audio fixture.
func fixture(t *testing.T, name string) string {
	t.Helper()
	requireFFmpeg(t)
	path := filepath.Join("../testdata/audio", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture %s: %v", path, err)
	}
	return path
}

// mustProbe probes path or ends the test.
func mustProbe(t *testing.T, path string) probe.Media {
	t.Helper()
	media, err := probe.Probe(t.Context(), ffmpeg.Tools{}, path)
	if err != nil {
		t.Fatalf("Probe(%s): %v", filepath.Base(path), err)
	}
	return media
}

// TestProbeReadsTypedStreams pins every field Probe reports for a clip of
// picture and sound against the recipe that made it.
func TestProbeReadsTypedStreams(t *testing.T) {
	media := mustProbe(t, clip(t, "av.mp4"))
	if media.Format != "mov,mp4,m4a,3gp,3g2,mj2" {
		t.Errorf("Format = %q, want the mp4 family", media.Format)
	}
	if media.Duration != 2*time.Second {
		t.Errorf("Duration = %v, want 2s", media.Duration)
	}
	if len(media.Streams) != 2 {
		t.Fatalf("Streams = %d, want 2", len(media.Streams))
	}
	video, ok := media.Video()
	if !ok {
		t.Fatal("Video() found no stream")
	}
	wantVideo := probe.Stream{
		Index: 0, Kind: probe.KindVideo, Codec: "h264", Duration: 2 * time.Second,
		BitRate: video.BitRate, Width: 160, Height: 90, FrameRate: probe.Rate{Num: 25, Den: 1},
	}
	if video != wantVideo {
		t.Errorf("video = %+v, want %+v", video, wantVideo)
	}
	if video.BitRate <= 0 {
		t.Errorf("video BitRate = %d, want a positive rate", video.BitRate)
	}
	audio, ok := media.Audio()
	if !ok {
		t.Fatal("Audio() found no stream")
	}
	wantAudio := probe.Stream{
		Index: 1, Kind: probe.KindAudio, Codec: "aac", Duration: 2 * time.Second,
		BitRate: audio.BitRate, SampleRate: 44100, Channels: 2, ChannelLayout: "stereo",
	}
	if audio != wantAudio {
		t.Errorf("audio = %+v, want %+v", audio, wantAudio)
	}
	if w, h := video.DisplaySize(); w != 160 || h != 90 {
		t.Errorf("DisplaySize = %dx%d, want 160x90", w, h)
	}
}

// TestProbeAgreesWithTheFFmpegPackage pins the promise that Probe builds on
// ffmpeg.Duration and ffmpeg.AudioFormat: for every committed fixture and a
// generated clip, the duration and the first audio stream match field for
// field.
func TestProbeAgreesWithTheFFmpegPackage(t *testing.T) {
	paths := []string{clip(t, "av.mp4"), clip(t, "cover.mp3")}
	for _, name := range []string{"tone-48k.wav", "tone-24k.wav", "tone-48k.opus", "tone-48k.mp3", "truncated.wav"} {
		paths = append(paths, fixture(t, name))
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			media := mustProbe(t, path)
			duration, err := ffmpeg.Duration(t.Context(), ffmpeg.Tools{}, path)
			if err != nil {
				t.Fatalf("ffmpeg.Duration: %v", err)
			}
			if media.Duration != duration {
				t.Errorf("Probe duration = %v, ffmpeg.Duration = %v", media.Duration, duration)
			}
			format, err := ffmpeg.AudioFormat(t.Context(), ffmpeg.Tools{}, path)
			if err != nil {
				t.Fatalf("ffmpeg.AudioFormat: %v", err)
			}
			audio, ok := media.Audio()
			if !ok {
				t.Fatal("Audio() found no stream")
			}
			got := ffmpeg.Format{SampleRate: audio.SampleRate, Channels: audio.Channels,
				ChannelLayout: audio.ChannelLayout, Codec: audio.Codec, BitRate: audio.BitRate}
			if got != format {
				t.Errorf("Probe audio = %+v, ffmpeg.AudioFormat = %+v", got, format)
			}
		})
	}
}

// TestProbeReportsRotationExplicitly pins the clockwise rotation and the
// display size for each quarter turn a stream copy stamps on the clip.
func TestProbeReportsRotationExplicitly(t *testing.T) {
	tests := []struct {
		name     string
		rotation int
		w, h     int
	}{
		{name: "av.mp4", rotation: 0, w: 160, h: 90},
		{name: "rot90.mp4", rotation: 90, w: 90, h: 160},
		{name: "rot180.mp4", rotation: 180, w: 160, h: 90},
		{name: "rot270.mp4", rotation: 270, w: 90, h: 160},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			video, ok := mustProbe(t, clip(t, tt.name)).Video()
			if !ok {
				t.Fatal("Video() found no stream")
			}
			if video.Rotation != tt.rotation {
				t.Errorf("Rotation = %d, want %d", video.Rotation, tt.rotation)
			}
			if video.Width != 160 || video.Height != 90 {
				t.Errorf("stored size = %dx%d, want 160x90", video.Width, video.Height)
			}
			if w, h := video.DisplaySize(); w != tt.w || h != tt.h {
				t.Errorf("DisplaySize = %dx%d, want %dx%d", w, h, tt.w, tt.h)
			}
		})
	}
}

// TestProbeFlagsVariableFrameRate pins the packet scan. Both containers of
// the variable clip are flagged, including the Matroska file whose ffprobe
// rate fields both claim a constant 20 fps, and the NTSC clip whose gaps
// alternate by one tick stays constant at its exact rate.
func TestProbeFlagsVariableFrameRate(t *testing.T) {
	tests := []struct {
		name     string
		variable bool
		rate     probe.Rate
	}{
		{name: "av.mp4", variable: false, rate: probe.Rate{Num: 25, Den: 1}},
		{name: "ntsc.mkv", variable: false, rate: probe.Rate{Num: 30000, Den: 1001}},
		{name: "vfr.mkv", variable: true},
		{name: "vfr.mp4", variable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			video, ok := mustProbe(t, clip(t, tt.name)).Video()
			if !ok {
				t.Fatal("Video() found no stream")
			}
			if video.VariableFrameRate != tt.variable {
				t.Errorf("VariableFrameRate = %v, want %v", video.VariableFrameRate, tt.variable)
			}
			if !tt.variable && video.FrameRate != tt.rate {
				t.Errorf("FrameRate = %v, want %v", video.FrameRate, tt.rate)
			}
			if tt.variable {
				// Thirty frames over a little under three seconds sit
				// between the two rates the clip mixes.
				if fps := video.FrameRate.Float64(); fps <= 10 || fps >= 20 {
					t.Errorf("FrameRate = %v (%.3f fps), want a mean between 10 and 20", video.FrameRate, fps)
				}
			}
		})
	}
}

// TestProbeTreatsMissingStreamsExplicitly pins that a file without a kind
// of stream says so instead of handing back a zero stream, and that cover
// art does not pass for moving picture.
func TestProbeTreatsMissingStreamsExplicitly(t *testing.T) {
	tests := []struct {
		name      string
		path      func(t *testing.T) string
		wantAudio bool
		wantVideo bool
	}{
		{name: "audio only", path: func(t *testing.T) string { return fixture(t, "tone-48k.wav") }, wantAudio: true},
		{name: "picture only", path: func(t *testing.T) string { return clip(t, "silent.mkv") }, wantVideo: true},
		{name: "music with cover art", path: func(t *testing.T) string { return clip(t, "cover.mp3") }, wantAudio: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			media := mustProbe(t, tt.path(t))
			if _, ok := media.Audio(); ok != tt.wantAudio {
				t.Errorf("Audio() ok = %v, want %v", ok, tt.wantAudio)
			}
			if _, ok := media.Video(); ok != tt.wantVideo {
				t.Errorf("Video() ok = %v, want %v", ok, tt.wantVideo)
			}
		})
	}
	media := mustProbe(t, clip(t, "cover.mp3"))
	var cover probe.Stream
	for _, s := range media.Streams {
		if s.Kind == probe.KindVideo {
			cover = s
		}
	}
	if !cover.AttachedPicture || cover.FrameRate != (probe.Rate{}) || cover.VariableFrameRate {
		t.Errorf("cover stream = %+v, want an attached picture with no frame rate", cover)
	}
}

// TestProbeRefusesWhatItCannotRead pins the error for an empty path, a file
// ffprobe cannot read, a missing binary and a cancelled context.
func TestProbeRefusesWhatItCannotRead(t *testing.T) {
	if _, err := probe.Probe(t.Context(), ffmpeg.Tools{}, ""); !errors.Is(err, probe.ErrInvalid) {
		t.Errorf("Probe(empty) error = %v, want ErrInvalid", err)
	}
	if _, err := probe.Probe(t.Context(), ffmpeg.Tools{}, fixture(t, "random-20m.bin")); !errors.Is(err, ffmpeg.ErrFailed) {
		t.Errorf("Probe(random bytes) error = %v, want ffmpeg.ErrFailed", err)
	}
	missing := ffmpeg.Tools{FFprobe: filepath.Join(t.TempDir(), "no-such-ffprobe")}
	if _, err := probe.Probe(t.Context(), missing, fixture(t, "tone-48k.wav")); !errors.Is(err, ffmpeg.ErrNotFound) {
		t.Errorf("Probe(missing binary) error = %v, want ffmpeg.ErrNotFound", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := probe.Probe(ctx, ffmpeg.Tools{}, fixture(t, "tone-48k.wav")); !errors.Is(err, context.Canceled) {
		t.Errorf("Probe(cancelled) error = %v, want context.Canceled", err)
	}
}

// TestScenesReturnsCutTimes pins the cut times the scene filter prints for
// a clip of three shots at the default threshold and two others.
func TestScenesReturnsCutTimes(t *testing.T) {
	path := clip(t, "shots.mkv")
	tests := []struct {
		threshold float64
		want      []time.Duration
	}{
		{threshold: 0, want: []time.Duration{time.Second, 2 * time.Second}},
		{threshold: probe.DefaultSceneThreshold, want: []time.Duration{time.Second, 2 * time.Second}},
		{threshold: 0.7, want: []time.Duration{2 * time.Second}},
		{threshold: 0.95, want: []time.Duration{}},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.threshold), func(t *testing.T) {
			got, err := probe.Scenes(t.Context(), ffmpeg.Tools{}, path, tt.threshold)
			if err != nil {
				t.Fatalf("Scenes: %v", err)
			}
			if !slices.Equal(got, tt.want) || got == nil {
				t.Errorf("Scenes = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestScenesRefusesBadInput pins the refusals that come before any scene
// run: a threshold out of range and a file without moving picture.
func TestScenesRefusesBadInput(t *testing.T) {
	for _, threshold := range []float64{-0.1, 1.01} {
		if _, err := probe.Scenes(t.Context(), ffmpeg.Tools{}, "clip.mp4", threshold); !errors.Is(err, probe.ErrInvalid) {
			t.Errorf("Scenes(threshold %v) error = %v, want ErrInvalid", threshold, err)
		}
	}
	if _, err := probe.Scenes(t.Context(), ffmpeg.Tools{}, clip(t, "cover.mp3"), 0); !errors.Is(err, probe.ErrNoStream) {
		t.Errorf("Scenes(cover art only) error = %v, want ErrNoStream", err)
	}
}

// grey reads the centre pixel of a PNG as one grey level, so a test can
// tell a black still from a white one.
func grey(t *testing.T, path string) (uint8, image.Rectangle) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read still: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode still %s: %v", filepath.Base(path), err)
	}
	b := img.Bounds()
	r, g, bl, _ := img.At(b.Min.X+b.Dx()/2, b.Min.Y+b.Dy()/2).RGBA()
	return uint8(((r + g + bl) / 3) >> 8), b
}

// TestFramesWritesTheFrameAtEachTime pins which frame each time selects on
// a clip that turns from black to white at one second: a time inside a
// frame's span returns the next frame, and a time on a boundary returns
// that frame. The stills come back in the order of the times.
func TestFramesWritesTheFrameAtEachTime(t *testing.T) {
	dir := t.TempDir()
	times := []time.Duration{
		1500 * time.Millisecond, 0, 500 * time.Millisecond, 950 * time.Millisecond,
		970 * time.Millisecond, time.Second,
	}
	white := []bool{true, false, false, false, true, true}
	paths, err := probe.Frames(t.Context(), ffmpeg.Tools{}, clip(t, "blackwhite.mkv"), times, probe.Size{}, dir)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	if len(paths) != len(times) {
		t.Fatalf("Frames returned %d paths, want %d", len(paths), len(times))
	}
	for i, path := range paths {
		if want := filepath.Join(dir, fmt.Sprintf("frame-%04d.png", i)); path != want {
			t.Errorf("path %d = %s, want %s", i, path, want)
		}
		level, bounds := grey(t, path)
		if got := level > 128; got != white[i] {
			t.Errorf("still at %v has grey level %d, want white %v", times[i], level, white[i])
		}
		if bounds.Dx() != 64 || bounds.Dy() != 36 {
			t.Errorf("still at %v is %dx%d, want 64x36", times[i], bounds.Dx(), bounds.Dy())
		}
	}
}

// TestFramesScalesInDisplayOrientation pins the size rules and that the
// rotation is applied before them.
func TestFramesScalesInDisplayOrientation(t *testing.T) {
	tests := []struct {
		clip string
		size probe.Size
		w, h int
	}{
		{clip: "av.mp4", size: probe.Size{}, w: 160, h: 90},
		{clip: "av.mp4", size: probe.Size{Width: 80}, w: 80, h: 45},
		{clip: "av.mp4", size: probe.Size{Height: 45}, w: 80, h: 45},
		{clip: "av.mp4", size: probe.Size{Width: 50, Height: 50}, w: 50, h: 28},
		{clip: "rot90.mp4", size: probe.Size{}, w: 90, h: 160},
		{clip: "rot90.mp4", size: probe.Size{Height: 80}, w: 45, h: 80},
		{clip: "rot180.mp4", size: probe.Size{}, w: 160, h: 90},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s %dx%d", tt.clip, tt.size.Width, tt.size.Height), func(t *testing.T) {
			paths, err := probe.Frames(t.Context(), ffmpeg.Tools{}, clip(t, tt.clip), []time.Duration{time.Second}, tt.size, t.TempDir())
			if err != nil {
				t.Fatalf("Frames: %v", err)
			}
			_, bounds := grey(t, paths[0])
			if bounds.Dx() != tt.w || bounds.Dy() != tt.h {
				t.Errorf("still is %dx%d, want %dx%d", bounds.Dx(), bounds.Dy(), tt.w, tt.h)
			}
		})
	}
}

// TestFramesRefusesBadInput pins every refusal: times outside the file, a
// time inside the last frame that yields no frame, a negative size, and a
// file without moving picture.
func TestFramesRefusesBadInput(t *testing.T) {
	path := clip(t, "blackwhite.mkv")
	invalid := []struct {
		name  string
		times []time.Duration
		size  probe.Size
	}{
		{name: "negative time", times: []time.Duration{-time.Millisecond}},
		{name: "time at the end", times: []time.Duration{0, 2 * time.Second}},
		{name: "negative size", times: []time.Duration{0}, size: probe.Size{Width: -1}},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := probe.Frames(t.Context(), ffmpeg.Tools{}, path, tt.times, tt.size, dir); !errors.Is(err, probe.ErrInvalid) {
				t.Errorf("Frames error = %v, want ErrInvalid", err)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("Frames wrote %d files before refusing, want none", len(entries))
			}
		})
	}
	t.Run("inside the last frame", func(t *testing.T) {
		paths, err := probe.Frames(t.Context(), ffmpeg.Tools{}, path,
			[]time.Duration{0, 1961 * time.Millisecond}, probe.Size{}, t.TempDir())
		if !errors.Is(err, probe.ErrNoFrame) {
			t.Fatalf("Frames error = %v, want ErrNoFrame", err)
		}
		if len(paths) != 1 {
			t.Errorf("Frames returned %d paths, want the one written before the failure", len(paths))
		}
	})
	t.Run("no moving picture", func(t *testing.T) {
		if _, err := probe.Frames(t.Context(), ffmpeg.Tools{}, fixture(t, "tone-48k.wav"),
			[]time.Duration{0}, probe.Size{}, t.TempDir()); !errors.Is(err, probe.ErrNoStream) {
			t.Errorf("Frames error = %v, want ErrNoStream", err)
		}
	})
}

// TestExtractAudioWritesAWorkingWAV pins the format ExtractAudio writes at
// the source rate and at a requested one, measured from the written file,
// and that the same request writes the same bytes twice.
func TestExtractAudioWritesAWorkingWAV(t *testing.T) {
	src := clip(t, "av.mp4")
	tests := []struct {
		name string
		f    probe.Format
		want probe.Format
	}{
		{name: "source rate", f: probe.Format{}, want: probe.Format{SampleRate: 44100, Channels: 2}},
		{name: "requested rate", f: probe.Format{SampleRate: 16000, Channels: 1}, want: probe.Format{SampleRate: 16000, Channels: 1}},
		{name: "rate only", f: probe.Format{SampleRate: 48000}, want: probe.Format{SampleRate: 48000, Channels: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			first, second := filepath.Join(dir, "a.wav"), filepath.Join(dir, "b.wav")
			for _, out := range []string{first, second} {
				got, err := probe.ExtractAudio(t.Context(), ffmpeg.Tools{}, src, out, tt.f)
				if err != nil {
					t.Fatalf("ExtractAudio: %v", err)
				}
				if got != tt.want {
					t.Errorf("ExtractAudio = %+v, want %+v", got, tt.want)
				}
			}
			format, err := ffmpeg.AudioFormat(t.Context(), ffmpeg.Tools{}, first)
			if err != nil {
				t.Fatalf("AudioFormat: %v", err)
			}
			if format.Codec != "pcm_s16le" || format.SampleRate != tt.want.SampleRate || format.Channels != tt.want.Channels {
				t.Errorf("written format = %+v, want pcm_s16le at %+v", format, tt.want)
			}
			a, errA := os.ReadFile(first)
			b, errB := os.ReadFile(second)
			if errA != nil || errB != nil {
				t.Fatalf("read outputs: %v, %v", errA, errB)
			}
			if !bytes.Equal(a, b) {
				t.Error("two extractions of one request differ")
			}
		})
	}
}

// TestExtractAudioRefusesBadInput pins the refusals before and after the
// probe.
func TestExtractAudioRefusesBadInput(t *testing.T) {
	src := fixture(t, "tone-48k.wav")
	out := filepath.Join(t.TempDir(), "out.wav")
	invalid := []struct {
		name     string
		src, out string
		f        probe.Format
	}{
		{name: "empty source", src: "", out: out},
		{name: "empty output", src: src, out: ""},
		{name: "output is the source", src: src, out: "./" + src},
		{name: "negative rate", src: src, out: out, f: probe.Format{SampleRate: -1}},
		{name: "negative channels", src: src, out: out, f: probe.Format{Channels: -2}},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := probe.ExtractAudio(t.Context(), ffmpeg.Tools{}, tt.src, tt.out, tt.f); !errors.Is(err, probe.ErrInvalid) {
				t.Errorf("ExtractAudio error = %v, want ErrInvalid", err)
			}
		})
	}
	if _, err := probe.ExtractAudio(t.Context(), ffmpeg.Tools{}, clip(t, "silent.mkv"), out, probe.Format{}); !errors.Is(err, probe.ErrNoStream) {
		t.Errorf("ExtractAudio(no audio) error = %v, want ErrNoStream", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused extraction left %s behind: %v", out, err)
	}
}
