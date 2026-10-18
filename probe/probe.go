// Package probe answers what a media file holds before any work is planned
// on it: its streams, its scene changes, stills at chosen times, and its
// audio as a working file.
//
// Probe reads ffprobe's own JSON report and returns typed streams. A missing
// stream is never a zero value standing in for one: Media.Audio and
// Media.Video report whether the stream exists, and Scenes, Frames and
// ExtractAudio refuse a file without the stream they need with ErrNoStream.
// Durations follow the rule ffmpeg.Duration follows, the container's
// duration first and the first audio stream's when the container has none,
// rounded to the nearest millisecond, so the two never disagree about one
// file. A silent file whose container records no length falls back to its
// first video stream, where ffmpeg.Duration reports an error. The first audio stream reports the sample rate, channel count,
// layout, codec and bit rate ffmpeg.AudioFormat reports for it, with the
// same fallbacks.
//
// Rotation is explicit. Width and Height are the picture as it is stored,
// and Rotation is the clockwise turn, in degrees, a player applies before
// it shows the picture, read from the stream's display matrix. DisplaySize
// returns the size a viewer sees. Frames applies the rotation, so a still
// is upright and its size counts in display orientation.
//
// Variable frame rate is explicit too. ffprobe's two frame rate fields
// cannot tell a variable stream from a constant one in every container, so
// Probe reads the timestamp of every packet of each video stream and calls
// the stream variable when the gaps between consecutive frames differ by
// more than one tick of the stream's time base. A constant stream reports
// the frame rate ffprobe reports, such as 30000/1001, and a variable one
// reports the mean rate over its frames.
//
// Scenes runs the scene filter and parses the cut times from the filter's
// own printed output, never from a guess. Frames writes PNG stills, one
// seek per time, and ExtractAudio writes 16-bit PCM WAV at the source rate
// or a requested one. Every invocation goes through the ffmpeg package and
// carries the caller's context, so a cancelled probe stops its child.
//
// The results are pinned against clips the tests generate on the pinned
// ffmpeg 9.0.1. Another ffmpeg build may report a scene score or a
// rounding differently.
package probe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nrynss/keel/ffmpeg"
)

// ErrInvalid reports arguments a call cannot use: an empty path, a
// threshold or a time out of range, or a negative size or rate.
var ErrInvalid = errors.New("probe: invalid request")

// ErrNoStream reports a file without the stream a call needs. Probe reports
// it for a file ffprobe reads but finds no stream in.
var ErrNoStream = errors.New("probe: stream missing")

// ErrNoFrame reports a requested time that produced no still, because no
// frame starts at or after it.
var ErrNoFrame = errors.New("probe: no frame at time")

// Kind names what a stream carries, in ffprobe's own words.
type Kind string

// The stream kinds ffprobe reports. A kind ffprobe names that is not listed
// here arrives verbatim.
const (
	// KindAudio is a stream of sound.
	KindAudio Kind = "audio"
	// KindVideo is a stream of pictures, moving or one attached still.
	KindVideo Kind = "video"
	// KindSubtitle is a stream of timed text or bitmaps.
	KindSubtitle Kind = "subtitle"
	// KindData is a stream of timed data, such as a timecode track.
	KindData Kind = "data"
	// KindAttachment is a file carried in the container, such as a font.
	KindAttachment Kind = "attachment"
)

// Rate is an exact frame rate, a count of frames per Den seconds. The zero
// value means the rate is unknown.
type Rate struct {
	Num int64
	Den int64
}

