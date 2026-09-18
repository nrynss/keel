package edl_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/nrynss/keel/edl"
	"github.com/nrynss/keel/ffmpeg"
)

// fixturePath returns the path to a committed audio fixture.
func fixturePath(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("../testdata/audio", name)
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

// writeSine renders a sine tone of the given length into a temporary WAV, so
// a test can cut it without a committed fixture that long.
func writeSine(t *testing.T, d time.Duration) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sine.wav")
	err := ffmpeg.Run(t.Context(), ffmpeg.Tools{}, "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration="+strconv.Itoa(int(d/time.Second)),
		"-c:a", "pcm_s16le", "-ac", "1", path)
	if err != nil {
		t.Fatalf("write sine fixture: %v", err)
	}
	return path
}

// TestRenderDurationMatchesCutList renders the sine fixture with several cut
// lists and probes each result with ffprobe. CI and the gate both run one
// pinned ffmpeg version, so the duration asserts exact millisecond equality
// with the kept ranges minus the crossfade overlaps.
func TestRenderDurationMatchesCutList(t *testing.T) {
	requireFFmpeg(t)
	long := writeSine(t, 12*time.Second)

	tests := []struct {
		name      string
		src       string
		crossfade time.Duration
		segments  []edl.Segment
		wantMs    int64
	}{
		{
			name:     "hard cuts",
			src:      fixturePath(t, "tone-48k.wav"),
			segments: []edl.Segment{{Start: 0, End: 1500 * time.Millisecond}, {Start: 2 * time.Second, End: 3 * time.Second}},
			wantMs:   2500,
		},
		{
			name:      "one crossfade",
			src:       fixturePath(t, "tone-48k.wav"),
			crossfade: 250 * time.Millisecond,
			segments:  []edl.Segment{{Start: 0, End: 1500 * time.Millisecond}, {Start: 2 * time.Second, End: 3 * time.Second}},
			wantMs:    2250,
		},
		{
			name:      "one range with crossfade set",
			src:       fixturePath(t, "tone-48k.wav"),
			crossfade: 250 * time.Millisecond,
			segments:  []edl.Segment{{Start: 500 * time.Millisecond, End: 2 * time.Second}},
			wantMs:    1500,
		},
		{
			name:      "faded and cut joins",
			src:       long,
			crossfade: 500 * time.Millisecond,
			segments: []edl.Segment{
				{Start: 0, End: 1500 * time.Millisecond},
				{Start: 2 * time.Second, End: 3 * time.Second},
				{Start: 3200 * time.Millisecond, End: 3500 * time.Millisecond},
			},
			wantMs: 2300,
		},
		{
			name:      "unsorted duplicates merge away",
			src:       fixturePath(t, "tone-48k.wav"),
			crossfade: 250 * time.Millisecond,
			segments: []edl.Segment{
				{Start: 2 * time.Second, End: 3 * time.Second},
				{Start: 0, End: 1500 * time.Millisecond},
				{Start: 2 * time.Second, End: 3 * time.Second},
				{Start: 0, End: 2 * time.Second},
			},
			wantMs: 3000,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "cut.wav")
			err := edl.Render(t.Context(), ffmpeg.Tools{}, tt.src,
				edl.Config{Crossfade: tt.crossfade, Target: -16}, out, tt.segments...)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			got, err := ffmpeg.Duration(t.Context(), ffmpeg.Tools{}, out)
			if err != nil {
				t.Fatalf("probe rendered file: %v", err)
			}
			if want := time.Duration(tt.wantMs) * time.Millisecond; got != want {
				t.Fatalf("rendered duration = %v, want exactly %v", got, want)
			}
		})
	}
}

// measureFile measures one rendered file the way the loudnorm documentation
// describes a measurement. It runs ffmpeg on its own, so the pin shares no
// code with the render it checks, and returns the print_format values the
// tool reported.
func measureFile(t *testing.T, path string) map[string]string {
	t.Helper()
	stats := filepath.Join(t.TempDir(), "stats.json")
	cmd := ffmpeg.Command(t.Context(), "ffmpeg",
		"-i", path, "-af", "loudnorm=print_format=json:stats_file="+stats, "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("measure %s: %v: %s", path, err, stderr.String())
	}
	raw, err := os.ReadFile(stats)
	if err != nil {
		t.Fatalf("read stats: %v", err)
	}
	var parsed map[string]string
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode stats %s: %v", raw, err)
	}
	return parsed
}

