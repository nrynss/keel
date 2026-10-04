package bed

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nrynss/keel/ffmpeg"
)

func TestMixRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	tools := ffmpeg.Tools{FFmpeg: "ffmpeg"}
	base := Input{Film: "film.mp4", Bed: "bed.mp3", Duration: 10 * time.Second, Output: "out.mp4"}
	cases := []Input{
		{Bed: "bed.mp3", Duration: time.Second, Output: "out.mp4"},
		{Film: "film.mp4", Duration: time.Second, Output: "out.mp4"},
		{Film: "film.mp4", Bed: "bed.mp3", Duration: time.Second},
		{Film: "film.mp4", Bed: "bed.mp3", Output: "out.mp4"},
		{Film: "film.mp4", Bed: "bed.mp3", Duration: -time.Second, Output: "out.mp4"},
	}
	for _, in := range cases {
		err := Mix(ctx, tools, Config{}, in)
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("Mix(%+v) = %v, want ErrInvalid", in, err)
		}
	}
	err := Mix(ctx, tools, Config{Fade: 10 * time.Second}, base)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("long fade = %v, want ErrInvalid", err)
	}
}

func TestMixArgs(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args.txt")
	stub := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(log) + "\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := Mix(context.Background(), ffmpeg.Tools{FFmpeg: stub}, Config{Gain: 0.2, Fade: time.Second, Bitrate: "96k"}, Input{
		Film:     "film.mp4",
		Bed:      "bed.wav",
		Duration: 10 * time.Second,
		Output:   "out.mp4",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	args := string(raw)
	for _, want := range []string{
		"-stream_loop\n-1\n",
		"volume=0.2,afade=t=out:st=9.000:d=1.000",
		"apad=whole_dur=10.000",
		"[film][bed]amix=inputs=2:duration=first:normalize=0",
		"-c:v\ncopy\n",
		"-b:a\n96k\n",
		"-map\n0:v\n",
		"-map\n[a]\n",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("args missing %q:\n%s", want, args)
		}
	}
}

func TestMixDefaults(t *testing.T) {
	if (Config{}).gain() != DefaultGain || (Config{}).fade() != DefaultFade || (Config{}).bitrate() != DefaultBitrate {
		t.Fatal("zero config was not defaulted")
	}
}

func TestMixWithFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		if os.Getenv("KEEL_REQUIRE_FFMPEG") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		if os.Getenv("KEEL_REQUIRE_FFMPEG") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	dir := t.TempDir()
	film := filepath.Join(dir, "film.mp4")
	audio := filepath.Join(dir, "bed.wav")
	out := filepath.Join(dir, "mixed.mp4")
	run(t, "ffmpeg", "-y", "-f", "lavfi", "-i", "color=c=black:s=320x240:r=25:d=3",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=3",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", film)
	run(t, "ffmpeg", "-y", "-f", "lavfi", "-i", "sine=frequency=220:sample_rate=44100:duration=1", audio)
	err := Mix(context.Background(), ffmpeg.Tools{}, Config{Fade: 500 * time.Millisecond}, Input{
		Film: film, Bed: audio, Duration: 3 * time.Second, Output: out,
	})
	if err != nil {
		t.Fatal(err)
	}
	probe := run(t, "ffprobe", "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", out)
	if !strings.Contains(probe, "video") || !strings.Contains(probe, "audio") {
		t.Fatalf("probe = %q", probe)
	}
}

func TestMixPadsShortFilmAudio(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		if os.Getenv("KEEL_REQUIRE_FFMPEG") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		if os.Getenv("KEEL_REQUIRE_FFMPEG") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	dir := t.TempDir()
	film := filepath.Join(dir, "film.mp4")
	audio := filepath.Join(dir, "bed.wav")
	out := filepath.Join(dir, "mixed.mp4")
	// Six seconds of picture, four seconds of film audio. Without padding,
	// amix duration=first ends the mix at 4s and the fade never plays.
	run(t, "ffmpeg", "-y", "-f", "lavfi", "-i", "color=c=black:s=320x240:r=25:d=6",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=4",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", film)
	run(t, "ffmpeg", "-y", "-f", "lavfi", "-i", "sine=frequency=220:sample_rate=44100:duration=2", audio)
	err := Mix(context.Background(), ffmpeg.Tools{}, Config{Gain: 1, Fade: 2 * time.Second}, Input{
		Film: film, Bed: audio, Duration: 6 * time.Second, Output: out,
	})
	if err != nil {
		t.Fatal(err)
	}
	probe := run(t, "ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=duration", "-of", "csv=p=0", out)
	secs, err := strconv.ParseFloat(strings.TrimSpace(probe), 64)
	if err != nil {
		t.Fatalf("probe %q: %v", probe, err)
	}
	if secs < 5.5 {
		t.Fatalf("audio duration = %.3fs, want about 6s", secs)
	}
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", name, err, out)
	}
	return string(out)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
