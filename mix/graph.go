package mix

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nrynss/keel/internal/loudness"
)

// duckFrames cuts a stream into fixed frames of 1024 samples, the last one
// shorter, so the sidechain compressor reads both inputs in the same
// chunks on every run.
const duckFrames = "asetnsamples=n=1024:p=0"

// OutputLabel is the label of the stream Graph.Filter ends in, so a caller
// running the graph itself maps "[out]".
const OutputLabel = "out"

// Input is one ffmpeg input of a graph, in the order the filter counts
// them.
type Input struct {
	// Source is the file read.
	Source string
	// Loop reports that the input repeats until the graph stops reading,
	// as the bed does.
	Loop bool
}

// Graph is the work one render asks ffmpeg for. It is a pure function of
// the timeline and the config, so it is unit tested without ffmpeg and the
// same timeline always yields the same graph.
type Graph struct {
	// Inputs are the files the filter reads, input i being [i:a]: every
	// clip in sorted order, then the bed.
	Inputs []Input
	// Filter is the filter_complex graph. Its last stream is labelled
	// OutputLabel.
	Filter string
	// Samples is the rendered length in samples at the output rate.
	Samples int64
	// Length is Samples as a duration, rounded to the nearest nanosecond.
	Length time.Duration
}

// args renders the inputs as ffmpeg arguments.
func (g Graph) args() []string {
	var args []string
	for _, in := range g.Inputs {
		if in.Loop {
			args = append(args, "-stream_loop", "-1")
		}
		args = append(args, "-i", in.Source)
	}
	return args
}

// format resolves the output rate and layout.
func (c Config) format() (int, string) {
	rate, layout := c.SampleRate, c.Layout
	if rate == 0 {
		rate = DefaultSampleRate
	}
	if layout == "" {
		layout = DefaultLayout
	}
	return rate, layout
}

// duck resolves the duck settings.
func (c Config) duck() Duck {
	d := c.Duck
	if d.Threshold == 0 {
		d.Threshold = DefaultDuckThreshold
	}
	if d.Ratio == 0 {
		d.Ratio = DefaultDuckRatio
	}
	if d.Attack == 0 {
		d.Attack = DefaultDuckAttack
	}
	if d.Release == 0 {
		d.Release = DefaultDuckRelease
	}
	return d
}

// validate checks the config on its own.
func (c Config) validate() error {
	rate, layout := c.format()
	if rate < 1 || rate > 768000 {
		return fmt.Errorf("%w: sample rate %d must be from 1 to 768000", ErrInvalid, c.SampleRate)
	}
	if strings.ContainsAny(layout, ":,;[]='\"\\ \t\n") {
		return fmt.Errorf("%w: channel layout %q is not a layout name", ErrInvalid, layout)
	}
	d := c.duck()
	if !(d.Threshold >= -60 && d.Threshold < 0) {
		return fmt.Errorf("%w: duck threshold %v dBFS must be from -60 to below 0", ErrInvalid, c.Duck.Threshold)
	}
	if !(d.Ratio >= 1 && d.Ratio <= 20) {
		return fmt.Errorf("%w: duck ratio %v must be from 1 to 20", ErrInvalid, c.Duck.Ratio)
	}
	if d.Attack < 10*time.Microsecond || d.Attack > 2*time.Second {
		return fmt.Errorf("%w: duck attack %s must be from 0.01ms to 2s", ErrInvalid, c.Duck.Attack)
	}
	if d.Release < 10*time.Microsecond || d.Release > 9*time.Second {
		return fmt.Errorf("%w: duck release %s must be from 0.01ms to 9s", ErrInvalid, c.Duck.Release)
	}
	if !loudness.ValidTarget(c.Target) {
		return fmt.Errorf("%w: loudness target %v LUFS must be from -70 to -5", ErrInvalid, c.Target)
	}
	return nil
}

// validGain reports whether a gain in decibels is a finite number.
func validGain(db float64) bool {
	return !math.IsNaN(db) && !math.IsInf(db, 0)
}

// compareClips orders clips by offset, then by every other field, so the
// order never depends on the order the caller listed them in.
func compareClips(a, b Clip) int {
	return cmp.Or(
		cmp.Compare(a.Offset, b.Offset),
		cmp.Compare(a.In, b.In),
		cmp.Compare(a.Length, b.Length),
		cmp.Compare(a.Source, b.Source),
		cmp.Compare(a.Gain, b.Gain),
		cmp.Compare(a.FadeIn, b.FadeIn),
		cmp.Compare(a.FadeOut, b.FadeOut),
	)
}

