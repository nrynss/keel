package caption_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nrynss/keel/caption"
)

// update regenerates the fixture files under testdata when set.
var update = os.Getenv("CAPTION_UPDATE") != ""

func compareFixture(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s mismatch\n got: %q\nwant: %q", name, string(got), string(want))
	}
}

func TestRenderFixtures(t *testing.T) {
	hourWords := []caption.Word{
		{Text: "exactly", Start: 3599 * time.Second, End: 3599*time.Second + 500*time.Millisecond},
		{Text: "on", Start: 3599*time.Second + 500*time.Millisecond, End: 3599*time.Second + 800*time.Millisecond},
		{Text: "the", Start: 3599*time.Second + 800*time.Millisecond, End: 3599*time.Second + 950*time.Millisecond},
		{Text: "hour", Start: time.Hour, End: time.Hour + 300*time.Millisecond},
	}
	markupWords := []caption.Word{
		{Text: "shows", Start: time.Second, End: 1500 * time.Millisecond},
		{Text: "<b>&", Start: 1500 * time.Millisecond, End: 2 * time.Second},
		{Text: "raw", Start: 2 * time.Second, End: 2500 * time.Millisecond},
	}
	longWord := []caption.Word{
		{
			Text:  "supercalifragilisticexpialidociousandthensomemoreletterstillylong",
			Start: 250 * time.Millisecond,
			End:   1500 * time.Millisecond,
		},
	}
	normalWords := []caption.Word{
		{Text: "the", Start: 100 * time.Millisecond, End: 200 * time.Millisecond},
		{Text: "quick", Start: 200 * time.Millisecond, End: 400 * time.Millisecond},
		{Text: "brown", Start: 400 * time.Millisecond, End: 600 * time.Millisecond},
		{Text: "fox", Start: 600 * time.Millisecond, End: 800 * time.Millisecond},
		{Text: "jumps", Start: 2 * time.Second, End: 2300 * time.Millisecond},
		{Text: "over", Start: 2300 * time.Millisecond, End: 2500 * time.Millisecond},
		{Text: "the", Start: 2500 * time.Millisecond, End: 2600 * time.Millisecond},
		{Text: "lazy", Start: 2600 * time.Millisecond, End: 2800 * time.Millisecond},
		{Text: "dog", Start: 2800 * time.Millisecond, End: 3 * time.Second},
	}

	cases := []struct {
		name  string
		words []caption.Word
		cfg   caption.Config
	}{
		{"hour", hourWords, caption.Config{}},
		{"markup", markupWords, caption.Config{}},
		{"empty", nil, caption.Config{}},
		{"longword", longWord, caption.Config{LineLength: 20, MaxDuration: time.Second}},
		{"normal", normalWords, caption.Config{LineLength: 15, MaxDuration: time.Second}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cues := caption.Group(tc.words, tc.cfg)

			var srt bytes.Buffer
			if err := caption.SRT(&srt, cues); err != nil {
				t.Fatalf("SRT: %v", err)
			}
			compareFixture(t, tc.name+".srt", srt.Bytes())

			var vtt bytes.Buffer
			if err := caption.WebVTT(&vtt, cues); err != nil {
				t.Fatalf("WebVTT: %v", err)
			}
			compareFixture(t, tc.name+".vtt", vtt.Bytes())
		})
	}
}

func TestGroupRespectsLimits(t *testing.T) {
	cfg := caption.Config{LineLength: 10, MaxDuration: time.Second}
	words := make([]caption.Word, 0, 40)
	start := 100 * time.Millisecond
	for range 40 {
		words = append(words, caption.Word{
			Text:  "word",
			Start: start,
			End:   start + 100*time.Millisecond,
		})
		start += 150 * time.Millisecond
	}
	cues := caption.Group(words, cfg)
	if len(cues) == 0 {
		t.Fatal("no cues")
	}
	got := 0
	for i, c := range cues {
		if c.End-c.Start > time.Second {
			t.Errorf("cue %d spans %v, over the cap", i, c.End-c.Start)
		}
		if len(c.Lines) == 0 {
			t.Errorf("cue %d is empty", i)
		}
		for _, l := range c.Lines {
			got += len(splitWords(l))
		}
	}
	if got != len(words) {
		t.Fatalf("%d words came back, want %d, so a word was split or lost", got, len(words))
	}
}

func splitWords(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func TestHourBoundary(t *testing.T) {
	cues := caption.Group([]caption.Word{
		{Text: "tick", Start: time.Hour, End: time.Hour + time.Second},
	}, caption.Config{})
	if len(cues) != 1 {
		t.Fatalf("got %d cues, want 1", len(cues))
	}
	if cues[0].Start != time.Hour || cues[0].End != time.Hour+time.Second {
		t.Errorf("cue boundaries drifted: %v to %v", cues[0].Start, cues[0].End)
	}
}

var errFail = errors.New("write failed")

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) {
	return 0, errFail
}

func TestWriteError(t *testing.T) {
	cues := caption.Group([]caption.Word{
		{Text: "tick", Start: 0, End: time.Second},
	}, caption.Config{})
	if err := caption.SRT(failWriter{}, cues); !errors.Is(err, errFail) {
		t.Errorf("SRT error = %v, want errFail", err)
	}
	if err := caption.WebVTT(failWriter{}, cues); !errors.Is(err, errFail) {
		t.Errorf("WebVTT error = %v, want errFail", err)
	}
}
