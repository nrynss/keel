package waveform_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nrynss/keel/ffmpeg"
	"github.com/nrynss/keel/waveform"
)

// ExampleRender renders a still and an audio file into one video with the
// default frame of 1920 by 1080 pixels. The stub ffmpeg only creates the
// output file, so the example runs on a host without the binaries.
func ExampleRender() {
	dir, err := os.MkdirTemp("", "keel-waveform-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	stub := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\n" +
		"out=\"\"\n" +
		"for arg in \"$@\"; do out=\"$arg\"; done\n" +
		": > \"$out\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		fmt.Println("write stub failed:", err)
		return
	}

	err = waveform.Render(context.Background(), ffmpeg.Tools{FFmpeg: stub}, waveform.Config{},
		"still.png", "tone.wav", filepath.Join(dir, "out.mkv"))
	if err != nil {
		fmt.Println("render failed:", err)
		return
	}
	fmt.Println("rendered out.mkv")
	// Output:
	// rendered out.mkv
}