// Build validates the timeline and returns the graph that renders it. It
// copies what it sorts, so the caller's timeline is left as it was. Every
// time is counted in samples at the output rate, rounded once to the
// nearest sample.
//
// Each clip is resampled to the output rate and layout, cut to its range,
// padded with silence to its Length when the source ends sooner, gained,
// faded, and delayed to its offset. Clips sum into their track, the bed
// ducks under the sum of its keyed tracks, and everything sums into one
// stream padded or cut to the timeline's length.
//
// A timeline without tracks, a track without clips, an empty or repeated
// track name, an empty source, a negative In or Offset, a Length that is
// not positive or rounds to no sample, fades that are negative or do not
// fit their clip, a gain that is not finite, a negative timeline Length, a
// bed keyed on a track that does not exist, or a config out of range
// reports ErrInvalid.
func Build(tl Timeline, cfg Config) (Graph, error) {
	if err := cfg.validate(); err != nil {
		return Graph{}, err
	}
	if len(tl.Tracks) == 0 {
		return Graph{}, fmt.Errorf("%w: a timeline needs at least one track", ErrInvalid)
	}
	if tl.Length < 0 {
		return Graph{}, fmt.Errorf("%w: timeline length %s must not be negative", ErrInvalid, tl.Length)
	}
	rate, layout := cfg.format()
	samples := func(d time.Duration) int64 {
		return int64(math.Round(d.Seconds() * float64(rate)))
	}

	tracks := make([]Track, len(tl.Tracks))
	names := make(map[string]int, len(tl.Tracks))
	for i, tr := range tl.Tracks {
		if strings.TrimSpace(tr.Name) == "" {
			return Graph{}, fmt.Errorf("%w: track %d has no name", ErrInvalid, i)
		}
		if _, dup := names[tr.Name]; dup {
			return Graph{}, fmt.Errorf("%w: track name %q is used twice", ErrInvalid, tr.Name)
		}
		names[tr.Name] = i
		if len(tr.Clips) == 0 {
			return Graph{}, fmt.Errorf("%w: track %q has no clips", ErrInvalid, tr.Name)
		}
		for j, c := range tr.Clips {
			if err := validateClip(c); err != nil {
				return Graph{}, fmt.Errorf("track %q clip %d: %w", tr.Name, j, err)
			}
			// A range shorter than half a sample would ask the pad for a
			// whole length of zero, which apad reads as pad forever.
			if samples(c.Length) < 1 {
				return Graph{}, fmt.Errorf("track %q clip %d: %w: length %s is under one sample at %d Hz", tr.Name, j, ErrInvalid, c.Length, rate)
			}
		}
		clips := slices.Clone(tr.Clips)
		slices.SortStableFunc(clips, compareClips)
		tracks[i] = Track{Name: tr.Name, Clips: clips}
	}
	slices.SortFunc(tracks, func(a, b Track) int { return cmp.Compare(a.Name, b.Name) })

	keyed := map[string]bool{}
	if tl.Bed != nil {
		if strings.TrimSpace(tl.Bed.Source) == "" {
			return Graph{}, fmt.Errorf("%w: the bed has no source", ErrInvalid)
		}
		if !validGain(tl.Bed.Gain) {
			return Graph{}, fmt.Errorf("%w: bed gain %v dB is not a number", ErrInvalid, tl.Bed.Gain)
		}
		for _, name := range tl.Bed.DuckOn {
			if _, ok := names[name]; !ok {
				return Graph{}, fmt.Errorf("%w: the bed ducks on track %q, which the timeline lacks", ErrInvalid, name)
			}
			keyed[name] = true
		}
	}

	total := samples(tl.Length)
	if tl.Length == 0 {
		for _, tr := range tracks {
			for _, c := range tr.Clips {
				total = max(total, samples(c.Offset)+samples(c.Length))
			}
		}
	}
	if total < 1 {
		return Graph{}, fmt.Errorf("%w: the timeline renders no sample at %d Hz", ErrInvalid, rate)
	}
	conform := "aresample=" + strconv.Itoa(rate) + ",aformat=sample_fmts=fltp:channel_layouts=" + layout

	var (
		g      Graph
		chains []string
		mixed  []string // the labels summed into the output
		keys   []string // the labels summed into the bed's key
	)
	for ti, tr := range tracks {
		var clipLabels []string
		for ci, c := range tr.Clips {
			in := len(g.Inputs)
			g.Inputs = append(g.Inputs, Input{Source: c.Source})
			label := fmt.Sprintf("trk%dclip%d", ti, ci)
			chains = append(chains, fmt.Sprintf("[%d:a]%s[%s]", in, clipChain(c, conform, samples), label))
			clipLabels = append(clipLabels, label)
		}
		track := fmt.Sprintf("trk%d", ti)
		chains = append(chains, sum(clipLabels, track))
		if keyed[tr.Name] {
			chains = append(chains, fmt.Sprintf("[%s]asplit=2[%smix][%skey]", track, track, track))
			mixed = append(mixed, track+"mix")
			keys = append(keys, track+"key")
		} else {
			mixed = append(mixed, track)
		}
	}
	end := strconv.FormatInt(total, 10)
	fill := "apad=whole_len=" + end + ",atrim=end_sample=" + end
	if tl.Bed != nil {
		in := len(g.Inputs)
		g.Inputs = append(g.Inputs, Input{Source: tl.Bed.Source, Loop: true})
		bed := fmt.Sprintf("[%d:a]%s,atrim=end_sample=%s,asetpts=PTS-STARTPTS,apad=whole_len=%s%s", in, conform, end, end, gainStage(tl.Bed.Gain))
		if len(keys) == 0 {
			chains = append(chains, bed+"[bed]")
		} else {
			// sidechaincompress works on whatever both inputs have queued,
			// and its output depends on where those chunks split, which
			// the decoders' threads decide. Equal fixed frames on both
			// inputs pin the splits, so the duck renders the same bytes on
			// every run.
			d := cfg.duck()
			chains = append(chains, bed+","+duckFrames+"[bedraw]", sum(keys, "keysum"),
				"[keysum]"+fill+","+duckFrames+"[key]",
				"[bedraw][key]sidechaincompress=threshold="+number(math.Pow(10, d.Threshold/20))+
					":ratio="+number(d.Ratio)+
					":attack="+number(float64(d.Attack)/float64(time.Millisecond))+
					":release="+number(float64(d.Release)/float64(time.Millisecond))+"[bed]")
		}
		mixed = append(mixed, "bed")
	}
	chains = append(chains, sum(mixed, "summed"), "[summed]"+fill+"["+OutputLabel+"]")
	g.Filter = strings.Join(chains, ";")
	g.Samples = total
	g.Length = time.Duration(math.Round(float64(total) * float64(time.Second) / float64(rate)))
	return g, nil
}

