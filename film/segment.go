package film

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nrynss/keel/ffmpeg"
)

// TitleCard writes a blurred still with the title and an optional byline.
// The card is the same frame as every other segment. cfg.FontFile is required.
func TitleCard(ctx context.Context, tools ffmpeg.Tools, cfg Config, image, title, byline, out string) error {
	if image == "" || strings.TrimSpace(title) == "" || out == "" {
		return fmt.Errorf("%w: title card needs an image, a title, and an output path", ErrInvalid)
	}
	if err := requireFont(cfg); err != nil {
		return err
	}
	work := workDir(cfg, out)
	titleFile, err := writeTemp(work, "title-*.txt", []byte(title))
	if err != nil {
		return fmt.Errorf("write title text: %w", err)
	}
	defer os.Remove(titleFile)
	line := formatByline(byline)
	var bylineFile string
	if line != "" {
		bylineFile, err = writeTemp(work, "byline-*.txt", []byte(line))
		if err != nil {
			return fmt.Errorf("write byline text: %w", err)
		}
		defer os.Remove(bylineFile)
	}
	font := fontOpt(cfg.FontFile)
	var vf strings.Builder
	fmt.Fprintf(&vf, "scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=%s,setsar=1,",
		frameWidth, frameHeight, frameWidth, frameHeight, filmColor)
	vf.WriteString("boxblur=18:2,eq=brightness=-0.22:saturation=0.8,")
	if line != "" {
		fmt.Fprintf(&vf, "drawtext=%stextfile=%s:fontcolor=white:fontsize=56:x=(w-text_w)/2:y=(h-text_h)/2-40,", font, escapeFilterPath(titleFile))
		fmt.Fprintf(&vf, "drawtext=%stextfile=%s:fontcolor=white:fontsize=36:x=(w-text_w)/2:y=(h-text_h)/2+40,", font, escapeFilterPath(bylineFile))
	} else {
		fmt.Fprintf(&vf, "drawtext=%stextfile=%s:fontcolor=white:fontsize=56:x=(w-text_w)/2:y=(h-text_h)/2,", font, escapeFilterPath(titleFile))
	}
	vf.WriteString("format=yuv420p")
	hold := fmt.Sprintf("%.3f", cfg.titleHold().Seconds())
	args := []string{
		"-loop", "1", "-t", hold, "-i", image,
		"-f", "lavfi", "-t", hold, "-i", "anullsrc=channel_layout=stereo:sample_rate=44100",
		"-vf", vf.String(),
		"-r", "25",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "20",
		"-c:a", "aac", "-b:a", "128k", "-ar", "44100", "-ac", "2",
		"-shortest", "-movflags", "+faststart", "-y", out,
	}
	return run(ctx, tools, out, args)
}

// PageSegment writes one still, its caption, and either its narration or a
// silent hold. hold is the narration's known length and is required when
// audio is set. A silent page ignores hold and uses SilentHold(text). The
// narrated segment is bounded with an output duration, not with -shortest,
// because -shortest on a looped still can run past the clip. A silent
// segment bounds the still itself with an input -t and keeps -shortest:
// an unbounded looped still flushes frames already queued when the audio
// ends, so the page runs past its hold under load.
func PageSegment(ctx context.Context, tools ffmpeg.Tools, cfg Config, image, text, audio string, hold time.Duration, out string) error {
	if image == "" || out == "" {
		return fmt.Errorf("%w: page segment needs an image and an output path", ErrInvalid)
	}
	if audio != "" && hold <= 0 {
		return fmt.Errorf("%w: a narrated page needs its duration", ErrInvalid)
	}
	if err := requireFont(cfg); err != nil {
		return err
	}
	work := workDir(cfg, out)
	vf := fmt.Sprintf(
		"scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,pad=%d:%d:0:0:color=%s,setsar=1,",
		artWidth, artHeight, artWidth, artHeight, frameWidth, frameHeight, surfaceColor)
	layout := LayoutCaption(text)
	if len(layout.Lines) > 0 {
		captionFile, err := writeTemp(work, "caption-*.txt", []byte(strings.Join(layout.Lines, "\n")))
		if err != nil {
			return fmt.Errorf("write caption text: %w", err)
		}
		defer os.Remove(captionFile)
		vf += fmt.Sprintf("drawtext=%stextfile=%s:fontcolor=%s:fontsize=%d:line_spacing=0:expansion=none:text_align=C:x=(w-text_w)/2:y=%d+((%d-text_h)/2)",
			fontOpt(cfg.FontFile), escapeFilterPath(captionFile), inkColor, layout.FontSize, artHeight, bandHeight)
	}
	vf += ",format=yuv420p"
	var args []string
	if audio != "" {
		args = []string{"-loop", "1", "-i", image, "-i", audio}
	} else {
		silent := fmt.Sprintf("%.3f", SilentHold(text).Seconds())
		args = []string{
			"-loop", "1", "-t", silent, "-i", image,
			"-f", "lavfi", "-t", silent, "-i", "anullsrc=channel_layout=stereo:sample_rate=44100",
		}
	}
	args = append(args,
		"-vf", vf,
		"-r", "25",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "20",
		"-c:a", "aac", "-b:a", "128k", "-ar", "44100", "-ac", "2",
	)
	if audio != "" {
		args = append(args, "-t", fmt.Sprintf("%.6f", hold.Seconds()))
	} else {
		args = append(args, "-shortest")
	}
	args = append(args, "-movflags", "+faststart", "-y", out)
	return run(ctx, tools, out, args)
}

