package edl

import (
	"testing"
	"time"
)

// ms and second shorten the test tables.
const (
	ms     = time.Millisecond
	second = time.Second
)

// TestMergeSegmentsSortsAndAbsorbs pins the pure merge: unsorted input is
// sorted, ranges that touch or overlap join into one, duplicate points
// collapse, and the caller's slice comes back untouched. No ffmpeg takes
// part.
func TestMergeSegmentsSortsAndAbsorbs(t *testing.T) {
	tests := []struct {
		name string
		in   []Segment
		want []Segment
	}{
		{"empty", nil, nil},
		{"single", []Segment{{10 * ms, 20 * ms}}, []Segment{{10 * ms, 20 * ms}}},
		{"disjoint", []Segment{{0, ms}, {5 * ms, 6 * ms}}, []Segment{{0, ms}, {5 * ms, 6 * ms}}},
		{"unsorted", []Segment{{5 * ms, 6 * ms}, {0, ms}}, []Segment{{0, ms}, {5 * ms, 6 * ms}}},
		{"touching", []Segment{{0, ms}, {ms, 2 * ms}}, []Segment{{0, 2 * ms}}},
		{"overlapping", []Segment{{0, 5 * ms}, {3 * ms, 8 * ms}}, []Segment{{0, 8 * ms}}},
		{"nested", []Segment{{0, 8 * ms}, {3 * ms, 5 * ms}}, []Segment{{0, 8 * ms}}},
		{"duplicate points", []Segment{{2 * ms, 5 * ms}, {2 * ms, 5 * ms}, {2 * ms, 5 * ms}}, []Segment{{2 * ms, 5 * ms}}},
		{"chain of touches", []Segment{{0, ms}, {ms, 2 * ms}, {2 * ms, 4 * ms}}, []Segment{{0, 4 * ms}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := merge(tt.in)
			if !slicesEqual(got, tt.want) {
				t.Fatalf("merge(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}

	unsorted := []Segment{{5 * ms, 6 * ms}, {0, ms}}
	merge(unsorted)
	if unsorted[0] != (Segment{5 * ms, 6 * ms}) {
		t.Fatalf("merge modified the caller's slice: %v", unsorted)
	}
}

// slicesEqual compares two segment lists, treating nil and empty as equal.
func slicesEqual(got, want []Segment) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestJoinFadesBoundedByShorterNeighbour pins the join rule: a join carries
// the crossfade only when both neighbours are at least that long, and a
// shorter neighbour makes the join a cut.
func TestJoinFadesBoundedByShorterNeighbour(t *testing.T) {
	const fade = 250 * ms
	tests := []struct {
		name      string
		crossfade time.Duration
		segments  []Segment
		want      []time.Duration
	}{
		{"both long enough", fade, []Segment{{0, second}, {2 * second, 3 * second}}, []time.Duration{fade}},
		{"left shorter", fade, []Segment{{0, 100 * ms}, {2 * second, 3 * second}}, []time.Duration{0}},
		{"right shorter", fade, []Segment{{0, second}, {2 * second, second + 100*ms}}, []time.Duration{0}},
		{"exactly as long", fade, []Segment{{0, fade}, {second, second + fade}}, []time.Duration{fade}},
		{"no crossfade", 0, []Segment{{0, second}, {2 * second, 3 * second}, {4 * second, 5 * second}}, []time.Duration{0, 0}},
		{"one range", fade, []Segment{{0, second}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := joinFades(tt.crossfade, tt.segments)
			if len(got) != len(tt.want) {
				t.Fatalf("joinFades(%v, %v) = %v, want %v", tt.crossfade, tt.segments, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("joinFades(%v, %v) = %v, want %v", tt.crossfade, tt.segments, got, tt.want)
				}
			}
		})
	}
}

// TestCutGraphShapes pins the built filter graphs: a lone range trims
// straight, a faded join chains through acrossfade, and a join without a
// fade is a concat cut, so the graph never receives a zero length range.
func TestCutGraphShapes(t *testing.T) {
	t.Run("one range", func(t *testing.T) {
		got := cutGraph([]Segment{{500 * ms, 2 * second}}, nil)
		want := "[0:a]atrim=start=0.500000:end=2.000000,asetpts=PTS-STARTPTS[cut]"
		if got != want {
			t.Fatalf("cutGraph = %s, want %s", got, want)
		}
	})
	t.Run("faded join", func(t *testing.T) {
		segs := []Segment{{0, 1500 * ms}, {2 * second, 3 * second}}
		got := cutGraph(segs, []time.Duration{250 * ms})
		want := "[0:a]asplit=2[trim0][trim1]" +
			";[trim0]atrim=start=0.000000:end=1.500000,asetpts=PTS-STARTPTS[seg0]" +
			";[trim1]atrim=start=2.000000:end=3.000000,asetpts=PTS-STARTPTS[seg1]" +
			";[seg0][seg1]acrossfade=d=0.250000:curve1=tri:curve2=tri[cut]"
		if got != want {
			t.Fatalf("cutGraph = %s, want %s", got, want)
		}
	})
	t.Run("faded then cut join", func(t *testing.T) {
		segs := []Segment{{0, 1500 * ms}, {2 * second, 3 * second}, {3200 * ms, 3500 * ms}}
		got := cutGraph(segs, []time.Duration{250 * ms, 0})
		want := "[0:a]asplit=3[trim0][trim1][trim2]" +
			";[trim0]atrim=start=0.000000:end=1.500000,asetpts=PTS-STARTPTS[seg0]" +
			";[trim1]atrim=start=2.000000:end=3.000000,asetpts=PTS-STARTPTS[seg1]" +
			";[trim2]atrim=start=3.200000:end=3.500000,asetpts=PTS-STARTPTS[seg2]" +
			";[seg0][seg1]acrossfade=d=0.250000:curve1=tri:curve2=tri[join0]" +
			";[join0][seg2]concat=n=2:v=0:a=1[cut]"
		if got != want {
			t.Fatalf("cutGraph = %s, want %s", got, want)
		}
	})
}

// TestSecondsFormatsMicroseconds pins the duration formatting that filter
// arguments carry. Integer microseconds cannot drift the way float seconds
// can.
func TestSecondsFormatsMicroseconds(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0.000000"},
		{25 * ms, "0.025000"},
		{1500 * ms, "1.500000"},
		{time.Minute + 1001*time.Microsecond, "60.001001"},
	}
	for _, tt := range tests {
		if got := seconds(tt.in); got != tt.want {
			t.Fatalf("seconds(%v) = %s, want %s", tt.in, got, tt.want)
		}
	}
}
