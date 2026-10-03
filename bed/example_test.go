package bed_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nrynss/keel/bed"
	"github.com/nrynss/keel/ffmpeg"
)

func ExampleMix() {
	dir, err := os.MkdirTemp("", "keel-bed-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	stub := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\nout=\"\"\nfor arg in \"$@\"; do out=\"$arg\"; done\n: > \"$out\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		fmt.Println("write stub failed:", err)
		return
	}
	err = bed.Mix(context.Background(), ffmpeg.Tools{FFmpeg: stub}, bed.Config{}, bed.Input{
		Film:     "film.mp4",
		Bed:      "bed.mp3",
		Duration: 30 * time.Second,
		Output:   filepath.Join(dir, "mixed.mp4"),
	})
	if err != nil {
		fmt.Println("mix failed:", err)
		return
	}
	fmt.Println("mixed")
	// Output:
	// mixed
}
