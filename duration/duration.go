// Package duration reads the length of a WAV or MP3 from its own bytes.
//
// A runtime may ship ffmpeg without ffprobe. Film totals and fade anchors
// still need a length known in Go. Read handles the two containers a speech
// clip usually arrives in. WAV length comes from the fmt and data chunks, so
// PCM is exact. MP3 length comes from the frame structure, never from
// decoding audio. ffmpeg.Duration remains the path when ffprobe is present.
package duration

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrUnknown reports bytes that are not a WAV or MP3 this package can measure.
// A truncated header, a missing MPEG frame, and an unsupported version or
// layer all match it with errors.Is.
var ErrUnknown = errors.New("duration: unrecognised audio")

// Read reports the length of a WAV or MP3 held in b. A WAV is recognised by
// its RIFF/WAVE header. Anything else is read as an MP3.
func Read(b []byte) (time.Duration, error) {
	if len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE" {
		return wavDuration(b)
	}
	return mp3Duration(b)
}

// ReadFile reads path and reports its length. The whole file is loaded,
// which suits a speech clip and not a long recording.
func ReadFile(path string) (time.Duration, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	d, err := Read(b)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return d, nil
}

// wavDuration reads a WAV's length from its fmt and data chunks. The byte
// rate is the one the fmt chunk declares, and the data chunk supplies the
// byte count. PCM is exact, so this measurement has no frame rounding.
func wavDuration(b []byte) (time.Duration, error) {
	if len(b) < 12 {
		return 0, fmt.Errorf("%w: wav shorter than its header", ErrUnknown)
	}
	var byteRate uint32
	var dataSize uint32
	var haveFmt, haveData bool
	for off := 12; off+8 <= len(b); {
		id := string(b[off : off+4])
		size := binary.LittleEndian.Uint32(b[off+4 : off+8])
		payload := off + 8
		if uint64(payload)+uint64(size) > uint64(len(b)) {
			return 0, fmt.Errorf("%w: truncated wav chunk %s", ErrUnknown, id)
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return 0, fmt.Errorf("%w: truncated fmt chunk", ErrUnknown)
			}
			byteRate = binary.LittleEndian.Uint32(b[payload+8 : payload+12])
			haveFmt = true
		case "data":
			dataSize = size
			haveData = true
		}
		step := int(size)
		if size%2 == 1 {
			step++
		}
		off = payload + step
	}
	if !haveFmt || byteRate == 0 {
		return 0, fmt.Errorf("%w: wav has no readable fmt chunk", ErrUnknown)
	}
	if !haveData {
		return 0, fmt.Errorf("%w: wav has no data chunk", ErrUnknown)
	}
	return time.Duration(float64(dataSize) / float64(byteRate) * float64(time.Second)), nil
}

// mp3Duration reads an MP3's length from its frames. ID3 tags are skipped,
// the first MPEG frame is located, and the length is taken from a Xing or
// Info frame count when one is present. A LAME, Lavf, or Lavc tag that
// follows trims encoder delay and tail padding, which is the playable length
// ffmpeg reports. A header with no such tag keeps the declared count. A
// stream with no Xing or Info header is treated as constant bit rate, and
// the byte count after the tags over the per-frame size gives the frame
// count, to within one frame.
func mp3Duration(b []byte) (time.Duration, error) {
	off := 0
	if len(b) >= 10 && string(b[:3]) == "ID3" {
		// Version 0xFF is not an ID3 version, so the size that follows cannot
		// be trusted. A valid MPEG frame may still begin at offset 10. This
		// is a malformed tag, not a missing frame.
		if b[3] == 0xFF {
			return 0, fmt.Errorf("%w: malformed ID3 tag whose size cannot be trusted", ErrUnknown)
		}
		size := syncsafe(b[6:10])
		if size < 0 {
			return 0, fmt.Errorf("%w: invalid ID3 tag size", ErrUnknown)
		}
		off = 10 + size
		if b[5]&0x10 != 0 {
			off += 10
		}
	}
	end := len(b)
	if end-off >= 128 && string(b[end-128:end-125]) == "TAG" {
		end -= 128
	}
	i := off
	for i+4 <= end {
		if b[i] == 0xFF && b[i+1]&0xE0 == 0xE0 {
			break
		}
		i++
	}
	if i+4 > end {
		return 0, fmt.Errorf("%w: no MPEG audio frame found", ErrUnknown)
	}
	ver, layer, brIdx, srIdx, padding, chMode := mp3Header(b[i : i+4])
	if layer != 1 || brIdx == 0 || brIdx == 15 || srIdx == 3 {
		return 0, fmt.Errorf("%w: unsupported or corrupt MPEG frame header", ErrUnknown)
	}
	sr := mp3SampleRate(ver, srIdx)
	if sr <= 0 {
		return 0, fmt.Errorf("%w: unsupported MPEG version in frame header", ErrUnknown)
	}
	spf := 1152
	if ver != 3 {
		spf = 576
	}
	bitrate := mp3Bitrate(ver, brIdx) * 1000
	sideInfo := 17
	if chMode != 3 {
		sideInfo = 32
	}
	if ver != 3 {
		sideInfo = 9
		if chMode != 3 {
			sideInfo = 17
		}
	}
	payload := i + 4 + sideInfo
	if payload+12 <= end && (string(b[payload:payload+4]) == "Xing" || string(b[payload:payload+4]) == "Info") {
		if flags := binary.BigEndian.Uint32(b[payload+4 : payload+8]); flags&1 != 0 {
			if frames := int64(binary.BigEndian.Uint32(b[payload+8 : payload+12])); frames > 0 {
				startPad, endPad := mp3GaplessPads(b, payload, flags, end)
				samples := frames*int64(spf) - startPad - endPad
				if samples <= 0 {
					return 0, fmt.Errorf("%w: gapless delay and padding exceed the declared frames", ErrUnknown)
				}
				return durationFromSamples(samples, int64(sr)), nil
			}
		}
	}
	frameLen := int64(spf/8*bitrate/sr) + int64(padding)
	if frameLen <= 0 {
		return 0, fmt.Errorf("%w: MPEG frame length is zero", ErrUnknown)
	}
	frames := int64(end-i) / frameLen
	if frames <= 0 {
		return 0, fmt.Errorf("%w: audio data shorter than one MPEG frame", ErrUnknown)
	}
	return durationFromSamples(frames*int64(spf), int64(sr)), nil
}

