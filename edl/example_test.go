package edl_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nrynss/keel/edl"
	"github.com/nrynss/keel/ffmpeg"
)

// ExampleRender renders a cut list with crossfades and two-pass loudness.
// The example drives stub tools, so it runs on a host with no ffmpeg
// installed. The stub answers a measure pass, the one carrying stats_file,
// with fixed loudnorm stats and touches the output file on a render pass.
func ExampleRender() {
	dir, err := os.MkdirTemp("", "keel-edl-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	ffmpegStub := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\n" +
		"stats=\n" +
		"out=\n" +
		"for arg in \"$@\"; do\n" +
		"  case \"$arg\" in\n" +
		"    *stats_file=*) stats=${arg#*stats_file=}\n" +
		"      stats=${stats%%\\[*} ;;\n" +
		"    *.wav) out=$arg ;;\n" +
		"  esac\n" +
		"done\n" +
		"if [ -n \"$stats\" ]; then\n" +
		"  cat > \"$stats\" <<'JSON'\n" +
		"{\n" +
		"\t\"input_i\" : \"-21.75\",\n" +
		"\t\"input_lra\" : \"0.00\",\n" +
		"\t\"input_tp\" : \"-18.06\",\n" +
		"\t\"input_thresh\" : \"-31.75\"\n" +
		"}\n" +
		"JSON\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ -n \"$out\" ]; then : > \"$out\"; exit 0; fi\n" +
		"exit 2\n"
	if err := os.WriteFile(ffmpegStub, []byte(script), 0o755); err != nil {
		fmt.Println("write stub failed:", err)
		return
	}

	probeStub := filepath.Join(dir, "ffprobe")
	probeScript := "#!/bin/sh\n" +
		"cat <<'JSON'\n" +
		`{"streams":[{"codec_name":"pcm_s16le","sample_rate":"48000","channels":1,"channel_layout":"mono"}],"format":{}}` + "\n" +
		"JSON\n"
	if err := os.WriteFile(probeStub, []byte(probeScript), 0o755); err != nil {
		fmt.Println("write stub failed:", err)
		return
	}

	src := filepath.Join(dir, "interview.wav")
	dst := filepath.Join(dir, "cut.wav")
	err = edl.Render(context.Background(),
		ffmpeg.Tools{FFmpeg: ffmpegStub, FFprobe: probeStub},
		src,
		edl.Config{Crossfade: 250 * time.Millisecond, Target: -16},
		dst,
		edl.Segment{Start: 0, End: 90 * time.Second},
		edl.Segment{Start: 95 * time.Second, End: 210 * time.Second})
	if err != nil {
		fmt.Println("render failed:", err)
		return
	}
	fmt.Println("rendered", filepath.Base(dst))
	// Output:
	// rendered cut.wav
}
