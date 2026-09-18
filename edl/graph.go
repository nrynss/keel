package edl

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
)

// merge returns the kept ranges sorted by start, with ranges that touch or
// overlap joined into one. The input slice is copied and never modified, so
// no caller sees its list change under it.
func merge(segments []Segment) []Segment {
	sorted := slices.Clone(segments)
	slices.SortFunc(sorted, func(a, b Segment) int {
		return cmp.Compare(a.Start, b.Start)
	})
	merged := make([]Segment, 0, len(sorted))
	for _, s := range sorted {
		n := len(merged) - 1
		if n >= 0 && s.Start <= merged[n].End {
			if s.End > merged[n].End {
				merged[n].End = s.End
			}
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// joinFades returns one fade length per join between neighbouring ranges. A
// join carries the configured crossfade only when both neighbours are at
// least that long, and zero, which means a cut, otherwise.
func joinFades(crossfade time.Duration, segments []Segment) []time.Duration {
	fades := make([]time.Duration, max(len(segments)-1, 0))
	for i := range fades {
		left := segments[i].End - segments[i].Start
		right := segments[i+1].End - segments[i+1].Start
		if crossfade > 0 && left >= crossfade && right >= crossfade {
			fades[i] = crossfade
		}
	}
	return fades
}

// cutGraph builds the filter graph that trims every merged range out of the
// first input and joins the pieces in order. A join with a positive fade is
// an acrossfade and a join without one is a concat cut. The last piece ends
// at the label [cut].
func cutGraph(segments []Segment, fades []time.Duration) string {
	var b strings.Builder
	if len(segments) == 1 {
		fmt.Fprintf(&b, "[0:a]atrim=start=%s:end=%s,asetpts=PTS-STARTPTS[cut]",
			seconds(segments[0].Start), seconds(segments[0].End))
		return b.String()
	}
	fmt.Fprintf(&b, "[0:a]asplit=%d", len(segments))
	for i := range segments {
		fmt.Fprintf(&b, "[trim%d]", i)
	}
	for i, s := range segments {
		fmt.Fprintf(&b, ";[trim%d]atrim=start=%s:end=%s,asetpts=PTS-STARTPTS[seg%d]",
			i, seconds(s.Start), seconds(s.End), i)
	}
	acc := "seg0"
	for i, fade := range fades {
		out := fmt.Sprintf("join%d", i)
		if i == len(fades)-1 {
			out = "cut"
		}
		if fade > 0 {
			fmt.Fprintf(&b, ";[%s][seg%d]acrossfade=d=%s:curve1=tri:curve2=tri[%s]",
				acc, i+1, seconds(fade), out)
		} else {
			fmt.Fprintf(&b, ";[%s][seg%d]concat=n=2:v=0:a=1[%s]", acc, i+1, out)
		}
		acc = out
	}
	return b.String()
}

// seconds renders a duration as decimal seconds at microsecond precision,
// which is the precision ffmpeg parses durations at. The duration is never
// negative, because validation rejects such ranges before a graph is built.
func seconds(d time.Duration) string {
	return fmt.Sprintf("%d.%06d", d/time.Second, (d%time.Second)/time.Microsecond)
}
