package ffmpeg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Format reports the audio stream properties that ffprobe measures.
type Format struct {
	SampleRate    int
	Channels      int
	ChannelLayout string
	Codec         string
	BitRate       int64
}

// ffprobeStream is the subset of one ffprobe stream object the package reads.
type ffprobeStream struct {
	CodecName     string `json:"codec_name"`
	SampleRate    string `json:"sample_rate"`
	Channels      int    `json:"channels"`
	ChannelLayout string `json:"channel_layout"`
	BitRate       string `json:"bit_rate"`
	BitsPerSample int    `json:"bits_per_sample"`
}

// ffprobeFormat is the container-level subset the package reads.
type ffprobeFormat struct {
	BitRate string `json:"bit_rate"`
}

// ffprobeJSON is the response of an ffprobe run that asks for streams and
// format.
type ffprobeJSON struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

// Duration reports the length of the media at path as measured by ffprobe.
// The container duration is used when it exists. When the container reports
// none, the first audio stream stands in. The result rounds to the nearest
// millisecond.
func Duration(ctx context.Context, tools Tools, path string) (time.Duration, error) {
	raw, err := probeText(ctx, tools, path, "-show_entries", "format=duration")
	if err != nil {
		return 0, err
	}
	if raw == "" || raw == "N/A" {
		// The container carries no duration, so the first audio stream
		// supplies one.
		raw, err = probeText(ctx, tools, path,
			"-select_streams", "a:0",
			"-show_entries", "stream=duration",
		)
		if err != nil {
			return 0, err
		}
	}
	if raw == "" || raw == "N/A" {
		return 0, fmt.Errorf("ffprobe duration %s: duration missing", path)
	}
	sec, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("parse duration %q for %s: %w", raw, path, err)
	}
	ms := math.Round(sec * 1000)
	return time.Duration(ms) * time.Millisecond, nil
}

// AudioFormat reports the first audio stream at path as measured by ffprobe.
// A stream without a channel layout falls back to mono or stereo by channel
// count. A stream without a bit rate falls back to the sample layout and then
// to the container bit rate.
func AudioFormat(ctx context.Context, tools Tools, path string) (Format, error) {
	args := []string{
		"-v", "error",
		"-select_streams", "a:0",
		"-show_streams",
		"-show_format",
		"-of", "json",
		path,
	}
	var stdout bytes.Buffer
	if err := run(ctx, tools.ffprobePath(), args, &stdout); err != nil {
		return Format{}, err
	}

	var parsed ffprobeJSON
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		return Format{}, fmt.Errorf("decode ffprobe output for %s: %w", path, err)
	}
	if len(parsed.Streams) == 0 {
		return Format{}, fmt.Errorf("no audio stream found in %s", path)
	}

	stream := parsed.Streams[0]
	sampleRate, err := strconv.Atoi(stream.SampleRate)
	if err != nil {
		return Format{}, fmt.Errorf("invalid sample rate %q in %s: %w", stream.SampleRate, path, err)
	}

	layout := stream.ChannelLayout
	if layout == "" || layout == "unknown" {
		switch stream.Channels {
		case 1:
			layout = "mono"
		case 2:
			layout = "stereo"
		}
	}

	var bitRate int64
	if stream.BitRate != "" && stream.BitRate != "N/A" {
		bitRate, _ = strconv.ParseInt(stream.BitRate, 10, 64) // a bad value falls back to the sample maths
	}
	if bitRate == 0 && stream.BitsPerSample > 0 && sampleRate > 0 && stream.Channels > 0 {
		bitRate = int64(stream.BitsPerSample * sampleRate * stream.Channels)
	}
	if bitRate == 0 && len(parsed.Streams) == 1 && parsed.Format.BitRate != "" && parsed.Format.BitRate != "N/A" {
		bitRate, _ = strconv.ParseInt(parsed.Format.BitRate, 10, 64) // a bad value is not fatal
	}

	return Format{
		SampleRate:    sampleRate,
		Channels:      stream.Channels,
		ChannelLayout: layout,
		Codec:         stream.CodecName,
		BitRate:       bitRate,
	}, nil
}

// probeText runs ffprobe for one path and returns the single text value it
// printed for entries.
func probeText(ctx context.Context, tools Tools, path string, entries ...string) (string, error) {
	args := append([]string{"-v", "error"}, entries...)
	args = append(args, "-of", "default=noprint_wrappers=1:nokey=1", path)
	var stdout bytes.Buffer
	if err := run(ctx, tools.ffprobePath(), args, &stdout); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}