// EndCard writes a flat card with cfg.EndTitle and the optional domain.
// The color source is bounded with an input -t. An unbounded source plus
// -shortest can flush frames past the hold, the same way an unbounded still can.
func EndCard(ctx context.Context, tools ffmpeg.Tools, cfg Config, out string) error {
	if out == "" || strings.TrimSpace(cfg.EndTitle) == "" {
		return fmt.Errorf("%w: end card needs an output path and a title", ErrInvalid)
	}
	if err := requireFont(cfg); err != nil {
		return err
	}
	work := workDir(cfg, out)
	titleFile, err := writeTemp(work, "end-title-*.txt", []byte(cfg.EndTitle))
	if err != nil {
		return fmt.Errorf("write end title: %w", err)
	}
	defer os.Remove(titleFile)
	var domainFile string
	if strings.TrimSpace(cfg.Domain) != "" {
		domainFile, err = writeTemp(work, "end-domain-*.txt", []byte(cfg.Domain))
		if err != nil {
			return fmt.Errorf("write end domain: %w", err)
		}
		defer os.Remove(domainFile)
	}
	font := fontOpt(cfg.FontFile)
	var vf strings.Builder
	if domainFile != "" {
		fmt.Fprintf(&vf, "drawtext=%stextfile=%s:fontcolor=white:fontsize=56:x=(w-text_w)/2:y=(h-text_h)/2-30,", font, escapeFilterPath(titleFile))
		fmt.Fprintf(&vf, "drawtext=%stextfile=%s:fontcolor=0xaaaaaa:fontsize=32:x=(w-text_w)/2:y=(h-text_h)/2+40,", font, escapeFilterPath(domainFile))
	} else {
		fmt.Fprintf(&vf, "drawtext=%stextfile=%s:fontcolor=white:fontsize=56:x=(w-text_w)/2:y=(h-text_h)/2,", font, escapeFilterPath(titleFile))
	}
	vf.WriteString("format=yuv420p")
	hold := fmt.Sprintf("%.3f", cfg.endHold().Seconds())
	color := fmt.Sprintf("color=c=%s:s=%dx%d:r=25", filmColor, frameWidth, frameHeight)
	args := []string{
		"-f", "lavfi", "-t", hold, "-i", color,
		"-f", "lavfi", "-t", hold, "-i", "anullsrc=channel_layout=stereo:sample_rate=44100",
		"-vf", vf.String(),
		"-r", "25",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "20",
		"-c:a", "aac", "-b:a", "128k", "-ar", "44100", "-ac", "2",
		"-shortest", "-movflags", "+faststart", "-y", out,
	}
	return run(ctx, tools, out, args)
}

