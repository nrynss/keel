package ffmpeg

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// audioDir holds the committed audio fixtures, relative to the package dir.
const audioDir = "../testdata/audio"

// fixturePath returns the path to a committed audio fixture.
func fixturePath(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(audioDir, name)
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

// TestDurationMatchesFixtures checks the length ffprobe measures for every
// fixture against the value recorded in testdata/audio/README.md. CI and the
// gate both run one pinned ffmpeg version, so every fixture asserts exact
// millisecond equality.
func TestDurationMatchesFixtures(t *testing.T) {
	requireFFmpeg(t)
	tests := []struct {
		name string
		ms   int64
	}{
		{name: "tone-48k.wav", ms: 3000},
		{name: "tone-24k.wav", ms: 3000},
		{name: "tone-48k.opus", ms: 3007}, // README records 3.006500 s, rounded to 3007 ms
		{name: "tone-48k.mp3", ms: 3000},
		{name: "truncated.wav", ms: 1194}, // README records 1.194208 s, rounded to 1194 ms
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Duration(t.Context(), Tools{}, fixturePath(t, tc.name))
			if err != nil {
				t.Fatalf("Duration(%s) failed: %v", tc.name, err)
			}
			if got.Milliseconds() != tc.ms {
				t.Errorf("Duration(%s) = %d ms, want %d ms", tc.name, got.Milliseconds(), tc.ms)
			}
		})
	}
}

// TestDurationRejectsUnreadableFixture checks the fixture whose bytes carry no
// audio. ffprobe cannot probe it, so Duration reports ErrFailed.
func TestDurationRejectsUnreadableFixture(t *testing.T) {
	requireFFmpeg(t)
	_, err := Duration(t.Context(), Tools{}, fixturePath(t, "random-20m.bin"))
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("Duration(random-20m.bin) error = %v, want ErrFailed", err)
	}
}

// stubFFprobe writes an executable that answers a format=duration entry with
// format and a stream=duration entry with stream. A container that omits its
// duration is rare, so a stub lets the fallback path run on demand.
func stubFFprobe(t *testing.T, format, stream string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffprobe")
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  case \"$arg\" in\n" +
		"    format=duration) echo " + format + "; exit 0 ;;\n" +
		"    stream=duration) echo " + stream + "; exit 0 ;;\n" +
		"  esac\n" +
		"done\n" +
		"exit 2\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub ffprobe: %v", err)
	}
	return path
}

// TestDurationFallsBackToStream checks that Duration reads the first audio
// stream when the container reports no duration, and reports a missing
// duration when neither source has one.
func TestDurationFallsBackToStream(t *testing.T) {
	t.Run("stream_supplies_the_duration", func(t *testing.T) {
		got, err := Duration(t.Context(), Tools{FFprobe: stubFFprobe(t, "N/A", "1.500000")}, "ignored")
		if err != nil {
			t.Fatalf("Duration failed: %v", err)
		}
		if want := 1500 * time.Millisecond; got != want {
			t.Errorf("Duration = %v, want %v", got, want)
		}
	})
	t.Run("neither_source_has_one", func(t *testing.T) {
		_, err := Duration(t.Context(), Tools{FFprobe: stubFFprobe(t, "N/A", "N/A")}, "ignored")
		if err == nil {
			t.Fatal("Duration succeeded with no duration in either source")
		}
	})
}

// TestAudioFormatMatchesFixtures checks the audio stream ffprobe measures for
// every fixture against the value recorded in testdata/audio/README.md.
func TestAudioFormatMatchesFixtures(t *testing.T) {
	requireFFmpeg(t)
	tests := []struct {
		name       string
		codec      string
		sampleRate int
		channels   int
		layout     string
		bitRate    int64
	}{
		{name: "tone-48k.wav", codec: "pcm_s16le", sampleRate: 48000, channels: 1, layout: "mono", bitRate: 768000},
		{name: "tone-24k.wav", codec: "pcm_s16le", sampleRate: 24000, channels: 1, layout: "mono", bitRate: 384000},
		{name: "tone-48k.opus", codec: "opus", sampleRate: 48000, channels: 1, layout: "mono", bitRate: 78217},
		{name: "tone-48k.mp3", codec: "mp3", sampleRate: 48000, channels: 1, layout: "mono", bitRate: 128000},
		{name: "truncated.wav", codec: "pcm_s16le", sampleRate: 48000, channels: 1, layout: "mono", bitRate: 768000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			format, err := AudioFormat(t.Context(), Tools{}, fixturePath(t, tc.name))
			if err != nil {
				t.Fatalf("AudioFormat(%s) failed: %v", tc.name, err)
			}
			if format.Codec != tc.codec {
				t.Errorf("Codec = %q, want %q", format.Codec, tc.codec)
			}
			if format.SampleRate != tc.sampleRate {
				t.Errorf("SampleRate = %d, want %d", format.SampleRate, tc.sampleRate)
			}
			if format.Channels != tc.channels {
				t.Errorf("Channels = %d, want %d", format.Channels, tc.channels)
			}
			if format.ChannelLayout != tc.layout {
				t.Errorf("ChannelLayout = %q, want %q", format.ChannelLayout, tc.layout)
			}
			if format.BitRate != tc.bitRate {
				t.Errorf("BitRate = %d, want %d", format.BitRate, tc.bitRate)
			}
		})
	}
}

// TestAudioFormatRejectsUnreadableFixture checks that a file with no audio
// stream reports ErrFailed rather than an empty Format.
func TestAudioFormatRejectsUnreadableFixture(t *testing.T) {
	requireFFmpeg(t)
	_, err := AudioFormat(t.Context(), Tools{}, fixturePath(t, "random-20m.bin"))
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("AudioFormat(random-20m.bin) error = %v, want ErrFailed", err)
	}
}

