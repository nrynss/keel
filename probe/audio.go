package probe

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/nrynss/keel/ffmpeg"
)

// Format is the shape of the WAV ExtractAudio writes. A zero field keeps the
// source's value, so the zero Format writes the audio at its own rate and
// channel count.
type Format struct {
	// SampleRate is the output rate in hertz. Zero keeps the source rate.
	SampleRate int
	// Channels is the output channel count. Zero keeps the source's count.
	Channels int
}

// ExtractAudio writes the first audio stream at path to out as a 16-bit PCM
// WAV, the working format every audio tool reads, and returns the format it
// wrote. The source's rate and channel count, read as Probe reads them and
// so as ffmpeg.AudioFormat reports them, fill any zero field of f; a field
// the source's report leaves unknown stays zero in the returned Format and
// the decoder's own value is written. The probe
// skips the packet scan, so a long film costs one ffprobe run before the
// extraction. The WAV header carries no tool version or
// metadata, so the same source and format write the same bytes on the
// pinned ffmpeg. A file already at out is replaced.
//
// An empty path or out, an out that names the source, or a negative field
// reports ErrInvalid. A file without audio reports ErrNoStream. A missing
// binary reports ffmpeg.ErrNotFound, a run that fails reports
// ffmpeg.ErrFailed, and a cancelled context stops the child and reports the
// context error.
func ExtractAudio(ctx context.Context, tools ffmpeg.Tools, path, out string, f Format) (Format, error) {
	if path == "" || out == "" {
		return Format{}, fmt.Errorf("%w: extract audio needs a source and an output path", ErrInvalid)
	}
	if samePath(path, out) {
		return Format{}, fmt.Errorf("%w: extract audio would overwrite its source %s", ErrInvalid, path)
	}
	if f.SampleRate < 0 || f.Channels < 0 {
		return Format{}, fmt.Errorf("%w: sample rate %d and channels %d must not be negative", ErrInvalid, f.SampleRate, f.Channels)
	}
	dst, err := filepath.Abs(out)
	if err != nil {
		return Format{}, fmt.Errorf("extract audio %s: %w", path, err)
	}
	media, err := read(ctx, tools, path, false)
	if err != nil {
		return Format{}, err
	}
	source, ok := media.Audio()
	if !ok {
		return Format{}, fmt.Errorf("extract audio %s: %w: no audio", path, ErrNoStream)
	}
	if f.SampleRate == 0 {
		f.SampleRate = source.SampleRate
	}
	if f.Channels == 0 {
		f.Channels = source.Channels
	}
	args := []string{
		"-nostdin", "-v", "error", "-y", "-i", path,
		"-map", "0:a:0", "-vn", "-sn", "-dn",
		"-map_metadata", "-1", "-fflags", "+bitexact", "-flags:a", "+bitexact",
		"-c:a", "pcm_s16le",
	}
	// A source whose report leaves a field unknown keeps the decoder's
	// own value rather than passing ffmpeg a zero it would refuse.
	if f.SampleRate > 0 {
		args = append(args, "-ar", strconv.Itoa(f.SampleRate))
	}
	if f.Channels > 0 {
		args = append(args, "-ac", strconv.Itoa(f.Channels))
	}
	args = append(args, "-f", "wav", dst)
	err = ffmpeg.Run(ctx, tools, args...)
	if err != nil {
		return Format{}, fmt.Errorf("extract audio %s: %w", path, err)
	}
	return f, nil
}

// samePath reports whether two paths name one file once cleaned and made
// absolute. A path that cannot be made absolute is compared as cleaned.
func samePath(a, b string) bool {
	if aa, err := filepath.Abs(a); err == nil {
		a = aa
	}
	if bb, err := filepath.Abs(b); err == nil {
		b = bb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
