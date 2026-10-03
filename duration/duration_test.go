package duration

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadWAV(t *testing.T) {
	// 1 second of 16-bit mono at 8000 Hz: byte rate 16000, data 16000 bytes.
	b := make([]byte, 44+16000)
	copy(b[0:4], "RIFF")
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(b)-8))
	copy(b[8:12], "WAVE")
	copy(b[12:16], "fmt ")
	binary.LittleEndian.PutUint32(b[16:20], 16)
	binary.LittleEndian.PutUint16(b[20:22], 1)
	binary.LittleEndian.PutUint16(b[22:24], 1)
	binary.LittleEndian.PutUint32(b[24:28], 8000)
	binary.LittleEndian.PutUint32(b[28:32], 16000)
	binary.LittleEndian.PutUint16(b[32:34], 2)
	binary.LittleEndian.PutUint16(b[34:36], 16)
	copy(b[36:40], "data")
	binary.LittleEndian.PutUint32(b[40:44], 16000)
	got, err := Read(b)
	if err != nil {
		t.Fatal(err)
	}
	if got != time.Second {
		t.Fatalf("wav = %s, want 1s", got)
	}
}

func TestReadWAVOddChunkPadding(t *testing.T) {
	// A 1-byte fact chunk before data forces the word pad. 0.5s at 8 kHz.
	fmtChunk := make([]byte, 24)
	copy(fmtChunk[0:4], "fmt ")
	binary.LittleEndian.PutUint32(fmtChunk[4:8], 16)
	binary.LittleEndian.PutUint16(fmtChunk[8:10], 1)
	binary.LittleEndian.PutUint16(fmtChunk[10:12], 1)
	binary.LittleEndian.PutUint32(fmtChunk[12:16], 8000)
	binary.LittleEndian.PutUint32(fmtChunk[16:20], 16000)
	binary.LittleEndian.PutUint16(fmtChunk[20:22], 2)
	binary.LittleEndian.PutUint16(fmtChunk[22:24], 16)
	fact := []byte{'f', 'a', 'c', 't', 1, 0, 0, 0, 0x7f, 0}
	data := make([]byte, 8+8000)
	copy(data[0:4], "data")
	binary.LittleEndian.PutUint32(data[4:8], 8000)
	body := append(append(fmtChunk, fact...), data...)
	b := make([]byte, 12)
	copy(b[0:4], "RIFF")
	copy(b[8:12], "WAVE")
	b = append(b, body...)
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(b)-8))
	got, err := Read(b)
	if err != nil {
		t.Fatal(err)
	}
	if got != 500*time.Millisecond {
		t.Fatalf("wav = %s, want 500ms", got)
	}
}

func TestReadWAVErrors(t *testing.T) {
	if _, err := Read([]byte("RIFF\x00\x00\x00\x00WAVE")); !errors.Is(err, ErrUnknown) {
		t.Fatalf("short wave: %v", err)
	}
	if _, err := Read([]byte("not audio at all!!")); !errors.Is(err, ErrUnknown) {
		t.Fatalf("garbage: %v", err)
	}
}