// Float64 returns the rate in frames per second, or zero when it is
// unknown.
func (r Rate) Float64() float64 {
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

// String renders the rate as ffprobe does, such as 30000/1001. The zero
// Rate renders as 0/0, as ffprobe prints an unknown rate.
func (r Rate) String() string {
	return strconv.FormatInt(r.Num, 10) + "/" + strconv.FormatInt(r.Den, 10)
}

// Stream is one stream of a media file. Fields that do not apply to the
// stream's kind stay zero, so an audio stream has no Width and a video
// stream has no SampleRate.
type Stream struct {
	// Index is the stream's position in the file, as ffmpeg's -map counts
	// it.
	Index int
	// Kind says what the stream carries.
	Kind Kind
	// Codec is the codec's short name, such as h264 or pcm_s16le.
	Codec string
	// Duration is the stream's own length rounded to the nearest
	// millisecond, or zero when the stream reports none.
	Duration time.Duration
	// BitRate is the stream's bit rate in bits per second, or zero when it
	// cannot be read. An audio stream without one falls back to the sample
	// layout and then, in a file of one stream, to the container's rate,
	// exactly as ffmpeg.AudioFormat does.
	BitRate int64

	// SampleRate is an audio stream's rate in hertz.
	SampleRate int
	// Channels is an audio stream's channel count.
	Channels int
	// ChannelLayout is an audio stream's layout, such as stereo. A stream
	// without one falls back to mono or stereo by channel count.
	ChannelLayout string

	// Width and Height are a video stream's picture as it is stored,
	// before Rotation turns it.
	Width  int
	Height int
	// Rotation is the clockwise turn in degrees, from 0 up to but not
	// including 360, that a player applies to the stored picture before it
	// shows it. Phones record portrait video as a landscape picture with a
	// Rotation of 90 or 270. Zero means the picture shows as stored.
	Rotation int
	// FrameRate is a video stream's frame rate. A constant stream reports
	// the rate ffprobe reports, and a variable one the mean rate over its
	// frames. Zero means unknown.
	FrameRate Rate
	// VariableFrameRate reports that the gaps between the stream's frames
	// differ by more than one tick of its time base, so FrameRate is a
	// mean that no stretch of the stream may match. A stream with fewer
	// than three timestamped frames reports false, because two gaps are
	// the least that can differ.
	VariableFrameRate bool
	// AttachedPicture reports a video stream that holds one still, such as
	// the cover art of an audio file, rather than moving picture.
	AttachedPicture bool
}

// DisplaySize returns the picture's size as a viewer sees it. A Rotation of
// 90 or 270 swaps Width and Height. Any other rotation returns the stored
// size.
func (s Stream) DisplaySize() (width, height int) {
	if s.Rotation == 90 || s.Rotation == 270 {
		return s.Height, s.Width
	}
	return s.Width, s.Height
}

// Media is what Probe read about one file.
type Media struct {
	// Format is the container's short name list as ffprobe reports it,
	// such as mov,mp4,m4a,3gp,3g2,mj2.
	Format string
	// Duration is the file's length rounded to the nearest millisecond:
	// the container's duration, else the first audio stream's, else the
	// first video stream's, else zero. Whenever ffmpeg.Duration reports a
	// length for the file it reports this one.
	Duration time.Duration
	// Streams lists every stream in file order. Probe never returns an
	// empty list without an error.
	Streams []Stream
}

// Audio returns the first audio stream and true, or false when the file
// has none.
func (m Media) Audio() (Stream, bool) {
	for _, s := range m.Streams {
		if s.Kind == KindAudio {
			return s, true
		}
	}
	return Stream{}, false
}

// Video returns the first video stream that carries moving picture and
// true, or false when the file has none. An attached picture, such as cover
// art, is not moving picture and is skipped.
func (m Media) Video() (Stream, bool) {
	for _, s := range m.Streams {
		if s.Kind == KindVideo && !s.AttachedPicture {
			return s, true
		}
	}
	return Stream{}, false
}

// probeStream is the subset of one ffprobe stream object Probe reads.
type probeStream struct {
	Index         int    `json:"index"`
	CodecType     string `json:"codec_type"`
	CodecName     string `json:"codec_name"`
	SampleRate    string `json:"sample_rate"`
	Channels      int    `json:"channels"`
	ChannelLayout string `json:"channel_layout"`
	BitRate       string `json:"bit_rate"`
	BitsPerSample int    `json:"bits_per_sample"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	RFrameRate    string `json:"r_frame_rate"`
	AvgFrameRate  string `json:"avg_frame_rate"`
	TimeBase      string `json:"time_base"`
	Duration      string `json:"duration"`
	Disposition   struct {
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
	SideData []struct {
		Type     string   `json:"side_data_type"`
		Rotation *float64 `json:"rotation"`
	} `json:"side_data_list"`
	Tags struct {
		Rotate string `json:"rotate"`
	} `json:"tags"`
}

// probeReport is the response of an ffprobe run asked for streams and
// format.
type probeReport struct {
	Streams []probeStream `json:"streams"`
	Format  struct {
		Name     string `json:"format_name"`
		Duration string `json:"duration"`
		BitRate  string `json:"bit_rate"`
	} `json:"format"`
}

// Probe reads the streams of the media at path. It runs ffprobe once for
// the streams and the container, and once more over the packet timestamps
// when the file holds moving picture, which is how a variable frame rate is
// told apart. A missing binary reports ffmpeg.ErrNotFound, a file ffprobe
// cannot read reports ffmpeg.ErrFailed, and a file with no stream reports
// ErrNoStream. A cancelled context stops the child and reports the context
// error.
func Probe(ctx context.Context, tools ffmpeg.Tools, path string) (Media, error) {
	if path == "" {
		return Media{}, fmt.Errorf("%w: probe needs a path", ErrInvalid)
	}
	return read(ctx, tools, path, true)
}

// read runs the probe. With timing false it skips the packet scan, so a
// caller that needs only the streams' layout, such as ExtractAudio on a long
// film, never reads every packet. VariableFrameRate then stays false and
// FrameRate is ffprobe's own figure.
func read(ctx context.Context, tools ffmpeg.Tools, path string, timing bool) (Media, error) {
	var out bytes.Buffer
	err := ffmpeg.ProbeTo(ctx, tools, &out,
		"-v", "error", "-show_streams", "-show_format", "-of", "json", "-i", path)
	if err != nil {
		return Media{}, fmt.Errorf("probe %s: %w", path, err)
	}
	var report probeReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		return Media{}, fmt.Errorf("probe %s: decode ffprobe report: %w", path, err)
	}
	media, timeBases, err := parseReport(report)
	if err != nil {
		return Media{}, fmt.Errorf("probe %s: %w", path, err)
	}
	if timing && len(timeBases) > 0 {
		timestamps, err := packetTimestamps(ctx, tools, path)
		if err != nil {
			return Media{}, fmt.Errorf("probe %s: %w", path, err)
		}
		for i := range media.Streams {
			s := &media.Streams[i]
			tb, ok := timeBases[s.Index]
			if !ok {
				continue
			}
			if mean, variable := frameTiming(timestamps[s.Index], tb); variable {
				s.VariableFrameRate = true
				s.FrameRate = mean
			}
		}
	}
	return media, nil
}

// parseReport turns ffprobe's report into Media. It returns the time base
// of every video stream that carries moving picture, keyed by stream index,
// so the caller knows which streams need the packet scan.
func parseReport(report probeReport) (Media, map[int]Rate, error) {
	if len(report.Streams) == 0 {
		return Media{}, nil, ErrNoStream
	}
	media := Media{Format: report.Format.Name, Streams: make([]Stream, 0, len(report.Streams))}
	timeBases := make(map[int]Rate)
	for _, ps := range report.Streams {
		d, _ := seconds(ps.Duration)
		s := Stream{
			Index:    ps.Index,
			Kind:     Kind(ps.CodecType),
			Codec:    ps.CodecName,
			Duration: d,
		}
		if ps.BitRate != "" && ps.BitRate != "N/A" {
			s.BitRate, _ = strconv.ParseInt(ps.BitRate, 10, 64) // a bad value falls back below
		}
		switch s.Kind {
		case KindAudio:
			rate, err := strconv.Atoi(ps.SampleRate)
			if err != nil {
				return Media{}, nil, fmt.Errorf("stream %d: invalid sample rate %q: %w", ps.Index, ps.SampleRate, err)
			}
			s.SampleRate = rate
			s.Channels = ps.Channels
			s.ChannelLayout = layout(ps.ChannelLayout, ps.Channels)
			if s.BitRate == 0 && ps.BitsPerSample > 0 && rate > 0 && ps.Channels > 0 {
				s.BitRate = int64(ps.BitsPerSample * rate * ps.Channels)
			}
			if s.BitRate == 0 && len(report.Streams) == 1 && report.Format.BitRate != "" && report.Format.BitRate != "N/A" {
				s.BitRate, _ = strconv.ParseInt(report.Format.BitRate, 10, 64) // a bad value is not fatal
			}
		case KindVideo:
			s.Width = ps.Width
			s.Height = ps.Height
			s.Rotation = rotation(ps)
			s.AttachedPicture = ps.Disposition.AttachedPic != 0
			if s.AttachedPicture {
				// One still has no frame rate, whatever the container
				// stamps on it, and no timing to scan.
				break
			}
			s.FrameRate = parseRate(ps.AvgFrameRate)
			if s.FrameRate == (Rate{}) {
				s.FrameRate = parseRate(ps.RFrameRate)
			}
			if tb := parseRate(ps.TimeBase); tb != (Rate{}) {
				timeBases[s.Index] = tb
			}
		}
		media.Streams = append(media.Streams, s)
	}
	// The container's duration wins whenever it is present, and the first
	// audio stream stands in only when it is absent, as ffmpeg.Duration
	// decides. The first video stream is the last resort, for a silent
	// file whose container records no length.
	d, ok := seconds(report.Format.Duration)
	if !ok {
		if a, found := media.Audio(); found && a.Duration != 0 {
			d, ok = a.Duration, true
		}
	}
	if !ok {
		if v, found := media.Video(); found {
			d = v.Duration
		}
	}
	media.Duration = d
	return media, timeBases, nil
}

// layout returns the channel layout ffprobe named, or mono or stereo by
// channel count when it named none, as ffmpeg.AudioFormat does.
func layout(name string, channels int) string {
	if name != "" && name != "unknown" {
		return name
	}
	switch channels {
	case 1:
		return "mono"
	case 2:
		return "stereo"
	}
	return name
}

// rotation reads the clockwise display rotation of a video stream. The
// display matrix ffprobe prints gives the counter-clockwise angle, so the
// sign flips. A file written before display matrices carries a rotate tag,
// which is already clockwise. The result is rounded to a whole degree and
// brought into the range 0 up to 360.
func rotation(ps probeStream) int {
	for _, sd := range ps.SideData {
		if sd.Type == "Display Matrix" && sd.Rotation != nil {
			return normaliseDegrees(-*sd.Rotation)
		}
	}
	if ps.Tags.Rotate != "" {
		if deg, err := strconv.ParseFloat(ps.Tags.Rotate, 64); err == nil {
			return normaliseDegrees(deg)
		}
	}
	return 0
}

// normaliseDegrees rounds deg to a whole degree in the range 0 up to 360.
// A value that is not finite reads as no rotation.
func normaliseDegrees(deg float64) int {
	if math.IsNaN(deg) || math.IsInf(deg, 0) {
		return 0
	}
	d := int(math.Round(math.Mod(deg, 360)))
	if d < 0 {
		d += 360
	}
	return d % 360
}

// parseRate reads a rational such as 30000/1001. A malformed value, a zero
// denominator, or a zero or negative numerator reads as the zero Rate.
func parseRate(s string) Rate {
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		return Rate{}
	}
	n, err1 := strconv.ParseInt(num, 10, 64)
	d, err2 := strconv.ParseInt(den, 10, 64)
	if err1 != nil || err2 != nil || n <= 0 || d <= 0 {
		return Rate{}
	}
	return Rate{Num: n, Den: d}
}

// seconds reads a decimal seconds value and rounds it to the nearest
// millisecond, as ffmpeg.Duration does, and reports whether a value was
// present. An absent or unreadable value reads as zero and false.
func seconds(s string) (time.Duration, bool) {
	if s == "" || s == "N/A" {
		return 0, false
	}
	sec, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(sec, 0) || math.IsNaN(sec) {
		return 0, false
	}
	return time.Duration(math.Round(sec*1000)) * time.Millisecond, true
}

// packetTimestamps returns the presentation timestamp of every packet of
// every video stream, keyed by stream index, in the time base of its
// stream. A packet without a presentation timestamp falls back to its
// decode timestamp, and a packet with neither is skipped. The output is
// parsed as it streams, so a long file never sits in memory as text.
func packetTimestamps(ctx context.Context, tools ffmpeg.Tools, path string) (map[int][]int64, error) {
	sink := &packetSink{timestamps: make(map[int][]int64)}
	err := ffmpeg.ProbeTo(ctx, tools, sink,
		"-v", "error", "-select_streams", "v",
		"-show_entries", "packet=stream_index,pts,dts",
		"-of", "csv=p=0", "-i", path)
	if err != nil {
		return nil, err
	}
	sink.flush()
	if sink.err != nil {
		return nil, sink.err
	}
	return sink.timestamps, nil
}

// packetSink parses ffprobe's packet lines, stream_index,pts,dts, as they
// arrive. It never fails a write, because a short write would make the
// child see a closed pipe. A malformed line is kept as the first error and
// reported once the run ends.
type packetSink struct {
	partial    []byte
	timestamps map[int][]int64
	err        error
}

// Write consumes every whole line in p and keeps the tail for the next
// write.
func (s *packetSink) Write(p []byte) (int, error) {
	s.partial = append(s.partial, p...)
	for {
		i := bytes.IndexByte(s.partial, '\n')
		if i < 0 {
			break
		}
		s.line(string(s.partial[:i]))
		s.partial = s.partial[i+1:]
	}
	// Compact the kept tail so a long run never grows the backing array.
	s.partial = append([]byte(nil), s.partial...)
	return len(p), nil
}

// flush parses a last line that ended without a newline.
func (s *packetSink) flush() {
	if len(s.partial) > 0 {
		s.line(string(s.partial))
		s.partial = nil
	}
}

// line parses one stream_index,pts,dts line.
func (s *packetSink) line(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	fields := strings.Split(text, ",")
	if len(fields) < 2 {
		s.fail(fmt.Errorf("unreadable packet line %q", text))
		return
	}
	index, err := strconv.Atoi(fields[0])
	if err != nil {
		s.fail(fmt.Errorf("unreadable packet line %q: %w", text, err))
		return
	}
	for _, f := range fields[1:] {
		if ts, err := strconv.ParseInt(f, 10, 64); err == nil {
			s.timestamps[index] = append(s.timestamps[index], ts)
			return
		}
	}
	// Neither timestamp is set, so the packet cannot place a frame.
}

// fail keeps the first parse error.
func (s *packetSink) fail(err error) {
	if s.err == nil {
		s.err = err
	}
}

// frameTiming reads a stream's frame timestamps, in ticks of tb, and
// reports whether the gaps between consecutive frames differ by more than
// one tick, with the mean rate over the frames when they do. Timestamps
// arrive in decode order, so they are sorted into presentation order
// first, and a repeated timestamp counts once. Fewer than three distinct
// timestamps cannot show a variable rate.
func frameTiming(timestamps []int64, tb Rate) (Rate, bool) {
	ts := slices.Clone(timestamps)
	slices.Sort(ts)
	ts = slices.Compact(ts)
	if len(ts) < 3 || tb.Num <= 0 || tb.Den <= 0 {
		return Rate{}, false
	}
	minGap, maxGap := ts[1]-ts[0], ts[1]-ts[0]
	for i := 2; i < len(ts); i++ {
		gap := ts[i] - ts[i-1]
		minGap = min(minGap, gap)
		maxGap = max(maxGap, gap)
	}
	if maxGap-minGap <= 1 {
		return Rate{}, false
	}
	// The mean rate is (frames - 1) gaps over the span, where one tick is
	// tb.Num/tb.Den seconds: (n-1) * tb.Den / (span * tb.Num). The product
	// is reduced exactly, so the rate never drifts from rounding.
	span := ts[len(ts)-1] - ts[0]
	num := new(big.Int).Mul(big.NewInt(int64(len(ts)-1)), big.NewInt(tb.Den))
	den := new(big.Int).Mul(big.NewInt(span), big.NewInt(tb.Num))
	r := new(big.Rat).SetFrac(num, den)
	if !r.Num().IsInt64() || !r.Denom().IsInt64() {
		return Rate{}, true
	}
	return Rate{Num: r.Num().Int64(), Den: r.Denom().Int64()}, true
}

// lineScanner returns a scanner over raw that accepts long lines, for the
// filter output Scenes parses.
func lineScanner(raw []byte) *bufio.Scanner {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return sc
}
