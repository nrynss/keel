package film

import (
	"strings"
	"time"
	"unicode/utf8"
)

// The frame is one size for every segment so a concat can copy the streams.
// The page is the illustration on top and a caption band beneath it.
const (
	frameWidth  = 1080
	frameHeight = 1620
	artWidth    = 1080
	artHeight   = 1350
	bandHeight  = frameHeight - artHeight

	surfaceColor = "0xd8efe3"
	inkColor     = "0x17332b"
	filmColor    = "0x12241e"
)

const (
	captionMaxLines    = 3
	captionFontBase    = 60
	captionFontShrink  = 52
	captionSideMargins = 40
	captionAdvanceEm   = 0.55
	captionEllipsis    = "…"
	silentWordsPerSec  = 2.0
	silentHoldFloor    = 4 * time.Second
	silentHoldCeiling  = 14 * time.Second
)

// Caption is the wrapped band for one page: the drawtext size that fits it
// and the lines written to the text file.
type Caption struct {
	// FontSize is the drawtext size, in pixels.
	FontSize int
	// Lines are the wrapped lines, in order. A long caption ends its last
	// line with an ellipsis.
	Lines []string
}

// LayoutCaption wraps text for the caption band. It fits as many words as
// possible in three lines at the base size, shrinks one step when that
// overflows, and then truncates at a word boundary with an ellipsis. Words
// are never broken. An empty text returns the zero Caption.
func LayoutCaption(text string) Caption {
	if text == "" {
		return Caption{}
	}
	base := wrapText(text, maxRunesAt(captionFontBase))
	if len(base) <= captionMaxLines {
		return Caption{FontSize: captionFontBase, Lines: base}
	}
	shrunk := wrapText(text, maxRunesAt(captionFontShrink))
	if len(shrunk) <= captionMaxLines {
		return Caption{FontSize: captionFontShrink, Lines: shrunk}
	}
	return Caption{FontSize: captionFontShrink, Lines: truncateWithEllipsis(shrunk)}
}

// SilentHold is how long a page with no narration stays up: one second for
// every two words of text, and never less than four seconds or more than
// fourteen. Empty text holds the floor.
func SilentHold(text string) time.Duration {
	words := len(strings.Fields(text))
	d := time.Duration(float64(words) / silentWordsPerSec * float64(time.Second))
	if d < silentHoldFloor {
		return silentHoldFloor
	}
	if d > silentHoldCeiling {
		return silentHoldCeiling
	}
	return d
}

// maxRunesAt returns how many runes fit one caption line at fontSize.
func maxRunesAt(fontSize int) int {
	usable := frameWidth - 2*captionSideMargins
	return int(float64(usable) / (float64(fontSize) * captionAdvanceEm))
}

// wrapText breaks text into lines of at most maxRunes runes, only at spaces.
// A word longer than maxRunes keeps a line of its own.
func wrapText(text string, maxRunes int) []string {
	if maxRunes < 1 {
		maxRunes = 1
	}
	var lines []string
	current := ""
	for _, word := range strings.Fields(text) {
		candidate := word
		if current != "" {
			candidate = current + " " + word
		}
		if utf8.RuneCountInString(candidate) <= maxRunes {
			current = candidate
			continue
		}
		if current != "" {
			lines = append(lines, current)
		}
		current = word
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

// truncateWithEllipsis keeps the first three lines and ends the last one
// with an ellipsis, dropping trailing words so the marker fits.
func truncateWithEllipsis(wrapped []string) []string {
	if len(wrapped) <= captionMaxLines {
		return wrapped
	}
	kept := append([]string(nil), wrapped[:captionMaxLines]...)
	maxRunes := maxRunesAt(captionFontShrink)
	last := len(kept) - 1
	for utf8.RuneCountInString(kept[last])+utf8.RuneCountInString(captionEllipsis) > maxRunes {
		line := kept[last]
		if i := strings.LastIndex(line, " "); i >= 0 {
			kept[last] = line[:i]
			continue
		}
		kept = kept[:last]
		if len(kept) == 0 {
			return []string{captionEllipsis}
		}
		last = len(kept) - 1
	}
	kept[last] += captionEllipsis
	return kept
}
