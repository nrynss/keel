package probe

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nrynss/keel/ffmpeg"
)

// Size bounds the stills Frames writes, in display orientation. The zero
// value keeps the picture's own size. A Width alone or a Height alone scales
// to it and keeps the aspect ratio. Both together fit the picture inside
// the box they draw, keeping the aspect ratio, so one side meets the box
// and the other falls inside it.
type Size struct {
	Width  int
	Height int
}

// scale returns the scale filter for the size, or the empty string when the
// picture keeps its own size.
func (s Size) scale() string {
	switch {
	case s.Width == 0 && s.Height == 0:
		return ""
	case s.Height == 0:
		return "scale=" + strconv.Itoa(s.Width) + ":-1"
	case s.Width == 0:
		return "scale=-1:" + strconv.Itoa(s.Height)
	default:
		return "scale=" + strconv.Itoa(s.Width) + ":" + strconv.Itoa(s.Height) + ":force_original_aspect_ratio=decrease"
	}
}

// Frames writes one PNG still per time into dir and returns their paths,
// in the order of times. The still for times[i] is named frame-NNNN.png
// after i, counting from zero and padded to four digits, and a file already
// at that path is replaced. dir must exist, and the returned paths are
// absolute.
//
// Each still is the first frame of the first moving picture stream that
// starts at or after its time, found by an accurate seek, so a time that
// falls inside a frame's span returns the next frame and a time on a frame
// boundary returns that frame. The stream's rotation is applied, so a
// still is upright and size counts in display orientation. A time must not
// be negative and must fall before the probed duration, or the call
// reports ErrInvalid before it writes anything. A time that still yields
// no frame, such as one inside the last frame's span, reports ErrNoFrame.
// A negative size reports ErrInvalid, and a file without moving picture
// reports ErrNoStream. The stills written before a failure stay in dir.
//
// A missing binary reports ffmpeg.ErrNotFound, a run that fails reports
// ffmpeg.ErrFailed, and a cancelled context stops the child and reports
// the context error.
func Frames(ctx context.Context, tools ffmpeg.Tools, path string, times []time.Duration, size Size, dir string) ([]string, error) {
	if path == "" || dir == "" {
		return nil, fmt.Errorf("%w: frames needs a path and a directory", ErrInvalid)
	}
	if size.Width < 0 || size.Height < 0 {
		return nil, fmt.Errorf("%w: frame size %dx%d must not be negative", ErrInvalid, size.Width, size.Height)
	}
	media, err := read(ctx, tools, path, false)
	if err != nil {
		return nil, err
	}
	video, ok := media.Video()
	if !ok {
		return nil, fmt.Errorf("frames %s: %w: no moving picture", path, ErrNoStream)
	}
	for i, at := range times {
		if at < 0 || (media.Duration > 0 && at >= media.Duration) {
			return nil, fmt.Errorf("%w: frame %d at %s falls outside the file's %s", ErrInvalid, i, at, media.Duration)
		}
	}
	// An absolute directory keeps an output that starts with a dash from
	// reading as an ffmpeg option.
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("frames %s: %w", path, err)
	}
	paths := make([]string, 0, len(times))
	for i, at := range times {
		out := filepath.Join(abs, fmt.Sprintf("frame-%04d.png", i))
		if err := still(ctx, tools, path, video.Index, at, size, out); err != nil {
			return paths, fmt.Errorf("frames %s: frame %d at %s: %w", path, i, at, err)
		}
		paths = append(paths, out)
	}
	return paths, nil
}

// still writes the one frame at or after at to out. A stale file at out is
// removed first, so the existence check after the run proves this run
// wrote the still.
func still(ctx context.Context, tools ffmpeg.Tools, path string, stream int, at time.Duration, size Size, out string) error {
	if err := os.Remove(out); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove stale still: %w", err)
	}
	args := []string{
		"-nostdin", "-v", "error", "-y",
		"-ss", timestamp(at), "-i", path,
		"-map", "0:" + strconv.Itoa(stream),
		"-frames:v", "1",
	}
	if vf := size.scale(); vf != "" {
		args = append(args, "-vf", vf)
	}
	args = append(args, "-an", "-sn", "-dn", "-f", "image2", "-c:v", "png", "-update", "1", out)
	if err := ffmpeg.Run(ctx, tools, args...); err != nil {
		return err
	}
	if _, err := os.Stat(out); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNoFrame
		}
		return err
	}
	return nil
}

// timestamp renders a non-negative duration as decimal seconds at
// microsecond precision, the precision ffmpeg parses times at.
func timestamp(d time.Duration) string {
	return fmt.Sprintf("%d.%06d", d/time.Second, (d%time.Second)/time.Microsecond)
}
