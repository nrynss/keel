package caption_test

import (
	"bytes"
	"fmt"
	"time"

	"github.com/nrynss/keel/caption"
)

// Example_group shows words grouped into cues under a line length and a
// duration cap, then rendered to both formats.
func Example_group() {
	words := []caption.Word{
		{Text: "hello", Start: 100 * time.Millisecond, End: 400 * time.Millisecond},
		{Text: "world", Start: 400 * time.Millisecond, End: 900 * time.Millisecond},
		{Text: "again", Start: 2 * time.Second, End: 2300 * time.Millisecond},
	}
	cues := caption.Group(words, caption.Config{LineLength: 12, MaxDuration: time.Second})

	var srt bytes.Buffer
	if err := caption.SRT(&srt, cues); err != nil {
		fmt.Println("srt failed:", err)
		return
	}
	fmt.Print(srt.String())

	var vtt bytes.Buffer
	if err := caption.WebVTT(&vtt, cues); err != nil {
		fmt.Println("vtt failed:", err)
		return
	}
	fmt.Print(vtt.String())

	// Output:
	// 1
	// 00:00:00,100 --> 00:00:00,900
	// hello world
	//
	// 2
	// 00:00:02,000 --> 00:00:02,300
	// again
	//
	// WEBVTT
	//
	// 1
	// 00:00:00.100 --> 00:00:00.900
	// hello world
	//
	// 2
	// 00:00:02.000 --> 00:00:02.300
	// again
	//
}

// Example_escape shows a word carrying markup characters, escaped for WebVTT
// and passed through unchanged by SRT.
func Example_escape() {
	cues := caption.Group([]caption.Word{
		{Text: "a<b&c", Start: time.Second, End: 1500 * time.Millisecond},
	}, caption.Config{})

	var srt bytes.Buffer
	_ = caption.SRT(&srt, cues) // a bytes.Buffer write cannot fail
	fmt.Print(srt.String())

	var vtt bytes.Buffer
	_ = caption.WebVTT(&vtt, cues) // a bytes.Buffer write cannot fail
	fmt.Print(vtt.String())

	// Output:
	// 1
	// 00:00:01,000 --> 00:00:01,500
	// a<b&c
	//
	// WEBVTT
	//
	// 1
	// 00:00:01.000 --> 00:00:01.500
	// a&lt;b&amp;c
	//
}
