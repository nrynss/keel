package mix_test

import (
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nrynss/keel/mix"
)

// TestBuildPinsTheGraph pins the whole graph for a small timeline: one
// clip with every stage, an untouched clip, and no bed.
func TestBuildPinsTheGraph(t *testing.T) {
	tl := mix.Timeline{Tracks: []mix.Track{{Name: "fx", Clips: []mix.Clip{
		{Source: "hit.wav", In: 250 * time.Millisecond, Length: time.Second, Offset: 2 * time.Second,
			Gain: -6, FadeIn: 10 * time.Millisecond, FadeOut: 100 * time.Millisecond},
		{Source: "door.wav", Length: 500 * time.Millisecond},
	}}}}
	g, err := mix.Build(tl, mix.Config{SampleRate: 8000, Layout: "mono"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	conform := "aresample=8000,aformat=sample_fmts=fltp:channel_layouts=mono,"
	want := strings.Join([]string{
		"[0:a]" + conform + "atrim=start_sample=0:end_sample=4000,asetpts=PTS-STARTPTS,apad=whole_len=4000,atrim=end_sample=4000[trk0clip0]",
		"[1:a]" + conform + "atrim=start_sample=2000:end_sample=10000,asetpts=PTS-STARTPTS,apad=whole_len=8000,atrim=end_sample=8000," +
			"volume=-6dB,afade=t=in:ss=0:ns=80,afade=t=out:ss=7200:ns=800,adelay=delays=16000S:all=1[trk0clip1]",
		"[trk0clip0][trk0clip1]amix=inputs=2:duration=longest:dropout_transition=0:normalize=0[trk0]",
		"[trk0]anull[summed]",
		"[summed]apad=whole_len=24000,atrim=end_sample=24000[out]",
	}, ";")
	if g.Filter != want {
		t.Errorf("Filter =\n%s\nwant\n%s", g.Filter, want)
	}
	if !slices.Equal(g.Inputs, []mix.Input{{Source: "door.wav"}, {Source: "hit.wav"}}) {
		t.Errorf("Inputs = %+v, want the clips in offset order", g.Inputs)
	}
	if g.Samples != 24000 || g.Length != 3*time.Second {
		t.Errorf("length = %d samples, %v, want 24000 and 3s", g.Samples, g.Length)
	}
	if !strings.HasSuffix(g.Filter, "["+mix.OutputLabel+"]") {
		t.Errorf("Filter does not end in [%s]", mix.OutputLabel)
	}
}

// TestBuildPinsTheDuck pins the bed's chain: looped, conformed, cut to the
// timeline, gained, and compressed under the sum of its keyed tracks with
// the default duck settings, while an unkeyed track only joins the mix.
func TestBuildPinsTheDuck(t *testing.T) {
	tl := mix.Timeline{
		Tracks: []mix.Track{
			{Name: "voice", Clips: []mix.Clip{{Source: "line.wav", Length: time.Second}}},
			{Name: "alt", Clips: []mix.Clip{{Source: "alt.wav", Length: time.Second, Offset: time.Second}}},
			{Name: "fx", Clips: []mix.Clip{{Source: "hit.wav", Length: time.Second}}},
		},
		Bed:    &mix.Bed{Source: "music.mp3", Gain: -12, DuckOn: []string{"voice", "alt"}},
		Length: 4 * time.Second,
	}
	g, err := mix.Build(tl, mix.Config{SampleRate: 8000, Layout: "mono"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if last := g.Inputs[len(g.Inputs)-1]; last != (mix.Input{Source: "music.mp3", Loop: true}) {
		t.Errorf("last input = %+v, want the looped bed", last)
	}
	for _, part := range []string{
		"[trk0]asplit=2[trk0mix][trk0key]", // alt
		"[trk2]asplit=2[trk2mix][trk2key]", // voice
		"[3:a]aresample=8000,aformat=sample_fmts=fltp:channel_layouts=mono,atrim=end_sample=32000,asetpts=PTS-STARTPTS,apad=whole_len=32000,volume=-12dB,asetnsamples=n=1024:p=0[bedraw]",
		"[trk0key][trk2key]amix=inputs=2:duration=longest:dropout_transition=0:normalize=0[keysum]",
		"[keysum]apad=whole_len=32000,atrim=end_sample=32000,asetnsamples=n=1024:p=0[key]",
		"[bedraw][key]sidechaincompress=threshold=0.03162277660168379:ratio=8:attack=20:release=400[bed]",
		"[trk0mix][trk1][trk2mix][bed]amix=inputs=4:duration=longest:dropout_transition=0:normalize=0[summed]",
		"[summed]apad=whole_len=32000,atrim=end_sample=32000[out]",
	} {
		if !strings.Contains(g.Filter, part) {
			t.Errorf("Filter lacks %s\nin %s", part, g.Filter)
		}
	}
	if strings.Contains(g.Filter, "[trk1]asplit") {
		t.Error("the unkeyed fx track was split for the key")
	}

	tl.Bed.DuckOn = nil
	g, err = mix.Build(tl, mix.Config{SampleRate: 8000, Layout: "mono", Duck: mix.Duck{Threshold: -20}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(g.Filter, "sidechaincompress") || !strings.Contains(g.Filter, "volume=-12dB[bed]") {
		t.Errorf("a bed keyed on nothing must not duck: %s", g.Filter)
	}
}

// TestBuildIgnoresTheOrderGiven checks that shuffled tracks and clips
// build the same graph, and that Build leaves the caller's slices alone.
func TestBuildIgnoresTheOrderGiven(t *testing.T) {
	a := mix.Clip{Source: "a.wav", Length: time.Second}
	b := mix.Clip{Source: "b.wav", Length: time.Second, Offset: time.Second}
	b2 := mix.Clip{Source: "a.wav", Length: time.Second, Offset: time.Second}
	c := mix.Clip{Source: "c.wav", Length: 2 * time.Second, Gain: 3}
	one := mix.Timeline{Tracks: []mix.Track{
		{Name: "x", Clips: []mix.Clip{a, b, b2}},
		{Name: "y", Clips: []mix.Clip{c}},
	}}
	two := mix.Timeline{Tracks: []mix.Track{
		{Name: "y", Clips: []mix.Clip{c}},
		{Name: "x", Clips: []mix.Clip{b, b2, a}},
	}}
	before := slices.Clone(two.Tracks[1].Clips)
	g1, err := mix.Build(one, mix.Config{})
	if err != nil {
		t.Fatal(err)
	}
	g2, err := mix.Build(two, mix.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g1, g2) {
		t.Errorf("the order given changed the graph:\n%s\n%s", g1.Filter, g2.Filter)
	}
	if !slices.Equal(two.Tracks[1].Clips, before) || two.Tracks[0].Name != "y" {
		t.Error("Build reordered the caller's timeline")
	}
}

// TestBuildRefusesWhatItCannotPlace checks every refusal names ErrInvalid.
func TestBuildRefusesWhatItCannotPlace(t *testing.T) {
	clip := mix.Clip{Source: "a.wav", Length: time.Second}
	track := func(clips ...mix.Clip) []mix.Track { return []mix.Track{{Name: "x", Clips: clips}} }
	with := func(f func(*mix.Clip)) []mix.Track {
		c := clip
		f(&c)
		return track(c)
	}
	tests := []struct {
		name string
		tl   mix.Timeline
		cfg  mix.Config
	}{
		{name: "no tracks", tl: mix.Timeline{}},
		{name: "no clips", tl: mix.Timeline{Tracks: track()}},
		{name: "no name", tl: mix.Timeline{Tracks: []mix.Track{{Clips: []mix.Clip{clip}}}}},
		{name: "repeated name", tl: mix.Timeline{Tracks: append(track(clip), track(clip)...)}},
		{name: "no source", tl: mix.Timeline{Tracks: with(func(c *mix.Clip) { c.Source = " " })}},
		{name: "negative in", tl: mix.Timeline{Tracks: with(func(c *mix.Clip) { c.In = -1 })}},
		{name: "negative offset", tl: mix.Timeline{Tracks: with(func(c *mix.Clip) { c.Offset = -1 })}},
		{name: "zero length", tl: mix.Timeline{Tracks: with(func(c *mix.Clip) { c.Length = 0 })}},
		{name: "negative fade", tl: mix.Timeline{Tracks: with(func(c *mix.Clip) { c.FadeIn = -1 })}},
		{name: "fades overrun", tl: mix.Timeline{Tracks: with(func(c *mix.Clip) { c.FadeIn, c.FadeOut = 600*time.Millisecond, 600*time.Millisecond })}},
		{name: "gain NaN", tl: mix.Timeline{Tracks: with(func(c *mix.Clip) { c.Gain = math.NaN() })}},
		{name: "gain infinite", tl: mix.Timeline{Tracks: with(func(c *mix.Clip) { c.Gain = math.Inf(-1) })}},
		{name: "negative length", tl: mix.Timeline{Tracks: track(clip), Length: -time.Second}},
		{name: "bed without source", tl: mix.Timeline{Tracks: track(clip), Bed: &mix.Bed{}}},
		{name: "bed gain NaN", tl: mix.Timeline{Tracks: track(clip), Bed: &mix.Bed{Source: "m.mp3", Gain: math.NaN()}}},
		{name: "duck on a missing track", tl: mix.Timeline{Tracks: track(clip), Bed: &mix.Bed{Source: "m.mp3", DuckOn: []string{"voice"}}}},
		{name: "a clip too short for one sample", tl: mix.Timeline{Tracks: with(func(c *mix.Clip) { c.Length = time.Nanosecond })}},
		{name: "a short clip among long ones", tl: mix.Timeline{Tracks: track(clip, mix.Clip{Source: "b.wav", Length: time.Nanosecond})}},
		{name: "negative rate", tl: mix.Timeline{Tracks: track(clip)}, cfg: mix.Config{SampleRate: -1}},
		{name: "layout that breaks the graph", tl: mix.Timeline{Tracks: track(clip)}, cfg: mix.Config{Layout: "stereo[x]"}},
		{name: "threshold above 0", tl: mix.Timeline{Tracks: track(clip)}, cfg: mix.Config{Duck: mix.Duck{Threshold: 3}}},
		{name: "threshold below -60", tl: mix.Timeline{Tracks: track(clip)}, cfg: mix.Config{Duck: mix.Duck{Threshold: -61}}},
		{name: "ratio below 1", tl: mix.Timeline{Tracks: track(clip)}, cfg: mix.Config{Duck: mix.Duck{Ratio: 0.5}}},
		{name: "attack too long", tl: mix.Timeline{Tracks: track(clip)}, cfg: mix.Config{Duck: mix.Duck{Attack: 3 * time.Second}}},
		{name: "negative release", tl: mix.Timeline{Tracks: track(clip)}, cfg: mix.Config{Duck: mix.Duck{Release: -time.Second}}},
		{name: "target out of range", tl: mix.Timeline{Tracks: track(clip)}, cfg: mix.Config{Target: -2}},
		{name: "target NaN", tl: mix.Timeline{Tracks: track(clip)}, cfg: mix.Config{Target: math.NaN()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := mix.Build(tt.tl, tt.cfg); !errors.Is(err, mix.ErrInvalid) {
				t.Errorf("Build error = %v, want ErrInvalid", err)
			}
		})
	}
}