// Concat joins segments with a stream copy and writes optional metadata.
func Concat(ctx context.Context, tools ffmpeg.Tools, cfg Config, segments []string, title, out string) error {
	if len(segments) == 0 || out == "" {
		return fmt.Errorf("%w: concat needs segments and an output path", ErrInvalid)
	}
	work := workDir(cfg, out)
	var b strings.Builder
	for _, path := range segments {
		fmt.Fprintf(&b, "file %s\n", escapeConcatPath(path))
	}
	list, err := writeTemp(work, "concat-*.txt", []byte(b.String()))
	if err != nil {
		return fmt.Errorf("write concat list: %w", err)
	}
	defer os.Remove(list)
	args := []string{"-f", "concat", "-safe", "0", "-i", list, "-c", "copy", "-movflags", "+faststart"}
	if title != "" {
		args = append(args, "-metadata", "title="+title)
	}
	if cfg.Artist != "" {
		args = append(args, "-metadata", "artist="+cfg.Artist)
	}
	if cfg.Comment != "" {
		args = append(args, "-metadata", "comment="+cfg.Comment)
	}
	args = append(args, "-y", out)
	return run(ctx, tools, out, args)
}

// requireFont reports ErrInvalid when the font is missing or unreadable.
func requireFont(cfg Config) error {
	if strings.TrimSpace(cfg.FontFile) == "" {
		return fmt.Errorf("%w: font file is required", ErrInvalid)
	}
	if _, err := os.Stat(cfg.FontFile); err != nil {
		return fmt.Errorf("%w: font file: %v", ErrInvalid, err)
	}
	return nil
}

// workDir returns the scratch directory for one segment.
func workDir(cfg Config, out string) string {
	if cfg.WorkDir != "" {
		return cfg.WorkDir
	}
	return filepath.Dir(out)
}

// fontOpt is the drawtext fontfile option, escaped and ending in a colon.
func fontOpt(path string) string {
	return "fontfile=" + escapeFilterPath(path) + ":"
}

// run executes ffmpeg and names the output in the error.
func run(ctx context.Context, tools ffmpeg.Tools, out string, args []string) error {
	if err := ffmpeg.Run(ctx, tools, args...); err != nil {
		return fmt.Errorf("film %s: %w", out, err)
	}
	return nil
}

// escapeFilterPath escapes a path for a filtergraph option value.
func escapeFilterPath(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch r {
		case '\\':
			b.WriteString(`\\\\`)
		case '\'':
			b.WriteString(`\\\'`)
		case ':':
			b.WriteString(`\\:`)
		case ',', ';', '[', ']', ' ':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeConcatPath quotes a path for a concat demuxer list.
func escapeConcatPath(p string) string {
	return "'" + strings.ReplaceAll(p, `'`, `'\''`) + "'"
}

// formatByline returns the title card's second line.
func formatByline(b string) string {
	trimmed := strings.TrimSpace(b)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "by ") {
		return trimmed
	}
	return "by " + trimmed
}

// writeTemp writes data to a new file in dir and returns its path.
func writeTemp(dir, pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// publish moves src onto dst without truncating a previous dst first.
// A same-filesystem rename replaces dst in one step. Any other rename error,
// including a cross-filesystem move, copies the bytes to a temporary file in
// dst's directory and renames that complete file onto dst. A copy, sync, or
// context error removes the temporary file and leaves the previous dst in
// place. Callers that publish the same dst together are not queued: the last
// successful rename is the file a reader sees.
func publish(ctx context.Context, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	return publishBeside(ctx, src, dst)
}

// publishBeside copies src to a temporary file next to dst, then renames it
// onto dst. dst is not opened for writing until that rename.
func publishBeside(ctx context.Context, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("publish %s: source is not a regular file", dst)
	}
	if err := publishReader(ctx, in, info.Mode().Perm(), dst); err != nil {
		return err
	}
	_ = os.Remove(src)
	return nil
}

// publishReader writes r to a temporary file in dst's directory and renames
// it onto dst only after the copy and sync succeed. mode is the permission
// of the published file. A failure or a cancelled context removes the
// temporary file and does not modify dst.
func publishReader(ctx context.Context, r io.Reader, mode os.FileMode, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".keel-film-*.partial")
	if err != nil {
		return fmt.Errorf("create publish temp for %s: %w", dst, err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if err := copyContext(ctx, tmp, r); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy %s: %w", dst, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %s: %w", dst, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", dst, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("replace %s: %w", dst, err)
	}
	committed = true
	syncDir(dir)
	return nil
}

// copyContext copies until src ends, ctx is cancelled, or a read or write
// fails.
func copyContext(ctx context.Context, dst io.Writer, src io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// syncDir best-effort flushes the directory entry of a published file.
// A filesystem that cannot sync a directory does not undo the rename.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