// validateClip checks one clip on its own.
func validateClip(c Clip) error {
	switch {
	case strings.TrimSpace(c.Source) == "":
		return fmt.Errorf("%w: no source", ErrInvalid)
	case c.In < 0 || c.Offset < 0:
		return fmt.Errorf("%w: in point %s and offset %s must not be negative", ErrInvalid, c.In, c.Offset)
	case c.Length <= 0:
		return fmt.Errorf("%w: length %s must be positive", ErrInvalid, c.Length)
	case c.FadeIn < 0 || c.FadeOut < 0:
		return fmt.Errorf("%w: fades %s and %s must not be negative", ErrInvalid, c.FadeIn, c.FadeOut)
	case c.FadeIn+c.FadeOut > c.Length:
		return fmt.Errorf("%w: fades %s and %s do not fit the length %s", ErrInvalid, c.FadeIn, c.FadeOut, c.Length)
	case !validGain(c.Gain):
		return fmt.Errorf("%w: gain %v dB is not a number", ErrInvalid, c.Gain)
	}
	return nil
}

// clipChain renders the filters one clip runs through, every count in
// samples at the output rate.
func clipChain(c Clip, conform string, samples func(time.Duration) int64) string {
	in, length := samples(c.In), samples(c.Length)
	n := strconv.FormatInt(length, 10)
	stages := []string{
		conform,
		"atrim=start_sample=" + strconv.FormatInt(in, 10) + ":end_sample=" + strconv.FormatInt(in+length, 10),
		"asetpts=PTS-STARTPTS",
		"apad=whole_len=" + n,
		"atrim=end_sample=" + n,
	}
	if g := gainStage(c.Gain); g != "" {
		stages = append(stages, g[1:])
	}
	if fi := samples(c.FadeIn); fi > 0 {
		stages = append(stages, "afade=t=in:ss=0:ns="+strconv.FormatInt(fi, 10))
	}
	if fo := samples(c.FadeOut); fo > 0 {
		stages = append(stages, "afade=t=out:ss="+strconv.FormatInt(length-fo, 10)+":ns="+strconv.FormatInt(fo, 10))
	}
	if off := samples(c.Offset); off > 0 {
		stages = append(stages, "adelay=delays="+strconv.FormatInt(off, 10)+"S:all=1")
	}
	return strings.Join(stages, ",")
}

// gainStage returns ",volume=<db>dB" for a gain other than zero, or the
// empty string.
func gainStage(db float64) string {
	if db == 0 {
		return ""
	}
	return ",volume=" + number(db) + "dB"
}

// sum renders the stage that adds labels into out. One label passes
// through untouched, and more sum without normalisation, so adding a
// stream never lowers the others.
func sum(labels []string, out string) string {
	if len(labels) == 1 {
		return "[" + labels[0] + "]anull[" + out + "]"
	}
	return "[" + strings.Join(labels, "][") + "]amix=inputs=" + strconv.Itoa(len(labels)) +
		":duration=longest:dropout_transition=0:normalize=0[" + out + "]"
}

// number renders a float in the shortest form that reads back exactly.
func number(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
