package fit

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAtempoChainKeepsEachStageInRange pins the chain for ratios inside
// one stage, at the stage limits, and beyond them in both directions.
func TestAtempoChainKeepsEachStageInRange(t *testing.T) {
	tests := []struct {
		ratio float64
		want  string
	}{
		{ratio: 1, want: ""},
		{ratio: 1.25, want: "atempo=1.25"},
		{ratio: 0.8, want: "atempo=0.8"},
		{ratio: 2, want: "atempo=2"},
		{ratio: 0.5, want: "atempo=0.5"},
		{ratio: 3, want: "atempo=2,atempo=1.5"},
		{ratio: 4, want: "atempo=2,atempo=2"},
		{ratio: 5, want: "atempo=2,atempo=2,atempo=1.25"},
		{ratio: 0.3, want: "atempo=0.5,atempo=0.6"},
		{ratio: 0.25, want: "atempo=0.5,atempo=0.5"},
		{ratio: 0.1, want: "atempo=0.5,atempo=0.5,atempo=0.5,atempo=0.8"},
		{ratio: 1.0833333333333333, want: "atempo=1.0833333333333333"},
	}
	for _, tt := range tests {
		t.Run(strconv.FormatFloat(tt.ratio, 'g', -1, 64), func(t *testing.T) {
			got, err := AtempoChain(tt.ratio)
			if err != nil {
				t.Fatalf("AtempoChain(%v): %v", tt.ratio, err)
			}
			if got != tt.want {
				t.Errorf("AtempoChain(%v) = %q, want %q", tt.ratio, got, tt.want)
			}
			if got == "" {
				return
			}
			product := 1.0
			for _, stage := range strings.Split(got, ",") {
				f, err := strconv.ParseFloat(strings.TrimPrefix(stage, "atempo="), 64)
				if err != nil {
					t.Fatalf("stage %q: %v", stage, err)
				}
				if f < atempoMin || f > atempoMax {
					t.Errorf("stage %q leaves 0.5 to 2", stage)
				}
				product *= f
			}
			if math.Abs(product-tt.ratio) > 1e-12 {
				t.Errorf("stages multiply to %v, want %v", product, tt.ratio)
			}
		})
	}
	for _, bad := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := AtempoChain(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("AtempoChain(%v) error = %v, want ErrInvalid", bad, err)
		}
	}
}

// TestBoundsResolve pins the defaults and the refusals.
func TestBoundsResolve(t *testing.T) {
	got, err := Bounds{}.resolve()
	if err != nil || got != (Bounds{Slowest: DefaultSlowest, Fastest: DefaultFastest}) {
		t.Fatalf("zero Bounds resolve = %+v, %v", got, err)
	}
	got, err = Bounds{Slowest: 1}.resolve()
	if err != nil || got != (Bounds{Slowest: 1, Fastest: DefaultFastest}) {
		t.Fatalf("Bounds{Slowest: 1} resolve = %+v, %v", got, err)
	}
	for _, b := range []Bounds{
		{Slowest: -0.5}, {Slowest: 1.1}, {Fastest: 0.9}, {Fastest: -2},
		{Slowest: math.NaN()}, {Fastest: math.NaN()}, {Fastest: math.Inf(1)},
	} {
		if _, err := b.resolve(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v resolve error = %v, want ErrInvalid", b, err)
		}
	}
}

// TestPlanFitChoosesTheRatio pins the decision for a take inside the
// bounds both ways, one that fills the slot already, one that needs more
// slowing than allowed and is padded, and one that is refused.
func TestPlanFitChoosesTheRatio(t *testing.T) {
	def := Bounds{Slowest: DefaultSlowest, Fastest: DefaultFastest}
	tests := []struct {
		name         string
		length, slot time.Duration
		rate         int
		bounds       Bounds
		ratio        float64
		padded       time.Duration
		filter       string
	}{
		{
			name: "speed up", length: 2200 * time.Millisecond, slot: 2 * time.Second, rate: 48000, bounds: def,
			ratio: 1.1, filter: "atempo=1.1,apad=whole_len=96000,atrim=end_sample=96000",
		},
		{
			name: "slow down", length: 1800 * time.Millisecond, slot: 2 * time.Second, rate: 48000, bounds: def,
			ratio: 0.9, filter: "atempo=0.9,apad=whole_len=96000,atrim=end_sample=96000",
		},
		{
			name: "already fits", length: 2 * time.Second, slot: 2 * time.Second, rate: 44100, bounds: def,
			ratio: 1, filter: "apad=whole_len=88200,atrim=end_sample=88200",
		},
		{
			name: "within a sample keeps tempo", length: 2*time.Second + 10*time.Microsecond, slot: 2 * time.Second, rate: 44100, bounds: def,
			ratio: 1, filter: "apad=whole_len=88200,atrim=end_sample=88200",
		},
		{
			name: "short take slowed then padded", length: 2 * time.Second, slot: 3 * time.Second, rate: 48000, bounds: def,
			ratio: 0.8, padded: 500 * time.Millisecond, filter: "atempo=0.8,apad=whole_len=144000,atrim=end_sample=144000",
		},
		{
			name: "never slowed only padded", length: 2 * time.Second, slot: 3 * time.Second, rate: 48000, bounds: Bounds{Slowest: 1, Fastest: 1.25},
			ratio: 1, padded: time.Second, filter: "apad=whole_len=144000,atrim=end_sample=144000",
		},
		{
			name: "slot between samples rounds", length: time.Second, slot: time.Second + 10*time.Microsecond, rate: 48000, bounds: def,
			ratio: 1, filter: "apad=whole_len=48000,atrim=end_sample=48000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := planFit(tt.length, tt.slot, tt.rate, tt.bounds)
			if err != nil {
				t.Fatalf("planFit: %v", err)
			}
			if math.Abs(p.ratio-tt.ratio) > 1e-12 || p.padded != tt.padded {
				t.Errorf("ratio %v padded %v, want %v and %v", p.ratio, p.padded, tt.ratio, tt.padded)
			}
			want := tt.filter
			if tt.ratio != 1 {
				chain, _ := AtempoChain(p.ratio)
				want = chain + tt.filter[strings.Index(tt.filter, ",apad"):]
			}
			if p.filter != want {
				t.Errorf("filter = %q, want %q", p.filter, want)
			}
		})
	}
	t.Run("too long is refused", func(t *testing.T) {
		_, err := planFit(3*time.Second, 2*time.Second, 48000, def)
		var re *RatioError
		if !errors.As(err, &re) || !errors.Is(err, ErrOutOfBounds) {
			t.Fatalf("planFit error = %v, want a *RatioError wrapping ErrOutOfBounds", err)
		}
		if re.Ratio != 1.5 || re.Bounds != def {
			t.Errorf("RatioError = %+v, want ratio 1.5 under the default bounds", re)
		}
		if !strings.Contains(re.Error(), "1.5000") {
			t.Errorf("Error() = %q, want the needed ratio", re.Error())
		}
	})
}
