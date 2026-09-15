package ffmpeg_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nrynss/keel/ffmpeg"
)

// ExampleCommand builds an ffmpeg invocation bound to a timeout and shows the
// program and arguments it will run.
func ExampleCommand() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := ffmpeg.Command(ctx, "/usr/local/bin/ffmpeg", "-y", "-i", "in.wav", "-c:a", "libmp3lame", "out.mp3")

	fmt.Println(cmd.Path)
	fmt.Println(strings.Join(cmd.Args[1:], " "))
	// Output:
	// /usr/local/bin/ffmpeg
	// -y -i in.wav -c:a libmp3lame out.mp3
}

// ExampleDuration runs ffprobe on an audio file and prints the duration it
// reports. The example drives a stub probe, so it runs on a host with no
// ffmpeg installed.
func ExampleDuration() {
	dir, err := os.MkdirTemp("", "keel-ffmpeg-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	probe := filepath.Join(dir, "ffprobe")
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  case \"$arg\" in\n" +
		"    format=duration) echo 1.500000; exit 0 ;;\n" +
		"  esac\n" +
		"done\n" +
		"exit 2\n"
	if err := os.WriteFile(probe, []byte(script), 0o755); err != nil {
		fmt.Println("write stub failed:", err)
		return
	}

	duration, err := ffmpeg.Duration(context.Background(), ffmpeg.Tools{FFprobe: probe}, "tone.wav")
	if err != nil {
		fmt.Println("probe failed:", err)
		return
	}

	fmt.Println(duration)
	// Output:
	// 1.5s
}
