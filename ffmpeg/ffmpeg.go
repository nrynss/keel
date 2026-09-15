// Package ffmpeg runs the ffmpeg and ffprobe binaries and reads what they
// report about an audio file.
//
// ffmpeg is a runtime binary rather than a Go dependency, so a Tools value
// names the two executables. An empty field resolves the bare name on the
// PATH, which suits a host that installed ffmpeg in the usual place.
//
// Every command is bound to a context. The context kills the child when it
// ends, and a short wait delay bounds how long Wait then blocks on an
// inherited pipe that a killed child left open. Without that bound a kill
// could still hang the caller.
package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"strings"
	"time"
)

// ErrNotFound reports that the configured binary is missing. A caller that
// wants a fallback matches it with errors.Is.
var ErrNotFound = errors.New("ffmpeg: binary not found")

// ErrFailed reports that the binary ran and exited non-zero. The captured
// standard error travels in the returned message.
var ErrFailed = errors.New("ffmpeg: command failed")

// waitDelay bounds how long Wait blocks on an inherited pipe after the
// context kills a child. Five seconds lets ffmpeg flush a partial file while
// shutdown stays prompt.
const waitDelay = 5 * time.Second

// maxStderr is how many bytes of standard error the package keeps. A long
// encode writes progress without bound, and only the last lines describe why
// it stopped.
const maxStderr = 64 << 10

// The bare binary names that a PATH lookup resolves.
const (
	ffmpegName  = "ffmpeg"
	ffprobeName = "ffprobe"
)

// Tools names the ffmpeg and ffprobe executables. The zero value looks both
// up on the PATH. Set a field to pin an absolute path or a specific build.
type Tools struct {
	FFmpeg  string
	FFprobe string
}

// ffmpegPath returns the configured ffmpeg path, or the bare name that the
// PATH lookup resolves when the field is empty.
func (t Tools) ffmpegPath() string {
	if t.FFmpeg != "" {
		return t.FFmpeg
	}
	return ffmpegName
}

// ffprobePath returns the configured ffprobe path, or the bare name that the
// PATH lookup resolves when the field is empty.
func (t Tools) ffprobePath() string {
	if t.FFprobe != "" {
		return t.FFprobe
	}
	return ffprobeName
}

// Command builds a command for the executable at bin bound to ctx. bin is an
// absolute path or a bare name that the PATH lookup resolves. The context
// kills the child when it ends, and waitDelay bounds how long Wait then
// blocks on an inherited pipe.
func Command(ctx context.Context, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = waitDelay
	return cmd
}

// Run executes ffmpeg with args and discards standard output. A missing
// binary returns ErrNotFound, a non-zero exit returns ErrFailed, and a
// cancelled context returns the context error.
func Run(ctx context.Context, tools Tools, args ...string) error {
	return run(ctx, tools.ffmpegPath(), args, nil)
}

// run executes bin with args. A non-nil stdout receives standard output. It
// returns ErrNotFound, ErrFailed or the context error, each wrapped with the
// command line that produced it.
func run(ctx context.Context, bin string, args []string, stdout io.Writer) error {
	cmd := Command(ctx, bin, args...)
	var errOut tailBuffer
	cmd.Stderr = &errOut
	if stdout != nil {
		cmd.Stdout = stdout
	}
	err := cmd.Run()
	if err == nil {
		return nil
	}
	line := bin + " " + strings.Join(args, " ")
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", line, ctxErr)
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", line, ErrNotFound)
	}
	if msg := strings.TrimSpace(errOut.String()); msg != "" {
		return fmt.Errorf("%s failed: %s: %w", line, msg, ErrFailed)
	}
	return fmt.Errorf("%s failed: %w", line, ErrFailed)
}

// tailBuffer keeps the last maxStderr bytes written to it. ffmpeg writes
// progress to standard error without bound, so the buffer drops the oldest
// bytes and keeps the newest, which carry the failure.
type tailBuffer struct {
	buf []byte
}

// Write appends p and drops the oldest bytes past maxStderr. It always
// reports the full length written, because a short write would make the
// child see a closed pipe.
func (b *tailBuffer) Write(p []byte) (int, error) {
	if len(p) >= maxStderr {
		b.buf = append(b.buf[:0], p[len(p)-maxStderr:]...)
		return len(p), nil
	}
	if len(b.buf)+len(p) > maxStderr {
		drop := len(b.buf) + len(p) - maxStderr
		b.buf = append(b.buf[:0], b.buf[drop:]...)
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// String returns the retained bytes.
func (b *tailBuffer) String() string {
	return string(b.buf)
}
