// Package caption renders word timings as SRT and WebVTT subtitles.
//
// Words carry a text and a start and end time. Group joins words into cues
// that respect a line length and a maximum duration, and never splits a word.
// One cue list renders to either format through SRT or WebVTT.
//
// All times are time.Duration values, so formatting never sees a float and
// cannot drift at an hour boundary.
package caption

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Default line length in characters when Config.LineLength is not set.
const defaultLineLength = 42

// Default maximum cue duration when Config.MaxDuration is not set.
const defaultMaxDuration = 5 * time.Second

// Word is one transcribed word with its timing.
type Word struct {
	// Text is the word as spoken, without surrounding whitespace.
	Text string
	// Start is the offset from the start of the media.
	Start time.Duration
	// End is the offset from the start of the media. It must not be before Start.
	End time.Duration
}

// Cue is one subtitle block. Lines holds one or more display lines. A cue
// never carries a word split across lines.
type Cue struct {
	Start time.Duration
	End   time.Duration
	Lines []string
}

// Config controls how words group into cues. The zero value is usable and
// gives a line length of 42 characters and a maximum duration of 5 seconds.
type Config struct {
	// LineLength is the maximum character count of one display line.
	LineLength int
	// MaxDuration is the longest time span one cue may cover.
	MaxDuration time.Duration
}

func (c Config) lineLength() int {
	if c.LineLength <= 0 {
		return defaultLineLength
	}
	return c.LineLength
}

func (c Config) maxDuration() time.Duration {
	if c.MaxDuration <= 0 {
		return defaultMaxDuration
	}
	return c.MaxDuration
}

// Group joins words into cues. A word is never split and never crosses a cue
// boundary. A word longer than the line length gets a line of its own, and
// its own cue when adding a neighbour would exceed the limits.
func Group(words []Word, cfg Config) []Cue {
	limit := cfg.lineLength()
	maxDur := cfg.maxDuration()

	var cues []Cue
	var cur *Cue
	var line []string
	lineLen := 0

	closeLine := func() {
		if len(line) == 0 {
			return
		}
		cur.Lines = append(cur.Lines, strings.Join(line, " "))
		line = nil
		lineLen = 0
	}
	closeCue := func() {
		if cur == nil {
			return
		}
		closeLine()
		cues = append(cues, *cur)
		cur = nil
	}

	for _, w := range words {
		if w.End < w.Start {
			w.Start, w.End = w.End, w.Start
		}
		if cur != nil && w.End-cur.Start > maxDur {
			closeCue()
		}
		if cur == nil {
			cur = &Cue{Start: w.Start}
		}
		needed := len(w.Text)
		if len(line) > 0 {
			needed++ // one joining space
		}
		if len(line) > 0 && lineLen+needed > limit {
			closeLine()
		}
		line = append(line, w.Text)
		lineLen += needed
		cur.End = w.End
	}
	closeCue()
	return cues
}

// formatTime renders a duration as hh:mm:ss,mmm around the given decimal
// separator, using integer arithmetic. An hour boundary reads exactly
// 01:00:00,000 with no floating point drift.
func formatTime(d time.Duration, decimal string) string {
	if d < 0 {
		d = 0
	}
	ms := int64(d / time.Millisecond)
	hours := ms / 3_600_000
	minutes := (ms / 60_000) % 60
	secs := (ms / 1000) % 60
	millis := ms % 1000
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", hours, minutes, secs, decimal, millis)
}

type renderSpec struct {
	decimal string
	header  string
	number  bool
	escape  func(string) string
}

func writeCues(w io.Writer, cues []Cue, spec renderSpec) error {
	if spec.header != "" {
		if _, err := io.WriteString(w, spec.header); err != nil {
			return fmt.Errorf("writing header: %w", err)
		}
	}
	for i, c := range cues {
		var b []byte
		if spec.number {
			b = append(b, []byte(fmt.Sprintf("%d\n", i+1))...)
		}
		b = append(b, formatTime(c.Start, spec.decimal)...)
		b = append(b, " --> "...)
		b = append(b, formatTime(c.End, spec.decimal)...)
		b = append(b, '\n')
		for j, l := range c.Lines {
			b = append(b, spec.escape(l)...)
			if j < len(c.Lines)-1 {
				b = append(b, '\n')
			}
		}
		b = append(b, '\n', '\n')
		if _, err := w.Write(b); err != nil {
			return fmt.Errorf("writing cue %d: %w", i+1, err)
		}
	}
	return nil
}
