package fit_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nrynss/keel/ffmpeg"
	"github.com/nrynss/keel/fit"
)

// ExampleAtempoChain builds the chain for a tempo change too large for
// one atempo stage.
func ExampleAtempoChain() {
	for _, ratio := range []float64{1.1, 3, 0.3} {
		chain, err := fit.AtempoChain(ratio)
		if err != nil {
			fmt.Println("chain failed:", err)
			return
		}
		fmt.Printf("%v: %s\n", ratio, chain)
	}
	// Output:
	// 1.1: atempo=1.1
	// 3: atempo=2,atempo=1.5
	// 0.3: atempo=0.5,atempo=0.6
}

// ExampleFit shows the refusal a caller acts on: a three second take
// cannot fill a two second slot within the default bounds, so the error
// carries the ratio it would need and the caller decides what to do. The
// stub ffprobe reports the take's length and format, so the example runs
// on a host with no ffmpeg installed; a refused fit never runs ffmpeg.
func ExampleFit() {
	dir, err := os.MkdirTemp("", "keel-fit-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)
	stub := filepath.Join(dir, "ffprobe")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *format=duration*) echo 3.000000 ;;\n" +
		"  *) echo '{\"streams\":[{\"codec_name\":\"pcm_s16le\",\"sample_rate\":\"48000\",\"channels\":1}],\"format\":{}}' ;;\n" +
		"esac\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		fmt.Println("write stub failed:", err)
		return
	}
	tools := ffmpeg.Tools{FFprobe: stub, FFmpeg: filepath.Join(dir, "no-ffmpeg")}

	_, err = fit.Fit(context.Background(), tools,
		filepath.Join(dir, "line-12.wav"), filepath.Join(dir, "line-12-fitted.wav"),
		2*time.Second, fit.Bounds{})
	var re *fit.RatioError
	if errors.As(err, &re) {
		fmt.Printf("needs %.2f, fastest allowed %.2f: resynthesize\n", re.Ratio, re.Bounds.Fastest)
	}
	// Output:
	// needs 1.50, fastest allowed 1.25: resynthesize
}
