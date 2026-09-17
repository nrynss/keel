package caption

import (
	"io"
	"strings"
)

// srtSpec renders SubRip text. SRT is plain text, so nothing is escaped. A
// line break inside a word text stays a line break, which SRT allows inside
// one cue.
func srtSpec() renderSpec {
	return renderSpec{
		decimal: ",",
		number:  true,
		escape:  func(s string) string { return s },
	}
}

// SRT writes cues as SubRip text to w. Cues number from one, and times use a
// comma before the milliseconds. The writer receives the exact bytes, with no
// trailing blank line beyond the one that closes the last cue.
func SRT(w io.Writer, cues []Cue) error {
	return writeCues(w, cues, srtSpec())
}

// vttSpec renders WebVTT text. A literal < starts a tag in cue text and a
// literal & starts a character reference, so both are escaped. A line break
// inside a word text stays a line break, which a cue text block allows.
func vttSpec() renderSpec {
	return renderSpec{
		decimal: ".",
		header:  "WEBVTT\n\n",
		number:  true,
		escape:  vttEscape,
	}
}

func vttEscape(s string) string {
	if !strings.ContainsAny(s, "<&\n\r") {
		return s
	}
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		"\r\n", "\n",
		"\r", "\n",
	)
	return r.Replace(s)
}

// WebVTT writes cues in the WebVTT format to w. The output starts with the
// WEBVTT header, and times use a full stop before the milliseconds.
func WebVTT(w io.Writer, cues []Cue) error {
	return writeCues(w, cues, vttSpec())
}