// durationFromSamples converts a sample count at a sample rate into a
// duration.
func durationFromSamples(samples, sr int64) time.Duration {
	return time.Duration(float64(samples) / float64(sr) * float64(time.Second))
}

// mp3GaplessPads reads the encoder delay and tail padding a Xing or Info
// stream stores in the LAME, Lavf, or Lavc tag that follows the header's
// present fields. The 24-bit value at tag+21 stores the delay in the top 12
// bits and the padding in the bottom 12, which is the layout ffmpeg's MP3
// demuxer parses. Zero pads are returned when no such tag is present, which
// leaves the declared count untrimmed.
func mp3GaplessPads(b []byte, payload int, flags uint32, end int) (startPad, endPad int64) {
	off := payload + 8
	if flags&1 != 0 {
		off += 4
	}
	if flags&2 != 0 {
		off += 4
	}
	if flags&4 != 0 {
		off += 100
	}
	if flags&8 != 0 {
		off += 4
	}
	if off+24 > end {
		return 0, 0
	}
	tag := string(b[off : off+4])
	if tag != "LAME" && tag != "Lavf" && tag != "Lavc" {
		return 0, 0
	}
	v := uint32(b[off+21])<<16 | uint32(b[off+22])<<8 | uint32(b[off+23])
	return int64(v >> 12), int64(v & 0xFFF)
}

// mp3Header decodes a four-byte MPEG audio frame header. ver is 3 for MPEG-1,
// 2 for MPEG-2, and 0 for MPEG-2.5. layer 1 is Layer III, the only layer this
// reader accepts.
func mp3Header(h []byte) (ver, layer, brIdx, srIdx, padding, chMode int) {
	ver = int(h[1]>>3) & 0x3
	layer = int(h[1]>>1) & 0x3
	brIdx = int(h[2] >> 4)
	srIdx = int(h[2]>>2) & 0x3
	padding = int(h[2]>>1) & 0x1
	chMode = int(h[3] >> 6)
	return
}

// mp3Bitrate returns the Layer III bitrate in kbps for a version and bitrate
// index. Index 0 and 15 are rejected by the caller.
func mp3Bitrate(ver, idx int) int {
	if ver == 3 {
		table := [...]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
		return table[idx]
	}
	table := [...]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}
	return table[idx]
}

// mp3SampleRate returns the sample rate in Hz for a version and sample-rate
// index. Index 3 yields 0 and is rejected by the caller.
func mp3SampleRate(ver, idx int) int {
	switch ver {
	case 3:
		return [...]int{44100, 48000, 32000, 0}[idx]
	case 2:
		return [...]int{22050, 24000, 16000, 0}[idx]
	case 0:
		return [...]int{11025, 12000, 8000, 0}[idx]
	default:
		return 0
	}
}

// syncsafe decodes a four-byte ID3 syncsafe integer. A negative result marks
// a high bit set, which the tag spec forbids.
func syncsafe(b []byte) int {
	if b[0]&0x80 != 0 || b[1]&0x80 != 0 || b[2]&0x80 != 0 || b[3]&0x80 != 0 {
		return -1
	}
	return int(b[0])<<21 | int(b[1])<<14 | int(b[2])<<7 | int(b[3])
}