// TestRenderLoudnessMatchesTarget renders a cut at a -16 LUFS target and
// measures the rendered file with an independent loudnorm run. The measured
// values carry two decimals. The tool reports its own residual target offset
// of 0.05 LU on these fixtures, so the pin allows 0.5 LU, ten times that
// residual. A normalisation that misses by more fails.
func TestRenderLoudnessMatchesTarget(t *testing.T) {
	requireFFmpeg(t)
	out := filepath.Join(t.TempDir(), "cut.wav")
	err := edl.Render(t.Context(), ffmpeg.Tools{}, fixturePath(t, "tone-48k.wav"),
		edl.Config{Crossfade: 250 * time.Millisecond, Target: -16}, out,
		edl.Segment{Start: 0, End: 1500 * time.Millisecond},
		edl.Segment{Start: 2 * time.Second, End: 3 * time.Second})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	stats := measureFile(t, out)
	if stats["normalization_type"] != "linear" {
		t.Errorf("normalization_type = %q, want linear", stats["normalization_type"])
	}
	got, err := strconv.ParseFloat(stats["input_i"], 64)
	if err != nil {
		t.Fatalf("parse measured loudness %q: %v", stats["input_i"], err)
	}
	const target = -16.0
	if off := math.Abs(got - target); off > 0.5 {
		t.Errorf("measured loudness = %.2f LUFS, want %.1f within 0.5 LU, off by %.2f", got, target, off)
	}
}

// TestRenderKeepsSourceFormat checks the restore stage. loudnorm works at
// 192 kHz inside, and the render must hand back the sample rate and channel
// layout of the source.
func TestRenderKeepsSourceFormat(t *testing.T) {
	requireFFmpeg(t)
	out := filepath.Join(t.TempDir(), "cut.wav")
	err := edl.Render(t.Context(), ffmpeg.Tools{}, fixturePath(t, "tone-48k.wav"),
		edl.Config{Target: -16}, out,
		edl.Segment{Start: 0, End: 1500 * time.Millisecond},
		edl.Segment{Start: 2 * time.Second, End: 3 * time.Second})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	format, err := ffmpeg.AudioFormat(t.Context(), ffmpeg.Tools{}, out)
	if err != nil {
		t.Fatalf("probe rendered file: %v", err)
	}
	if format.SampleRate != 48000 || format.Channels != 1 || format.ChannelLayout != "mono" {
		t.Fatalf("rendered format = %+v, want 48000 Hz mono", format)
	}
}

// TestRenderCancelledStopsTheChild starts a render whose source never
// delivers bytes, cancels the context, and pins that the child is gone by
// reading the connection the child held. A closed peer means a dead process.
func TestRenderCancelledStopsTheChild(t *testing.T) {
	requireFFmpeg(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		// The goroutine ends when the listener closes or the probe connects.
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		// The goroutine ends when Render returns.
		done <- edl.Render(ctx, ffmpeg.Tools{}, "tcp://"+listener.Addr().String(),
			edl.Config{}, filepath.Join(t.TempDir(), "cut.wav"),
			edl.Segment{Start: 0, End: time.Second})
	}()

	var child net.Conn
	select {
	case child = <-accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("no child connected to the blocking source")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("render error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("render still running after cancellation")
	}

	child.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := child.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("child connection read = %v, want EOF from a dead child", err)
	}
}

// TestRenderRejectsInvalidInput checks the input guards. Every guard fires
// before any subprocess starts, so these run without ffmpeg installed.
func TestRenderRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name     string
		cfg      edl.Config
		dst      string
		segments []edl.Segment
		want     error
	}{
		{"no segments", edl.Config{}, "cut.wav", nil, edl.ErrNoSegments},
		{"end before start", edl.Config{}, "cut.wav", []edl.Segment{{Start: 2 * time.Second, End: time.Second}}, edl.ErrInvalidSegment},
		{"zero length", edl.Config{}, "cut.wav", []edl.Segment{{Start: time.Second, End: time.Second}}, edl.ErrInvalidSegment},
		{"negative start", edl.Config{}, "cut.wav", []edl.Segment{{Start: -time.Second, End: time.Second}}, edl.ErrInvalidSegment},
		{"no destination", edl.Config{}, "", []edl.Segment{{Start: 0, End: time.Second}}, edl.ErrNoDestination},
		{"target above range", edl.Config{Target: -3}, "cut.wav", []edl.Segment{{Start: 0, End: time.Second}}, edl.ErrInvalidTarget},
		{"target below range", edl.Config{Target: -80}, "cut.wav", []edl.Segment{{Start: 0, End: time.Second}}, edl.ErrInvalidTarget},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := edl.Render(t.Context(), ffmpeg.Tools{}, "src.wav", tt.cfg, tt.dst, tt.segments...)
			if !errors.Is(err, tt.want) {
				t.Fatalf("render error = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestRenderReportsMissingBinary checks that a Tools value pointing at a
// path that does not exist surfaces ffmpeg.ErrNotFound from the probe.
func TestRenderReportsMissingBinary(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-binary")
	err := edl.Render(t.Context(), ffmpeg.Tools{FFmpeg: missing, FFprobe: missing},
		fixturePath(t, "tone-48k.wav"), edl.Config{},
		filepath.Join(t.TempDir(), "cut.wav"),
		edl.Segment{Start: 0, End: time.Second})
	if !errors.Is(err, ffmpeg.ErrNotFound) {
		t.Fatalf("render error = %v, want ffmpeg.ErrNotFound", err)
	}
}