func TestReadCommittedWAV(t *testing.T) {
	got, err := ReadFile(filepath.Join("..", "testdata", "audio", "tone-48k.wav"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 3*time.Second {
		t.Fatalf("tone-48k.wav = %s, want 3s", got)
	}
}

func TestReadFileMissing(t *testing.T) {
	if _, err := ReadFile(filepath.Join(t.TempDir(), "missing.wav")); err == nil {
		t.Fatal("missing file returned nil")
	}
}

func TestReadMP3CBR(t *testing.T) {
	const frameLen = 417 // MPEG-1 Layer III, 128 kbps, 44100 Hz
	const frames = 10
	b := make([]byte, 0, frames*frameLen)
	for i := 0; i < frames; i++ {
		b = append(b, 0xFF, 0xFB, 0x90, 0xC0)
		b = append(b, make([]byte, frameLen-4)...)
	}
	got, err := Read(b)
	if err != nil {
		t.Fatal(err)
	}
	want := durationFromSamples(int64(frames)*1152, 44100)
	if got != want {
		t.Fatalf("cbr = %s, want %s", got, want)
	}
}

func TestReadMP3InfoGapless(t *testing.T) {
	got, err := Read(infoFixture(155, true))
	if err != nil {
		t.Fatal(err)
	}
	if got != 4*time.Second {
		t.Fatalf("gapless = %s, want 4s", got)
	}
	untrimmed := durationFromSamples(155*1152, 44100)
	if untrimmed == 4*time.Second {
		t.Fatal("fixture does not exceed the playable length")
	}
}

func TestReadMP3InfoWithoutTag(t *testing.T) {
	got, err := Read(infoFixture(155, false))
	if err != nil {
		t.Fatal(err)
	}
	want := durationFromSamples(155*1152, 44100)
	if got != want {
		t.Fatalf("bare info = %s, want %s", got, want)
	}
}

func TestReadMP3GaplessExceedsFrames(t *testing.T) {
	_, err := Read(infoFixture(1, true))
	if !errors.Is(err, ErrUnknown) {
		t.Fatalf("err = %v, want ErrUnknown", err)
	}
}

func TestReadMP3ID3AndTrailer(t *testing.T) {
	const frameLen = 417
	frame := append([]byte{0xFF, 0xFB, 0x90, 0xC0}, make([]byte, frameLen-4)...)
	tag := make([]byte, 10)
	copy(tag, "ID3")
	tag[3] = 4
	b := append(tag, frame...)
	trailer := make([]byte, 128)
	copy(trailer, "TAG")
	b = append(b, trailer...)
	got, err := Read(b)
	if err != nil {
		t.Fatal(err)
	}
	want := durationFromSamples(1152, 44100)
	if got != want {
		t.Fatalf("tagged = %s, want %s", got, want)
	}
}

func TestReadCommittedMP3(t *testing.T) {
	path := filepath.Join("..", "testdata", "audio", "tone-48k.mp3")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Read(b)
	if err != nil {
		t.Fatal(err)
	}
	// The container probe of this fixture is 3s. A header-only count stays
	// inside one MPEG frame of that.
	const frame = 26 * time.Millisecond
	if got < 3*time.Second-frame || got > 3*time.Second+frame {
		t.Fatalf("tone-48k.mp3 = %s, want within %s of 3s", got, frame)
	}
}

// infoFixture builds an ID3 tag and one mono MPEG-1 Layer III frame whose
// payload carries an Info header. wantTag adds a Lavc tag with encoder delay
// 576 and tail padding 1584.
func infoFixture(declaredFrames uint32, wantTag bool) []byte {
	const frameLen = 417
	buf := []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0}
	buf = append(buf, 0xFF, 0xFB, 0x90, 0xC0)
	buf = append(buf, make([]byte, 17)...)
	buf = append(buf, 'I', 'n', 'f', 'o')
	buf = append(buf, 0, 0, 0, 0x0f)
	buf = append(buf, byte(declaredFrames>>24), byte(declaredFrames>>16), byte(declaredFrames>>8), byte(declaredFrames))
	buf = append(buf, 0, 0, 0, 0)
	buf = append(buf, make([]byte, 100)...)
	buf = append(buf, 0, 0, 0, 0)
	if wantTag {
		tag := make([]byte, 36)
		copy(tag, "Lavc63.1.101")
		tag[21], tag[22], tag[23] = 0x24, 0x06, 0x30
		buf = append(buf, tag...)
	}
	for len(buf) < 10+frameLen {
		buf = append(buf, 0)
	}
	for i := uint32(1); i < declaredFrames; i++ {
		buf = append(buf, 0xFF, 0xFB, 0x90, 0xC0)
		buf = append(buf, make([]byte, frameLen-4)...)
	}
	return buf
}
