// Package waveform renders a still image and an audio file into one video
// with the waveform of that audio drawn over the still.
//
// One ffmpeg invocation builds the whole video. The still is scaled to fit
// the output frame and padded out to it, so a still of another aspect ratio
// keeps its shape rather than stretching. The audio stream is copied into
// the output container and never passes an encoder, so the output path must
// name a container that carries the input codec. Matroska carries every
// common one.
//
// Every render carries the caller context into the command, so a cancelled
// context stops the child process.
package waveform

import (
	"context"
	"errors"
	"fmt"

	"github.com/nrynss/keel/ffmpeg"
)

// ErrOddFrameSize reports a Config.Width or Config.Height above zero that is
// odd. The yuv420p output stores colour in pixel pairs, so an odd value
// cannot render at the size the caller asked for. A caller that wants to
// correct its config matches it with errors.Is.
var ErrOddFrameSize = errors.New("waveform: frame size must be even")

// Default frame width when Config.Width is not set.
const defaultWidth = 1920

// Default frame height when Config.Height is not set.
const defaultHeight = 1080

// Default waveform colour when Config.WaveColor is not set.
const defaultWaveColor = "white"

// Default pad colour when Config.PadColor is not set.
const defaultPadColor = "black"

// Config carries the frame size and the two colours of one render. The zero
// value draws a white waveform over a 1920 by 1080 frame padded with black.
type Config struct {
	// Width is the output frame width in pixels. The value must be even,
	// because the yuv420p output stores colour in pixel pairs. An odd value
	// reports ErrOddFrameSize. Zero or less means 1920.
	Width int
	// Height is the output frame height in pixels. The value must be even,
	// because the yuv420p output stores colour in pixel pairs. An odd value
	// reports ErrOddFrameSize. Zero or less means 1080.
	Height int
	// WaveColor is the colour the waveform is drawn in. Any colour name
	// ffmpeg accepts works. Empty means white.
	WaveColor string
	// PadColor fills the bars the padding adds. Empty means black.
	PadColor string
}

// width returns the frame width, defaulting to 1920 when unset.
func (c Config) width() int {
	if c.Width <= 0 {
		return defaultWidth
	}
	return c.Width
}

// height returns the frame height, defaulting to 1080 when unset.
func (c Config) height() int {
	if c.Height <= 0 {
		return defaultHeight
	}
	return c.Height
}

// waveColor returns the waveform colour, defaulting to white when unset.
func (c Config) waveColor() string {
	if c.WaveColor == "" {
		return defaultWaveColor
	}
	return c.WaveColor
}

// padColor returns the pad colour, defaulting to black when unset.
func (c Config) padColor() string {
	if c.PadColor == "" {
		return defaultPadColor
	}
	return c.PadColor
}

// Render draws the audio waveform over the still image and writes one video
// to out. The frame size and both colours come from cfg. An odd cfg.Width
// or cfg.Height reports ErrOddFrameSize before any process starts.
//
// The audio stream is copied, never encoded, so out must name a container
// that carries the input codec. Matroska carries every common one. The
// output duration equals the input duration for PCM audio. A copied lossy
// stream adds the priming window of its codec, for example 23 milliseconds
// for AAC at 44100 Hz. A missing binary reports ffmpeg.ErrNotFound and a
// failed run reports ffmpeg.ErrFailed. The context kills the child when it
// ends.
func Render(ctx context.Context, tools ffmpeg.Tools, cfg Config, still, audio, out string) error {
	if cfg.Width > 0 && cfg.Width%2 != 0 {
		return fmt.Errorf("width %d: %w", cfg.Width, ErrOddFrameSize)
	}
	if cfg.Height > 0 && cfg.Height%2 != 0 {
		return fmt.Errorf("height %d: %w", cfg.Height, ErrOddFrameSize)
	}
	w, h := cfg.width(), cfg.height()
	// The still fits the frame along its longer side and the pad filter
	// fills the rest, so the content keeps its shape. showwavespic draws
	// the whole waveform as one picture of the frame size, and overlay
	// composites it over the padded still.
	graph := fmt.Sprintf(
		"[0:v]scale=%d:%d:force_original_aspect_ratio=decrease,"+
			"pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=%s[bg];"+
			"[1:a]showwavespic=s=%dx%d:colors=%s[wave];"+
			"[bg][wave]overlay,format=yuv420p[v]",
		w, h, w, h, cfg.padColor(), w, h, cfg.waveColor(),
	)
	// The audio maps straight through with copy, so its bytes pass one mux
	// and no encoder.
	args := []string{
		"-y",
		"-i", still,
		"-i", audio,
		"-filter_complex", graph,
		"-map", "[v]",
		"-map", "1:a",
		"-c:v", "libx264",
		"-c:a", "copy",
		out,
	}
	if err := ffmpeg.Run(ctx, tools, args...); err != nil {
		return fmt.Errorf("waveform render %s: %w", out, err)
	}
	return nil
}
