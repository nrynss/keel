package waveform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nrynss/keel/ffmpeg"
)

// fixtureDir holds the committed fixtures, relative to the package dir.
const fixtureDir = "testdata"

// fixturePath returns the path to a committed fixture.
func fixturePath(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(fixtureDir, name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture %s: %v", path, err)
	}
	return path
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

// render writes one video from the given still and the committed tone and
// returns the output path.
func render(t *testing.T, cfg Config, still string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out.mkv")
	err := Render(context.Background(), ffmpeg.Tools{}, cfg, still, fixturePath(t, "tone.wav"), out)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return out
}

// probeStream holds the stream facts the tests assert on.
type probeStream struct {
	Index         int    `json:"index"`
	CodecName     string `json:"codec_name"`
	CodecType     string `json:"codec_type"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	PixFmt        string `json:"pix_fmt"`
	SampleAspect  string `json:"sample_aspect_ratio"`
	DisplayAspect string `json:"display_aspect_ratio"`
	SampleRate    string `json:"sample_rate"`
	Channels      int    `json:"channels"`
}

// probeOutput is what ffprobe reports about one file.
type probeOutput struct {
	Streams []probeStream `json:"streams"`
	Format  struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// probe runs ffprobe over one file and returns what it reported.
func probe(t *testing.T, path string) probeOutput {
	t.Helper()
	var stdout bytes.Buffer
	cmd := ffmpeg.Command(context.Background(), "ffprobe",
		"-v", "error", "-show_streams", "-show_format", "-of", "json", path)
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var res probeOutput
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("decode ffprobe output for %s: %v", path, err)
	}
	return res
}

// extractFrame decodes the first video frame of one rendered file.
func extractFrame(t *testing.T, video string) image.Image {
	t.Helper()
	path := filepath.Join(t.TempDir(), "frame.png")
	err := ffmpeg.Run(context.Background(), ffmpeg.Tools{},
		"-y", "-i", video, "-frames:v", "1", path)
	if err != nil {
		t.Fatalf("extract frame: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open frame: %v", err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return img
}

// pixel reads one pixel as three eight bit channels.
func pixel(img image.Image, x, y int) (int, int, int) {
	r, g, b, _ := img.At(x, y).RGBA()
	return int(r >> 8), int(g >> 8), int(b >> 8)
}

// isRed reports a strong red such as the still fixture keeps after the
// yuv420p round trip.
func isRed(r, g, b int) bool { return r > 180 && g < 80 && b < 80 }

// isWhite reports a strong white such as a waveform line keeps.
func isWhite(r, g, b int) bool { return r > 180 && g > 180 && b > 180 }

// isGreen reports a strong green such as the pad colour keeps. The yuv420p
// round trip halves the green channel of a pure green, so the bar reads as
// (0, 127, 0) rather than full green.
func isGreen(r, g, b int) bool { return g > 100 && r < 80 && b < 80 }

// isBlack reports a strong black such as a default pad bar keeps.
func isBlack(r, g, b int) bool { return r < 40 && g < 40 && b < 40 }

// countPixels counts the pixels of one image that satisfy match.
func countPixels(img image.Image, match func(r, g, b int) bool) int {
	n := 0
	bounds := img.Bounds()
	for x := bounds.Min.X; x < bounds.Max.X; x++ {
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			if match(pixel(img, x, y)) {
				n++
			}
		}
	}
	return n
}

// TestRenderMatchesFFprobe checks the dimensions, the duration and the
// stream layout ffprobe reports for one render. The gate runs one pinned
// ffmpeg version, so every value asserts exactly.
func TestRenderMatchesFFprobe(t *testing.T) {
	requireFFmpeg(t)
	out := render(t, Config{Width: 320, Height: 180}, fixturePath(t, "still.png"))
	res := probe(t, out)

	if len(res.Streams) != 2 {
		t.Fatalf("stream count = %d, want 2", len(res.Streams))
	}
	video, audio := res.Streams[0], res.Streams[1]
	if video.Index != 0 || video.CodecType != "video" || video.CodecName != "h264" {
		t.Errorf("stream 0 = index %d %s/%s, want index 0 video/h264",
			video.Index, video.CodecType, video.CodecName)
	}
	if video.Width != 320 || video.Height != 180 {
		t.Errorf("frame = %dx%d, want 320x180", video.Width, video.Height)
	}
	if video.PixFmt != "yuv420p" {
		t.Errorf("pix fmt = %s, want yuv420p", video.PixFmt)
	}
	if video.SampleAspect != "1:1" || video.DisplayAspect != "16:9" {
		t.Errorf("aspect = %s sample, %s display, want 1:1 sample, 16:9 display",
			video.SampleAspect, video.DisplayAspect)
	}
	if audio.Index != 1 || audio.CodecType != "audio" || audio.CodecName != "pcm_s16le" {
		t.Errorf("stream 1 = index %d %s/%s, want index 1 audio/pcm_s16le",
			audio.Index, audio.CodecType, audio.CodecName)
	}
	if audio.SampleRate != "44100" || audio.Channels != 1 {
		t.Errorf("audio shape = %s Hz over %d channels, want 44100 Hz over 1",
			audio.SampleRate, audio.Channels)
	}
	if res.Format.Duration != "2.000000" {
		t.Errorf("container duration = %s, want 2.000000", res.Format.Duration)
	}

	in, err := ffmpeg.Duration(context.Background(), ffmpeg.Tools{}, fixturePath(t, "tone.wav"))
	if err != nil {
		t.Fatalf("probe input duration: %v", err)
	}
	got, err := ffmpeg.Duration(context.Background(), ffmpeg.Tools{}, out)
	if err != nil {
		t.Fatalf("probe output duration: %v", err)
	}
	if in != 2*time.Second || got != in {
		t.Errorf("duration = %s, want the input's %s of exactly 2s", got, in)
	}
}

// TestRenderPadsDifferentAspectRatio checks that a 4:3 still fills a 16:9
// frame by pillarboxing. The content columns hold exactly on the pinned
// ffmpeg version, and a stretched render would fill the whole width instead.
func TestRenderPadsDifferentAspectRatio(t *testing.T) {
	requireFFmpeg(t)
	img := extractFrame(t, render(t, Config{Width: 320, Height: 180}, fixturePath(t, "still.png")))
	bounds := img.Bounds()
	if bounds.Dx() != 320 || bounds.Dy() != 180 {
		t.Fatalf("frame = %dx%d, want 320x180", bounds.Dx(), bounds.Dy())
	}

	// The 640x480 still scales to 240x180 inside the frame, which leaves
	// forty columns of pad colour on each side.
	first, last := -1, -1
	for x := bounds.Min.X; x < bounds.Max.X; x++ {
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			if isRed(pixel(img, x, y)) {
				if first < 0 {
					first = x
				}
				last = x
				break
			}
		}
	}
	if first != 40 || last != 279 {
		t.Errorf("content spans columns %d to %d, want 40 to 279", first, last)
	}
	if r, g, b := pixel(img, 10, 5); !isBlack(r, g, b) {
		t.Errorf("left bar pixel = (%d, %d, %d), want black", r, g, b)
	}
	if r, g, b := pixel(img, 310, 175); !isBlack(r, g, b) {
		t.Errorf("right bar pixel = (%d, %d, %d), want black", r, g, b)
	}
}

// TestRenderCopiesAudioCodec checks that the audio stream inside the render
// carries the codec of the input file. A stream copied into the container
// keeps pcm_s16le, while any encode would report a lossy codec instead.
func TestRenderCopiesAudioCodec(t *testing.T) {
	requireFFmpeg(t)
	out := render(t, Config{Width: 320, Height: 180}, fixturePath(t, "still.png"))
	ctx := context.Background()
	in, err := ffmpeg.AudioFormat(ctx, ffmpeg.Tools{}, fixturePath(t, "tone.wav"))
	if err != nil {
		t.Fatalf("probe input: %v", err)
	}
	got, err := ffmpeg.AudioFormat(ctx, ffmpeg.Tools{}, out)
	if err != nil {
		t.Fatalf("probe output: %v", err)
	}
	if got.Codec != in.Codec {
		t.Errorf("output codec = %s, want the input codec %s", got.Codec, in.Codec)
	}
	if got.SampleRate != in.SampleRate || got.Channels != in.Channels {
		t.Errorf("output shape = %d Hz over %d channels, want %d Hz over %d",
			got.SampleRate, got.Channels, in.SampleRate, in.Channels)
	}
}

// TestRenderDefaultFrameSize checks that the zero Config renders the
// documented 1920 by 1080 frame.
func TestRenderDefaultFrameSize(t *testing.T) {
	requireFFmpeg(t)
	res := probe(t, render(t, Config{}, fixturePath(t, "still.png")))
	if len(res.Streams) == 0 {
		t.Fatal("probe reported no streams")
	}
	video := res.Streams[0]
	if video.Width != 1920 || video.Height != 1080 {
		t.Errorf("default frame = %dx%d, want 1920x1080", video.Width, video.Height)
	}
}

// TestRenderWaveColorParameter checks that the wave colour is the one the
// caller set. The still is black, so the waveform is the only thing drawn.
func TestRenderWaveColorParameter(t *testing.T) {
	requireFFmpeg(t)
	black := filepath.Join(t.TempDir(), "black.png")
	err := ffmpeg.Run(context.Background(), ffmpeg.Tools{},
		"-y", "-f", "lavfi", "-i", "color=c=black:s=640x480", "-frames:v", "1", black)
	if err != nil {
		t.Fatalf("write black still: %v", err)
	}

	red := extractFrame(t, render(t, Config{Width: 320, Height: 180, WaveColor: "red"}, black))
	if n := countPixels(red, isRed); n == 0 {
		t.Error("red wave drew no red pixels")
	}
	if n := countPixels(red, isWhite); n != 0 {
		t.Errorf("red wave drew %d white pixels, want none", n)
	}

	white := extractFrame(t, render(t, Config{Width: 320, Height: 180, WaveColor: "white"}, black))
	if n := countPixels(white, isWhite); n == 0 {
		t.Error("white wave drew no white pixels")
	}
}

// TestRenderPadColorParameter checks that the pad colour fills the bars the
// padding adds.
func TestRenderPadColorParameter(t *testing.T) {
	requireFFmpeg(t)
	img := extractFrame(t, render(t, Config{Width: 320, Height: 180, PadColor: "green"},
		fixturePath(t, "still.png")))
	bounds := img.Bounds()
	green := 0
	for x := 0; x < 36; x++ {
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			if isGreen(pixel(img, x, y)) {
				green++
			}
		}
	}
	// The forty left columns carry the pad colour except where the waveform
	// crosses them, so most of the sampled strip counts green.
	if min := 36 * bounds.Dy() * 7 / 10; green < min {
		t.Errorf("green bar pixels = %d, want at least %d", green, min)
	}
}

// blockingRenderInput listens on a loopback port and returns its URL. ffmpeg
// blocks reading a media format from the socket, which holds the render open
// until the test cancels it. The wait function returns the accepted
// connection, whose close proves the child process died.
func blockingRenderInput(t *testing.T) (url string, wait func() net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		// The goroutine ends when the listener closes or a render connects.
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	wait = func() net.Conn {
		t.Helper()
		select {
		case conn := <-accepted:
			return conn
		case <-time.After(10 * time.Second):
			t.Fatal("ffmpeg did not connect to the blocking input")
			return nil
		}
	}
	return "tcp://" + listener.Addr().String(), wait
}

// assertPeerGone checks that the blocking input closed once the context
// ended, which shows the child process was killed.
func assertPeerGone(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("ffmpeg connection stayed open after the context ended")
	}
}

// TestRenderCancelledContextStopsChild checks that a cancelled context
// kills the render and that the connected socket closes with the child.
func TestRenderCancelledContextStopsChild(t *testing.T) {
	requireFFmpeg(t)
	url, waitConn := blockingRenderInput(t)
	out := filepath.Join(t.TempDir(), "out.mkv")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		// The goroutine ends when Render returns, which the cancel below
		// forces by killing the child.
		done <- Render(ctx, ffmpeg.Tools{}, Config{Width: 320, Height: 180},
			url, fixturePath(t, "tone.wav"), out)
	}()
	conn := waitConn()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("render error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Render did not return after the context ended")
	}
	assertPeerGone(t, conn)
}

// TestRenderReportsMissingBinary checks that a Tools value pointing at a
// path that does not exist reports ErrNotFound.
func TestRenderReportsMissingBinary(t *testing.T) {
	tools := ffmpeg.Tools{FFmpeg: filepath.Join(t.TempDir(), "absent")}
	err := Render(context.Background(), tools, Config{},
		"still.png", "tone.wav", filepath.Join(t.TempDir(), "out.mkv"))
	if !errors.Is(err, ffmpeg.ErrNotFound) {
		t.Fatalf("error = %v, want ffmpeg.ErrNotFound", err)
	}
}

// TestRenderRejectsOddFrameSize checks that an odd width and an odd height
// both report ErrOddFrameSize. The Tools value points at a binary that does
// not exist, so a report of ffmpeg.ErrNotFound would mean the check let a
// process start. The output file must never appear.
func TestRenderRejectsOddFrameSize(t *testing.T) {
	tools := ffmpeg.Tools{FFmpeg: filepath.Join(t.TempDir(), "absent")}
	out := filepath.Join(t.TempDir(), "out.mkv")
	t.Run("odd width", func(t *testing.T) {
		err := Render(context.Background(), tools, Config{Width: 321, Height: 180},
			"still.png", "tone.wav", out)
		if !errors.Is(err, ErrOddFrameSize) {
			t.Fatalf("Render(Width: 321) error = %v, want ErrOddFrameSize", err)
		}
	})
	t.Run("odd height", func(t *testing.T) {
		err := Render(context.Background(), tools, Config{Width: 320, Height: 181},
			"still.png", "tone.wav", out)
		if !errors.Is(err, ErrOddFrameSize) {
			t.Fatalf("Render(Height: 181) error = %v, want ErrOddFrameSize", err)
		}
	})
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("rejected config still wrote %s, stat error = %v", out, statErr)
	}
}
