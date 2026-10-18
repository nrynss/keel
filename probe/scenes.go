package probe

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/nrynss/keel/ffmpeg"
)

// DefaultSceneThreshold is the scene score Scenes cuts at when the caller
// passes zero. The scene filter scores each frame's change from the one
// before from 0 to 1, and 0.4 marks a cut between shots while it ignores
// motion and lighting changes within one.
const DefaultSceneThreshold = 0.4

// Scenes returns the times of the scene changes in the first moving
// picture stream at path, in order. A frame whose scene score exceeds
// threshold starts a new scene, and its presentation time, rounded to the
// nearest millisecond, is the cut. A threshold of zero means
// DefaultSceneThreshold, and anything else outside the range above 0 up to
// and including 1 reports ErrInvalid. The first frame never counts as a
// cut, so a file of one shot returns an empty list.
//
// The times are parsed from the lines the metadata filter prints for each
// selected frame, the tool's own output. A file without moving picture
// reports ErrNoStream. A missing binary reports ffmpeg.ErrNotFound, a run
// that fails reports ffmpeg.ErrFailed, and a cancelled context stops the
// child and reports the context error.
func Scenes(ctx context.Context, tools ffmpeg.Tools, path string, threshold float64) ([]time.Duration, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: scenes needs a path", ErrInvalid)
	}
	if threshold == 0 {
		threshold = DefaultSceneThreshold
	}
	if math.IsNaN(threshold) || threshold <= 0 || threshold > 1 {
		return nil, fmt.Errorf("%w: scene threshold %v must be above 0 and at most 1", ErrInvalid, threshold)
	}
	media, err := read(ctx, tools, path, false)
	if err != nil {
		return nil, err
	}
	video, ok := media.Video()
	if !ok {
		return nil, fmt.Errorf("scenes %s: %w: no moving picture", path, ErrNoStream)
	}
	var out bytes.Buffer
	filter := "select='gt(scene," + strconv.FormatFloat(threshold, 'f', -1, 64) + ")',metadata=mode=print:file=-"
	err = ffmpeg.RunTo(ctx, tools, &out,
		"-nostdin", "-v", "error", "-i", path,
		"-map", "0:"+strconv.Itoa(video.Index),
		"-vf", filter, "-an", "-sn", "-dn", "-f", "null", "-")
	if err != nil {
		return nil, fmt.Errorf("scenes %s: %w", path, err)
	}
	cuts, err := parseScenes(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("scenes %s: %w", path, err)
	}
	return cuts, nil
}

// parseScenes reads the cut times out of the metadata filter's printed
// lines. Each selected frame prints one header line such as
//
//	frame:0    pts:25600   pts_time:2
//
// followed by one line per metadata key. Only the header carries the time,
// and a header without a readable pts_time is an error, because a guessed
// cut would mislead every plan built on it. The times come back in the
// order the filter printed them, which is presentation order, with any
// repeat dropped.
func parseScenes(raw []byte) ([]time.Duration, error) {
	cuts := []time.Duration{}
	sc := lineScanner(raw)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "frame:") {
			continue
		}
		var at string
		for _, field := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(field, "pts_time:"); ok {
				at = v
			}
		}
		d, ok := seconds(at)
		if !ok {
			return nil, fmt.Errorf("unreadable scene line %q", line)
		}
		if n := len(cuts); n > 0 && cuts[n-1] >= d {
			continue
		}
		cuts = append(cuts, d)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read scene lines: %w", err)
	}
	return cuts, nil
}
