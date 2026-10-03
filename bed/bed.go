// Package bed mixes a looping music bed under a finished video.
//
// The film's length is known before the mix. The end fade starts that length
// minus the fade and reaches silence at the film's end. A bed shorter than
// the film loops. A longer bed is trimmed by the mix, which follows the
// film's audio. The video stream is copied. Only the audio is encoded again.
//
// The filter pins are part of the contract. The bed is gained and faded on
// its own chain. The mix does not normalise, because the default mix halves
// the narration the moment a second input appears. The film's audio is the
// timeline.
package bed

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nrynss/keel/ffmpeg"
)

// ErrInvalid reports a mix whose paths or timing cannot be used. An empty
// path, a non-positive film duration, and a fade at least as long as the
// film all match it with errors.Is.
var ErrInvalid = errors.New("bed: invalid mix")

// DefaultGain is the bed's volume when Config.Gain is unset. It sits under
// the film's own audio.
const DefaultGain = 0.15

// DefaultFade is the end fade when Config.Fade is unset.
const DefaultFade = 2 * time.Second

// DefaultBitrate is the encoded audio bitrate when Config.Bitrate is unset.
const DefaultBitrate = "128k"

// Config carries the bed's gain, the end fade, and the audio bitrate. The
// zero value is usable.
type Config struct {
	// Gain is the bed's volume. Zero or negative means DefaultGain. A mute
	// is not a mix, so zero cannot mean silence.
	Gain float64
	// Fade is the end fade-out. Zero or negative means DefaultFade. A fade
	// at least as long as the film reports ErrInvalid.
	Fade time.Duration
	// Bitrate is the encoded audio bitrate ffmpeg accepts, such as "128k".
	// Empty means DefaultBitrate.
	Bitrate string
}

// Input names the finished film, the bed, and the output of one mix.
type Input struct {
	// Film is the finished video. Its audio is the timeline.
	Film string
	// Bed is the music file mixed under Film.
	Bed string
	// Duration is Film's length, known before the mix. It must be positive.
	// The fade is anchored to it, never to a probe of the file.
	Duration time.Duration
	// Output is where the mixed file is written.
	Output string
}

// gain returns the bed volume, substituting the default for an unset or
// negative value.
func (c Config) gain() float64 {
	if c.Gain <= 0 {
		return DefaultGain
	}
	return c.Gain
}

// fade returns the end fade, substituting the default for an unset or
// negative value.
func (c Config) fade() time.Duration {
	if c.Fade <= 0 {
		return DefaultFade
	}
	return c.Fade
}

// bitrate returns the audio bitrate, substituting the default when unset.
func (c Config) bitrate() string {
	if c.Bitrate == "" {
		return DefaultBitrate
	}
	return c.Bitrate
}

// Mix writes the bed under in.Film to in.Output. tools names ffmpeg. A
// missing binary reports ffmpeg.ErrNotFound and a failed run reports
// ffmpeg.ErrFailed. The context kills the child when it ends.
func Mix(ctx context.Context, tools ffmpeg.Tools, cfg Config, in Input) error {
	if strings.TrimSpace(in.Film) == "" || strings.TrimSpace(in.Bed) == "" || strings.TrimSpace(in.Output) == "" {
		return fmt.Errorf("%w: mix needs a film path, a bed path, and an output path", ErrInvalid)
	}
	if in.Duration <= 0 {
		return fmt.Errorf("%w: film duration %s must be positive", ErrInvalid, in.Duration)
	}
	fade := cfg.fade()
	if fade >= in.Duration {
		return fmt.Errorf("%w: fade %s must be shorter than the film %s", ErrInvalid, fade, in.Duration)
	}
	gain := cfg.gain()
	start := in.Duration.Seconds() - fade.Seconds()
	filter := "[1:a]volume=" + strconv.FormatFloat(gain, 'f', -1, 64) +
		",afade=t=out:st=" + strconv.FormatFloat(start, 'f', 3, 64) +
		":d=" + strconv.FormatFloat(fade.Seconds(), 'f', 3, 64) +
		"[bed];[0:a][bed]amix=inputs=2:duration=first:normalize=0[a]"
	args := []string{
		"-i", in.Film,
		"-stream_loop", "-1",
		"-i", in.Bed,
		"-filter_complex", filter,
		"-map", "0:v",
		"-map", "[a]",
		"-c:v", "copy",
		"-c:a", "aac",
		"-b:a", cfg.bitrate(),
		"-movflags", "+faststart",
		"-y",
		in.Output,
	}
	if err := ffmpeg.Run(ctx, tools, args...); err != nil {
		return fmt.Errorf("bed mix %s: %w", in.Output, err)
	}
	return nil
}
