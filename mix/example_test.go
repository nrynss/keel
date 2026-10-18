package mix_test

import (
	"fmt"
	"strings"
	"time"

	"github.com/nrynss/keel/mix"
)

// ExampleBuild lays one line of dialogue over a music bed that ducks under
// it, and prints the work the render will ask ffmpeg for. Build is pure,
// so the example runs without ffmpeg; Render runs this same graph.
func ExampleBuild() {
	tl := mix.Timeline{
		Tracks: []mix.Track{{Name: "dialogue", Clips: []mix.Clip{{
			Source: "line-12.wav", Length: 2 * time.Second, Offset: time.Second,
			FadeIn: 20 * time.Millisecond, FadeOut: 50 * time.Millisecond,
		}}}},
		Bed:    &mix.Bed{Source: "score.mp3", Gain: -12, DuckOn: []string{"dialogue"}},
		Length: 4 * time.Second,
	}
	g, err := mix.Build(tl, mix.Config{SampleRate: 8000, Layout: "mono"})
	if err != nil {
		fmt.Println("build failed:", err)
		return
	}
	for _, in := range g.Inputs {
		fmt.Printf("input %s loop=%v\n", in.Source, in.Loop)
	}
	fmt.Println("length:", g.Length, "samples:", g.Samples)
	for _, chain := range strings.Split(g.Filter, ";") {
		fmt.Println(chain)
	}
	// Output:
	// input line-12.wav loop=false
	// input score.mp3 loop=true
	// length: 4s samples: 32000
	// [0:a]aresample=8000,aformat=sample_fmts=fltp:channel_layouts=mono,atrim=start_sample=0:end_sample=16000,asetpts=PTS-STARTPTS,apad=whole_len=16000,atrim=end_sample=16000,afade=t=in:ss=0:ns=160,afade=t=out:ss=15600:ns=400,adelay=delays=8000S:all=1[trk0clip0]
	// [trk0clip0]anull[trk0]
	// [trk0]asplit=2[trk0mix][trk0key]
	// [1:a]aresample=8000,aformat=sample_fmts=fltp:channel_layouts=mono,atrim=end_sample=32000,asetpts=PTS-STARTPTS,apad=whole_len=32000,volume=-12dB,asetnsamples=n=1024:p=0[bedraw]
	// [trk0key]anull[keysum]
	// [keysum]apad=whole_len=32000,atrim=end_sample=32000,asetnsamples=n=1024:p=0[key]
	// [bedraw][key]sidechaincompress=threshold=0.03162277660168379:ratio=8:attack=20:release=400[bed]
	// [trk0mix][bed]amix=inputs=2:duration=longest:dropout_transition=0:normalize=0[summed]
	// [summed]apad=whole_len=32000,atrim=end_sample=32000[out]
}