// TestRunReportsFailure checks that a non-zero exit from ffmpeg surfaces as
// ErrFailed.
func TestRunReportsFailure(t *testing.T) {
	requireFFmpeg(t)
	err := Run(t.Context(), Tools{}, "-invalid_option_unknown_flag")
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("Run error = %v, want ErrFailed", err)
	}
}

// TestMissingBinaryReturnsErrNotFound checks that a Tools value pointing at a
// path that does not exist reports ErrNotFound at every entry point.
func TestMissingBinaryReturnsErrNotFound(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-binary")
	t.Run("run", func(t *testing.T) {
		err := Run(t.Context(), Tools{FFmpeg: missing}, "-version")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Run error = %v, want ErrNotFound", err)
		}
	})
	t.Run("duration", func(t *testing.T) {
		_, err := Duration(t.Context(), Tools{FFprobe: missing}, fixturePath(t, "tone-48k.wav"))
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Duration error = %v, want ErrNotFound", err)
		}
	})
	t.Run("audio_format", func(t *testing.T) {
		_, err := AudioFormat(t.Context(), Tools{FFprobe: missing}, fixturePath(t, "tone-48k.wav"))
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("AudioFormat error = %v, want ErrNotFound", err)
		}
	})
}

// TestRunCancelsLongEncode starts an encode that would run for a long time,
// cancels the context, and checks that Run returns within waitDelay, measured
// from the moment of cancellation.
func TestRunCancelsLongEncode(t *testing.T) {
	requireFFmpeg(t)
	out := filepath.Join(t.TempDir(), "long.wav")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		// -re reads the synthetic input at its native rate, so the encode
		// only finishes when the context ends.
		done <- Run(ctx, Tools{}, "-y", "-re", "-f", "lavfi", "-i", "sine=frequency=440",
			"-c:a", "pcm_s16le", out)
	}()
	waitForFile(t, out)

	cancel()
	start := time.Now()
	select {
	case err := <-done:
		elapsed := time.Since(start)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
		if elapsed > waitDelay {
			t.Errorf("Run returned %v after cancellation, want within %v", elapsed, waitDelay)
		}
	case <-time.After(2 * waitDelay):
		t.Fatalf("Run still running %v after cancellation", 2*waitDelay)
	}
}

// waitForFile waits until path exists so a cancel never races the encode
// start.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("encode never created %s", path)
}

// TestStderrTailKeepsLastBytes checks that the error capture stays bounded and
// keeps the newest output, which is where ffmpeg states its failure.
func TestStderrTailKeepsLastBytes(t *testing.T) {
	var buf tailBuffer
	if _, err := buf.Write([]byte(strings.Repeat("a", maxStderr))); err != nil {
		t.Fatalf("write first chunk: %v", err)
	}
	if _, err := buf.Write([]byte(strings.Repeat("b", maxStderr))); err != nil {
		t.Fatalf("write second chunk: %v", err)
	}
	if _, err := buf.Write([]byte("boom")); err != nil {
		t.Fatalf("write tail: %v", err)
	}
	got := buf.String()
	if len(got) != maxStderr {
		t.Fatalf("retained length = %d, want %d", len(got), maxStderr)
	}
	if !strings.HasSuffix(got, "boom") {
		t.Errorf("retained bytes do not end with the last write")
	}
	if strings.Contains(got, "a") {
		t.Errorf("retained bytes still hold the dropped first chunk")
	}
}

// blockingProbeInput listens on a random loopback port and never writes. It
// returns a probe URL and a wait function for the incoming connection.
func blockingProbeInput(t *testing.T) (string, func() net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		// The goroutine ends when the listener closes or a probe connects.
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	wait := func() net.Conn {
		t.Helper()
		select {
		case conn := <-accepted:
			return conn
		case <-time.After(10 * time.Second):
			t.Fatal("ffprobe did not connect to the blocking input")
			return nil
		}
	}
	return "tcp://" + listener.Addr().String(), wait
}

// waitProbeResult waits for a Duration call to return after the context ended.
func waitProbeResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Duration did not return after the context ended")
		return nil
	}
}

// assertPeerGone checks that ffprobe's connection closed once the context
// ended, which shows the child process was killed.
func assertPeerGone(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("ffprobe connection stayed open after the context ended")
	}
}

// TestDurationCancelsRunningProbe checks that a cancelled context kills a
// running ffprobe and returns its context error.
func TestDurationCancelsRunningProbe(t *testing.T) {
	requireFFmpeg(t)
	url, wait := blockingProbeInput(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Duration(ctx, Tools{}, url)
		done <- err
	}()
	conn := wait()
	defer conn.Close()

	start := time.Now()
	cancel()
	err := waitProbeResult(t, done)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Duration error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > waitDelay {
		t.Fatalf("Duration waited %v after cancellation", elapsed)
	}
	assertPeerGone(t, conn)
}

// TestDurationHonoursDeadline checks that an elapsed deadline kills a running
// ffprobe and returns its context error.
func TestDurationHonoursDeadline(t *testing.T) {
	requireFFmpeg(t)
	url, wait := blockingProbeInput(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Duration(ctx, Tools{}, url)
		done <- err
	}()
	conn := wait()
	defer conn.Close()

	err := waitProbeResult(t, done)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Duration error = %v, want context.DeadlineExceeded", err)
	}
	assertPeerGone(t, conn)
}
