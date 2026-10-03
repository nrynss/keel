package film

import (
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nrynss/keel/ffmpeg"
)

func TestLayoutCaptionShort(t *testing.T) {
	text := "Mira opens the garden gate."
	got := LayoutCaption(text)
	if got.FontSize != captionFontBase {
		t.Fatalf("size = %d", got.FontSize)
	}
	if strings.Join(got.Lines, " ") != text {
		t.Fatalf("lines = %#v", got.Lines)
	}
}

func TestLayoutCaptionShrinksThenTruncates(t *testing.T) {
	long := strings.Repeat("word ", 80)
	got := LayoutCaption(strings.TrimSpace(long))
	if got.FontSize != captionFontShrink {
		t.Fatalf("size = %d, want shrink", got.FontSize)
	}
	if len(got.Lines) != captionMaxLines {
		t.Fatalf("lines = %d", len(got.Lines))
	}
	if !strings.HasSuffix(got.Lines[len(got.Lines)-1], captionEllipsis) {
		t.Fatalf("last line %q", got.Lines[len(got.Lines)-1])
	}
}

func TestSilentHoldBounds(t *testing.T) {
	if SilentHold("") != silentHoldFloor || SilentHold("one") != silentHoldFloor {
		t.Fatal("floor")
	}
	if SilentHold(strings.Repeat("word ", 80)) != silentHoldCeiling {
		t.Fatal("ceiling")
	}
	// 10 words at 2 per second is 5s, inside the bounds.
	if got := SilentHold(strings.Repeat("word ", 10)); got != 5*time.Second {
		t.Fatalf("10 words = %s", got)
	}
}

func TestTotal(t *testing.T) {
	pages := []Page{
		{Text: "one"},
		{Text: "two", AudioPath: "a.mp3", Duration: 1500 * time.Millisecond},
	}
	got, err := Total(Config{}, pages)
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultTitleHold + DefaultEndHold + silentHoldFloor + 1500*time.Millisecond
	if got != want {
		t.Fatalf("total = %s, want %s", got, want)
	}
	if _, err := Total(Config{}, []Page{{Text: "x", Duration: time.Second}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("silent duration: %v", err)
	}
	if _, err := Total(Config{}, []Page{{Text: "x", Audio: []byte{1}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing duration: %v", err)
	}
	if _, err := Total(Config{}, []Page{{AudioPath: "a.mp3", Duration: time.Second}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing words: %v", err)
	}
}

func TestFormatByline(t *testing.T) {
	if got := formatByline(" Mira "); got != "by Mira" {
		t.Fatal(got)
	}
	if got := formatByline("by Mira"); got != "by Mira" {
		t.Fatal(got)
	}
	if formatByline("  ") != "" {
		t.Fatal("blank byline")
	}
}

func TestPageSegmentNarratedArgs(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args.txt")
	stub := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + "'" + strings.ReplaceAll(log, "'", "'\\''") + "'\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	font := filepath.Join(dir, "font.ttf")
	if err := os.WriteFile(font, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(dir, "still.png")
	if err := os.WriteFile(image, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	audio := filepath.Join(dir, "clip.mp3")
	if err := os.WriteFile(audio, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "page.mp4")
	err := PageSegment(context.Background(), ffmpeg.Tools{FFmpeg: stub}, Config{FontFile: font, WorkDir: dir},
		image, "hello there", audio, 1500*time.Millisecond, out)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	args := string(raw)
	if !strings.Contains(args, "-t\n1.500000\n") {
		t.Fatalf("missing explicit duration:\n%s", args)
	}
	if strings.Contains(args, "-shortest") {
		t.Fatalf("narrated segment used -shortest:\n%s", args)
	}
	if !strings.Contains(args, "textfile=") || !strings.Contains(args, "text_align=C") {
		t.Fatalf("caption filter missing:\n%s", args)
	}
}

func TestRenderRejectsMissingFont(t *testing.T) {
	_, err := Render(context.Background(), ffmpeg.Tools{}, Config{EndTitle: "End"}, Input{
		Title: "Story", Output: filepath.Join(t.TempDir(), "out.mp4"),
		Pages: []Page{{Text: "Hi", Image: []byte{1}}},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestRenderWithFFmpeg(t *testing.T) {
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(name); err != nil {
			if os.Getenv("KEEL_REQUIRE_FFMPEG") != "" {
				t.Fatal(err)
			}
			t.Skip(err)
		}
	}
	font := filepath.Join("testdata", "LiberationSans-Regular.ttf")
	if _, err := os.Stat(font); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	still := filepath.Join(dir, "still.png")
	writePNG(t, still)
	out := filepath.Join(dir, "film.mp4")
	cfg := Config{
		FontFile:  font,
		EndTitle:  "The end",
		Domain:    "example.com",
		TitleHold: 200 * time.Millisecond,
		EndHold:   200 * time.Millisecond,
		Artist:    "Keel",
	}
	pages := []Page{{Text: "Hello from the band.", ImagePath: still}}
	got, err := Render(context.Background(), ffmpeg.Tools{}, cfg, Input{
		Title: "A short film", Byline: "Ada", Pages: pages, Output: out,
	})
	if err != nil {
		t.Fatal(err)
	}
	want, err := Total(cfg, pages)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("render returned %s, total %s", got, want)
	}
	probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration:stream=codec_type,width,height", "-of", "default=nw=1", out).CombinedOutput()
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, probe)
	}
	text := string(probe)
	if !strings.Contains(text, "codec_type=video") || !strings.Contains(text, "codec_type=audio") {
		t.Fatalf("streams:\n%s", text)
	}
	if !strings.Contains(text, "width=1080") || !strings.Contains(text, "height=1620") {
		t.Fatalf("frame:\n%s", text)
	}
	// The joined file can run about one AAC frame past the arithmetic.
	d, err := ffmpeg.Duration(context.Background(), ffmpeg.Tools{}, out)
	if err != nil {
		t.Fatal(err)
	}
	if d < want || d > want+100*time.Millisecond {
		t.Fatalf("probed %s, arithmetic %s", d, want)
	}
}

func writePNG(t *testing.T, path string) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 32, 40))
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}
