package throttle

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fixedClock is a clock the test moves by hand.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fixedClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// interval2 is the spacing of a two-per-minute pacer: 30s plus its
// twentieth as a margin for a wake-up that runs late.
const interval2 = 31500 * time.Millisecond

func TestPacerSpacesSlotsOneIntervalApart(t *testing.T) {
	clk := &fixedClock{t: time.Unix(1_000_000, 0)}
	rec := &sleepRecorder{}
	p := NewPacer(Config{PerMinute: 2, Sleep: rec.sleep, Now: clk.now})
	for i := 0; i < 4; i++ {
		if err := p.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// From a quiet start the first goes at once and the rest grow by the
	// interval.
	want := []time.Duration{interval2, 2 * interval2, 3 * interval2}
	got := rec.waits()
	if len(got) != len(want) {
		t.Fatalf("waits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("wait %d = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestPacerQuietSpellResetsTheQueue(t *testing.T) {
	clk := &fixedClock{t: time.Unix(1_000_000, 0)}
	rec := &sleepRecorder{}
	p := NewPacer(Config{PerMinute: 2, Sleep: rec.sleep, Now: clk.now})
	if err := p.Wait(context.Background()); err != nil { // slot now
		t.Fatal(err)
	}
	clk.advance(10 * time.Minute)
	if err := p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := rec.waits(); len(got) != 0 {
		t.Fatalf("waits = %v, want none, since the queue drained while nobody asked", got)
	}
}

// TestPacerRateComesFromConfig pins that the per-minute figure is the
// caller's, and that the margin scales with the interval.
func TestPacerRateComesFromConfig(t *testing.T) {
	for _, tc := range []struct {
		rpm  int
		want time.Duration // the second slot's wait
	}{
		{2, 31500 * time.Millisecond},
		{6, 10500 * time.Millisecond},
		{60, 1050 * time.Millisecond},
	} {
		t.Run(fmt.Sprint(tc.rpm), func(t *testing.T) {
			clk := &fixedClock{t: time.Unix(1_000_000, 0)}
			rec := &sleepRecorder{}
			p := NewPacer(Config{PerMinute: tc.rpm, Sleep: rec.sleep, Now: clk.now})
			for i := 0; i < 2; i++ {
				if err := p.Wait(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if got := rec.waits(); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("per-minute %d: waits = %v, want [%s]", tc.rpm, got, tc.want)
			}
		})
	}
}

func TestPacerUnpacedRateNeverWaits(t *testing.T) {
	for _, rpm := range []int{0, -1} {
		rec := &sleepRecorder{}
		p := NewPacer(Config{PerMinute: rpm, Sleep: rec.sleep, Now: (&fixedClock{t: time.Unix(1_000_000, 0)}).now})
		for i := 0; i < 5; i++ {
			if err := p.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if got := rec.waits(); len(got) != 0 {
			t.Fatalf("per-minute %d: waits = %v, want none", rpm, got)
		}
	}
}

func TestPacerCancelledWaitReturnsTheContextError(t *testing.T) {
	clk := &fixedClock{t: time.Unix(1_000_000, 0)}
	p := NewPacer(Config{Sleep: sleep, Now: clk.now, PerMinute: 2})
	if err := p.Wait(context.Background()); err != nil { // the first slot is free, and not what is under test
		t.Fatalf("first wait: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// gateSleep records each wait and, while block is set, parks until ctx ends,
// so a test can hold waiters queued and then cancel them.
type gateSleep struct {
	rec   sleepRecorder
	block atomic.Bool
}

func (g *gateSleep) sleep(ctx context.Context, d time.Duration) error {
	g.rec.sleep(ctx, d)
	if g.block.Load() {
		<-ctx.Done()
	}
	return ctx.Err()
}

// slotCount is how many slots the pacer is holding.
func (p *Pacer) slotCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.slots)
}

// queueCancelled parks n waiters on p, each with its own context, until every
// one holds a slot, and returns their cancel funcs and a wait group over
// them. They are queued one at a time, so slots are handed out in a known
// order.
func queueCancelled(t *testing.T, p *Pacer, g *gateSleep, n int) (cancels []context.CancelFunc, wg *sync.WaitGroup) {
	t.Helper()
	g.block.Store(true)
	wg = &sync.WaitGroup{}
	base := p.slotCount()
	for i := 0; i < n; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Wait(ctx); !errors.Is(err, context.Canceled) {
				t.Errorf("queued waiter %d: err = %v, want context.Canceled", i, err)
			}
		}()
		deadline := time.Now().Add(5 * time.Second)
		for p.slotCount() < base+i+1 {
			if time.Now().After(deadline) {
				t.Fatalf("waiter %d never took a slot", i)
			}
			time.Sleep(time.Millisecond)
		}
	}
	return cancels, wg
}

// TestPacerCancelledWaitersDoNotStrandSlots pins the slot-return rule: one
// used slot, four queued waiters, all cancelled, and then the next caller.
// The next caller must wait one interval behind the slot that was really
// used, not five behind the four that never were.
func TestPacerCancelledWaitersDoNotStrandSlots(t *testing.T) {
	clk := &fixedClock{t: time.Unix(1_000_000, 0)}
	g := &gateSleep{}
	p := NewPacer(Config{PerMinute: 2, Sleep: g.sleep, Now: clk.now})
	if err := p.Wait(context.Background()); err != nil { // the used slot
		t.Fatal(err)
	}
	cancels, wg := queueCancelled(t, p, g, 4)
	for _, cancel := range cancels {
		cancel()
	}
	wg.Wait()
	g.block.Store(false)
	before := len(g.rec.waits())
	if err := p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := g.rec.waits()
	if len(w) != before+1 || w[before] != interval2 {
		t.Fatalf("next wait after four cancelled = %v, want exactly one interval %s", w[before:], interval2)
	}
}

// TestPacerCancelledMiddleWaiterLeavesAReusableGap pins that cancelling a
// waiter in the middle of the queue frees its slot for the next caller, not
// only the tail's.
func TestPacerCancelledMiddleWaiterLeavesAReusableGap(t *testing.T) {
	clk := &fixedClock{t: time.Unix(1_000_000, 0)}
	g := &gateSleep{}
	p := NewPacer(Config{PerMinute: 2, Sleep: g.sleep, Now: clk.now})
	if err := p.Wait(context.Background()); err != nil { // slot 0, used
		t.Fatal(err)
	}
	cancels, wg := queueCancelled(t, p, g, 3) // slots 1, 2, 3
	cancels[1]()                              // slot 2 goes back
	deadline := time.Now().Add(5 * time.Second)
	for p.slotCount() != 3 {
		if time.Now().After(deadline) {
			t.Fatal("the cancelled middle waiter never released its slot")
		}
		time.Sleep(time.Millisecond)
	}
	g.block.Store(false)
	before := len(g.rec.waits())
	if err := p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w := g.rec.waits(); w[before] != 2*interval2 {
		t.Fatalf("wait = %s, want the freed slot 2 (%s), not the end of the queue (%s)", w[before], 2*interval2, 4*interval2)
	}
	for _, i := range []int{0, 2} {
		cancels[i]()
	}
	wg.Wait()
}

// TestPacerUsedSlotsStillSpaceTheNextCaller is the other direction of the
// slot-return rule: a slot whose owner got to use it is spent, and the next
// caller waits behind it.
func TestPacerUsedSlotsStillSpaceTheNextCaller(t *testing.T) {
	clk := &fixedClock{t: time.Unix(1_000_000, 0)}
	rec := &sleepRecorder{}
	p := NewPacer(Config{PerMinute: 2, Sleep: rec.sleep, Now: clk.now})
	for i := 0; i < 2; i++ {
		if err := p.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if w := rec.waits(); len(w) != 1 || w[0] != interval2 {
		t.Fatalf("second caller waited %v, want [%s] behind the used slot", w, interval2)
	}

	// A waiter whose context ends only after its slot arrived has spent it.
	late := &fixedClock{t: time.Unix(2_000_000, 0)}
	arrives := func(ctx context.Context, d time.Duration) error {
		late.advance(d)
		return context.Canceled
	}
	q := NewPacer(Config{PerMinute: 2, Sleep: arrives, Now: late.now})
	if err := q.Wait(context.Background()); err != nil { // free, now
		t.Fatal(err)
	}
	if err := q.Wait(context.Background()); !errors.Is(err, context.Canceled) { // slot +interval arrives, then ends
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	rec2 := &sleepRecorder{}
	q.sleep = rec2.sleep
	if err := q.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w := rec2.waits(); len(w) != 1 || w[0] != interval2 {
		t.Fatalf("waited %v after a slot that had arrived, want [%s], since an arrived slot is spent", w, interval2)
	}
}

// TestPacerNoTwoSlotsCloserThanTheInterval is the safety half: whatever mix
// of reservations and releases happens, handed-out slots stay a full interval
// apart, so real submissions can never exceed the cap.
func TestPacerNoTwoSlotsCloserThanTheInterval(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	clk := &fixedClock{t: time.Unix(1_000_000, 0)}
	p := NewPacer(Config{PerMinute: 2, Now: clk.now})
	var held []time.Time
	for i := 0; i < 2000; i++ {
		switch rng.IntN(4) {
		case 0:
			clk.advance(time.Duration(rng.Int64N(int64(2 * p.interval))))
		case 1:
			if len(held) > 0 {
				k := rng.IntN(len(held))
				p.release(held[k])
				held = append(held[:k], held[k+1:]...)
			}
		default:
			slot, _ := p.reserve()
			held = append(held, slot)
		}
		p.mu.Lock()
		for j := 1; j < len(p.slots); j++ {
			if gap := p.slots[j].Sub(p.slots[j-1]); gap < p.interval {
				p.mu.Unlock()
				t.Fatalf("step %d: slots %v and %v are %s apart, want at least %s", i, p.slots[j-1], p.slots[j], gap, p.interval)
			}
		}
		p.mu.Unlock()
	}
}

// TestPacerReadsItsClockUnderItsLock pins the locking discipline: the clock
// is read in the same critical section that reserves, so callers take slots
// in the order they sampled time. A pacer that sampled it before taking the
// lock would let two callers reserve out of order and land two submissions
// closer than the interval. The injected clock asserts the mutex is held
// whenever it is read.
func TestPacerReadsItsClockUnderItsLock(t *testing.T) {
	clk := &fixedClock{t: time.Unix(1_000_000, 0)}
	var p *Pacer
	var reads, unlocked int
	now := func() time.Time {
		reads++
		if p.mu.TryLock() { // it succeeded, so the caller did not hold the lock
			p.mu.Unlock()
			unlocked++
		}
		return clk.now()
	}
	p = NewPacer(Config{PerMinute: 2, Sleep: (&sleepRecorder{}).sleep, Now: now})
	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < 3; i++ {
		if err := p.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Wait(ctx); err != nil { // a queued slot, so release has something to do
		t.Fatal(err)
	}
	cancel()
	p.release(time.Unix(2_000_000, 0)) // reads the clock too, and returns early
	if reads == 0 {
		t.Fatal("the pacer never read its clock")
	}
	if unlocked != 0 {
		t.Fatalf("%d of %d clock reads happened without the pacer's lock held", unlocked, reads)
	}
}

// TestPacerAnAlreadyCancelledContextGetsNoSlot pins the pre-check in Wait: a
// context that has already ended would meet a free slot, so without the check
// Wait would spend the slot and let a dead caller submit.
func TestPacerAnAlreadyCancelledContextGetsNoSlot(t *testing.T) {
	clk := &fixedClock{t: time.Unix(1_000_000, 0)}
	rec := &sleepRecorder{}
	p := NewPacer(Config{PerMinute: 2, Sleep: rec.sleep, Now: clk.now})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := p.slotCount(); n != 0 {
		t.Fatalf("slotCount = %d, want 0, since a dead context must not spend a slot", n)
	}
	if got := rec.waits(); len(got) != 0 {
		t.Fatalf("waits = %v, want none", got)
	}
	// The next, live caller goes at once, since nothing was reserved ahead
	// of it.
	if err := p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := rec.waits(); len(got) != 0 {
		t.Fatalf("waits = %v, want none after a refused caller", got)
	}
}

// TestPacerExpiredSlotsArePruned pins that slots a full interval behind the
// clock are dropped, so the list is bounded by live callers rather than
// growing with every call the process ever made.
func TestPacerExpiredSlotsArePruned(t *testing.T) {
	clk := &fixedClock{t: time.Unix(1_000_000, 0)}
	p := NewPacer(Config{PerMinute: 2, Sleep: (&sleepRecorder{}).sleep, Now: clk.now})
	for i := 0; i < 1000; i++ {
		if err := p.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := p.slotCount(); n > 2 {
			t.Fatalf("after %d waits slotCount = %d, want at most 2", i+1, n)
		}
		clk.advance(p.interval)
	}
	clk.advance(10 * time.Minute)
	if err := p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := p.slotCount(); n != 1 {
		t.Fatalf("after a long quiet spell slotCount = %d, want exactly 1", n)
	}
}

// TestPacerSharedWaitHonoursCancellation: a caller queued behind others on a
// shared pacer returns the context error when its own context ends, and does
// not block the callers that follow.
func TestPacerSharedWaitHonoursCancellation(t *testing.T) {
	reg := &pacerRegistry{}
	clk := &fixedClock{t: time.Unix(1_000_000, 0)}
	cfg := Config{PerMinute: 2, Name: "shared-cancel", Sleep: sleep, Now: clk.now, registry: reg}
	p := pacerFor(cfg)
	if err := p.Wait(context.Background()); err != nil { // first slot is free
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Wait(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled queued wait did not return")
	}
}

// TestPacerForSharingRules pins which callers share a pacer. A cap is
// account-wide, so a named window at one rate is one pacer for the life of
// the process. A different name or rate does not share. A config with an
// injected clock stays private, so a test's clock never leaks into
// production's. An unpaced rate bypasses every registry.
func TestPacerForSharingRules(t *testing.T) {
	clk := &fixedClock{t: time.Unix(1_000_000, 0)}
	rec := &sleepRecorder{}
	reg := &pacerRegistry{}
	inj := Config{PerMinute: 2, Name: "m", Sleep: rec.sleep, Now: clk.now, registry: reg}

	a := pacerFor(inj)
	if b := pacerFor(inj); a != b {
		t.Error("same name and rate in one registry got two pacers, so concurrent callers would each assume an empty window")
	}
	other := inj
	other.Name = "other"
	if b := pacerFor(other); a == b {
		t.Error("a different name shared a pacer, but the cap is per name")
	}
	fast := inj
	fast.PerMinute = 6
	if b := pacerFor(fast); a == b {
		t.Error("a different rate shared a pacer")
	}

	private := Config{PerMinute: 2, Name: "m", Sleep: rec.sleep, Now: clk.now}
	if x, y := pacerFor(private), pacerFor(private); x == y {
		t.Error("an injected clock without a registry was shared, so it would leak into every other caller")
	}

	off := Config{PerMinute: -1, Name: "m", Sleep: rec.sleep, Now: clk.now, registry: reg}
	if x, y := pacerFor(off), pacerFor(off); x == y || x.interval != 0 {
		t.Error("an unpaced config went through the registry or kept an interval")
	}
	reg.mu.Lock()
	if len(reg.m) != 3 { // m@2, other@2, m@6
		t.Errorf("registry holds %d pacers, want 3, since the unpaced config must not register", len(reg.m))
	}
	reg.mu.Unlock()

	// The production path: a named window with no injected clock resolves to
	// the process-wide registry. The entries it creates are removed again.
	live := Config{PerMinute: 2, Name: "process-wide-under-test"}
	z := pacerFor(live)
	if again := pacerFor(live); z != again {
		t.Error("two production-shape callers of one window got different pacers")
	}
	if want := processPacers.get("process-wide-under-test", 2, live); z != want {
		t.Error("the production shape did not resolve to the process-wide registry's pacer")
	}
	processPacers.mu.Lock()
	delete(processPacers.m, pacerKey{name: "process-wide-under-test", perMinute: 2})
	processPacers.mu.Unlock()
}

// TestPacerRegistryConcurrentGetIsOnePacer hammers get from many goroutines
// under the race detector: every caller must see the same pacer.
func TestPacerRegistryConcurrentGetIsOnePacer(t *testing.T) {
	reg := &pacerRegistry{}
	cfg := Config{PerMinute: 2, Name: "m"}
	var wg sync.WaitGroup
	got := make([]*Pacer, 32)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = reg.get("m", 2, cfg)
		}()
	}
	wg.Wait()
	for i, p := range got {
		if p != got[0] {
			t.Fatalf("goroutine %d got a different pacer", i)
		}
	}
}

// TestRetryPacesStartsThroughThePacer pins the wiring end to end: two calls
// of Retry over one named window take slots one interval apart, and the
// second wait is the pacer's, not the backoff's.
func TestRetryPacesStartsThroughThePacer(t *testing.T) {
	clk := &fixedClock{t: time.Unix(1_000_000, 0)}
	rec := &sleepRecorder{}
	reg := &pacerRegistry{}
	cfg := Config{
		PerMinute: 2,
		Name:      "paced-e2e",
		Backoff:   time.Hour, // would dominate if the pacer were not used
		Now:       clk.now,
		Sleep:     rec.sleep,
		registry:  reg,
	}
	for i := 0; i < 2; i++ {
		if err := Retry(context.Background(), cfg, retryable, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	w := rec.waits()
	if len(w) != 1 || w[0] != interval2 {
		t.Fatalf("waits = %v, want exactly one pacer wait of %s and no backoff wait", w, interval2)
	}
}

// TestRetryUnpacedStartsAtOnce pins the additive path: a config with no rate
// never waits in a pacer, as before.
func TestRetryUnpacedStartsAtOnce(t *testing.T) {
	rec := &sleepRecorder{}
	cfg := Config{Backoff: time.Microsecond, Sleep: rec.sleep}
	if err := Retry(context.Background(), cfg, retryable, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, w := range rec.waits() {
		if w > time.Millisecond {
			t.Fatalf("an unpaced call waited %s in a pacer", w)
		}
	}
}

// TestSleepWaitsOnARealTimer pins the production sleep against the real
// clock, so replacing its body with an instant return or dropping its context
// arm fails here in seconds instead of in production.
func TestSleepWaitsOnARealTimer(t *testing.T) {
	const d = 40 * time.Millisecond
	start := time.Now()
	if err := sleep(context.Background(), d); err != nil {
		t.Fatalf("sleep: %v", err)
	}
	if el := time.Since(start); el < d {
		t.Errorf("sleep(%s) returned after %s, want it to wait", d, el)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- sleep(ctx, time.Hour) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sleep on a cancelled ctx = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sleep ignored its context for 5s")
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	done2 := make(chan error, 1)
	go func() { done2 <- sleep(ctx2, time.Hour) }()
	select {
	case err := <-done2:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("sleep past a deadline = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sleep ignored its context for 5s")
	}
}

// vclock is a virtual clock for the rate-window simulation. Sleepers park on
// it, and a driver advances it to the earliest wake-up once every goroutine
// has been quiet for a few real milliseconds and no other goroutine is
// running or runnable.
//
// The second condition is what makes the simulation honest under load. The
// pacer reads its clock and reserves under one lock, so slots are handed out
// in clock order. But a woken goroutine that the scheduler has not yet run
// would, with only the quiet heuristic, be left behind while virtual time
// jumped to the next wake-up. Its submission would then reach the window late
// and too close to its neighbour's. A late goroutine can make requests
// denser, not only sparser, so the driver must not let it be late.
type vclock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*vwaiter
	busy    atomic.Int32 // goroutines inside a window decision
}

type vwaiter struct {
	wake time.Time
	ch   chan struct{}
}

func (c *vclock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *vclock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	c.mu.Lock()
	w := &vwaiter{wake: c.now.Add(d), ch: make(chan struct{})}
	c.waiters = append(c.waiters, w)
	c.mu.Unlock()
	select {
	case <-w.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// drive advances the clock until stop closes, which is its exit.
func (c *vclock) drive(stop <-chan struct{}) {
	lastN, quiet := -1, 0
	for {
		select {
		case <-stop:
			return
		case <-time.After(time.Millisecond):
		}
		c.mu.Lock()
		n := len(c.waiters)
		if n == lastN && n > 0 && c.busy.Load() == 0 && !otherGoroutinesRunnable() {
			quiet++
		} else {
			quiet = 0
		}
		lastN = n
		if quiet >= 2 {
			sort.Slice(c.waiters, func(i, j int) bool { return c.waiters[i].wake.Before(c.waiters[j].wake) })
			if c.waiters[0].wake.After(c.now) {
				c.now = c.waiters[0].wake
			}
			rest := c.waiters[:0]
			for _, w := range c.waiters {
				if w.wake.After(c.now) {
					rest = append(rest, w)
				} else {
					close(w.ch)
				}
			}
			c.waiters = rest
			quiet, lastN = 0, -1
		}
		c.mu.Unlock()
	}
}

// otherGoroutinesRunnable reports whether any goroutine besides the caller is
// running or runnable, read from a stop-the-world stack dump. The virtual
// clock may only advance when everyone else is parked.
func otherGoroutinesRunnable() bool {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	blocks := strings.Split(string(buf), "\n\n")
	for _, b := range blocks[1:] { // the first block is the calling goroutine
		head, _, _ := strings.Cut(b, "\n")
		if strings.Contains(head, "[runnable") || strings.Contains(head, "[running") {
			return true
		}
	}
	return false
}

// windowError is a rate-limit refusal that names the moment the oldest
// accepted submission leaves the window, the way a provider's refusal body
// names it. It is a typed error, not a parsed body, because reading a
// provider's shape is the caller's job, and the seam under test is the hint
// func.
type windowError struct {
	retryAfter time.Duration
}

func (e *windowError) Error() string {
	return fmt.Sprintf("rate limited, retry after %s", e.retryAfter)
}

// hintOf reads a windowError out of an error chain, as a caller's
// Config.RetryAfter would.
func hintOf(err error) (time.Duration, bool) {
	var we *windowError
	if errors.As(err, &we) {
		return we.retryAfter, true
	}
	return 0, false
}

// limitErr classifies refusals as worth a wait, the way a caller's
// classifier would.
func limitErr(err error) bool {
	var we *windowError
	return errors.As(err, &we)
}

// window is a fake endpoint enforcing a sliding cap: a submission is refused
// while limit accepted submissions sit in the trailing minute, and a refusal
// is not counted against the window. The render takes 5 to 40 virtual
// seconds, drawn from a seed-fixed source, and an accepted submission holds
// its window place from the moment it was accepted.
type window struct {
	clk     *vclock
	limit   int
	latency func() time.Duration

	mu       sync.Mutex
	accepted []time.Time
	refused  int
	maxIn    int // most accepted submissions ever seen inside one trailing minute
}

func (w *window) submit(ctx context.Context) error {
	w.clk.busy.Add(1)
	now := w.clk.Now()
	w.mu.Lock()
	var in []time.Time
	for _, a := range w.accepted {
		if now.Sub(a) < time.Minute {
			in = append(in, a)
		}
	}
	if len(in) >= w.limit {
		w.refused++
		oldest := in[0]
		for _, a := range in {
			if a.Before(oldest) {
				oldest = a
			}
		}
		retry := time.Minute - now.Sub(oldest)
		w.mu.Unlock()
		w.clk.busy.Add(-1)
		return &windowError{retryAfter: retry}
	}
	w.accepted = append(w.accepted, now)
	if w.maxIn < len(in)+1 {
		w.maxIn = len(in) + 1
	}
	w.mu.Unlock()
	w.clk.busy.Add(-1)
	return w.clk.Sleep(ctx, w.latency()) // the render itself
}

// counts returns the refusals drawn and the accepts recorded so far.
func (w *window) counts() (refused, accepted int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.refused, len(w.accepted)
}

// simulate runs calls concurrent Retry submissions against a sliding window
// of two per minute on a virtual clock. The window starts with prior accepts
// at seed-drawn ages, which is the state a previous run left behind. paced
// decides whether the submissions share one named pacer or start unpaced.
// randN fixes the jitter, so the outcome is a count and not a sample.
func simulate(t *testing.T, seed uint64, prior, calls int, paced bool, randN func(int64) int64, attempts int) (errs []error, refused, maxIn int) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 0x77696e646f77))
	clk := &vclock{now: time.Unix(2_000_000, 0)}
	stop := make(chan struct{})
	defer close(stop) // the driver's exit
	go clk.drive(stop)

	var rmu sync.Mutex // the rng is shared by concurrent renders
	win := &window{clk: clk, limit: 2, latency: func() time.Duration {
		rmu.Lock()
		defer rmu.Unlock()
		return 5*time.Second + time.Duration(rng.Int64N(int64(35*time.Second)))
	}}
	for i := 0; i < prior; i++ {
		rmu.Lock()
		age := time.Duration(rng.Int64N(int64(time.Minute)))
		rmu.Unlock()
		win.accepted = append(win.accepted, clk.now.Add(-age))
	}
	cfg := Config{
		PerMinute:  2,
		Name:       "sim",
		Attempts:   attempts,
		Backoff:    10 * time.Second,
		Max:        45 * time.Second,
		RetryAfter: hintOf,
		Now:        clk.Now,
		Sleep:      clk.Sleep,
		randN:      randN,
		registry:   &pacerRegistry{},
	}
	if !paced {
		cfg.PerMinute = 0 // unpaced, which bypasses every registry
		cfg.Name = ""
		cfg.registry = nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	errs = make([]error, calls)
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = Retry(ctx, cfg, limitErr, func() error {
				return win.submit(ctx)
			})
		}()
	}
	wg.Wait()
	refused, _ = win.counts()
	win.mu.Lock()
	maxIn = win.maxIn
	win.mu.Unlock()
	return errs, refused, maxIn
}

// TestWindowSimulationStaysInsideTheCap pins the pacing promise the whole
// change exists for: fan-outs of concurrent calls against a two-per-minute
// sliding window, some of it already spent by an earlier run, all finish, and
// from a quiet window not one refusal is drawn. The window is never exceeded.
func TestWindowSimulationStaysInsideTheCap(t *testing.T) {
	for seed := uint64(1); seed <= 6; seed++ {
		prior := int(seed % 3) // 0, 1 or 2 accepts already in the window
		errs, refused, maxIn := simulate(t, seed, prior, 6, true, nil, 8)
		for i, err := range errs {
			if err != nil {
				t.Fatalf("seed %d (prior %d): call %d: %v", seed, prior, i, err)
			}
		}
		if prior == 0 && refused != 0 {
			t.Errorf("seed %d: %d refusals from a quiet window, want none, since pacing should keep every start inside the cap", seed, refused)
		}
		if maxIn > 2 {
			t.Errorf("seed %d: %d accepted submissions inside one minute, want at most 2", seed, maxIn)
		}
	}
}

// TestTheWindowSimulationFailsWithoutPacing proves the pin above is
// load-bearing. Without pacing, six callers land in lockstep on a full
// window, and the hint moves them as one block. A block cannot fit through
// the places that free one at a time, so with four attempts each, some of the
// six always exhaust. With pacing, the same flow on the same seed and the
// same fixed jitter succeeds call by call. The draw is fixed at zero, so the
// outcome is a consequence of the geometry and not a sample, and the count of
// failures is the same on every run.
func TestTheWindowSimulationFailsWithoutPacing(t *testing.T) {
	for seed := uint64(1); seed <= 6; seed++ {
		errs, _, maxIn := simulate(t, seed, 2, 6, false, zero, 4)
		failed := 0
		for i, err := range errs {
			if err == nil {
				continue
			}
			if !limitErr(err) {
				t.Fatalf("seed %d: call %d failed with %v, want a rate-limit refusal", seed, i, err)
			}
			failed++
		}
		if failed == 0 {
			t.Errorf("seed %d: no unpaced call exhausted its attempts, so the simulation no longer reproduces the collision it exists for", seed)
		}
		if failed == 6 {
			t.Errorf("seed %d: every unpaced call failed, want some to get in, since the window does admit two", seed)
		}
		if maxIn > 2 {
			t.Errorf("seed %d: the unpaced flow put %d submissions inside one minute, want at most 2, since the window enforces it", seed, maxIn)
		}

		pacedErrs, pacedRefused, pacedMaxIn := simulate(t, seed, 2, 6, true, zero, 8)
		for i, err := range pacedErrs {
			if err != nil {
				t.Fatalf("seed %d: the paced flow with the same fixed jitter failed on call %d: %v", seed, i, err)
			}
		}
		if pacedMaxIn > 2 {
			t.Errorf("seed %d: the paced flow put %d submissions inside one minute", seed, pacedMaxIn)
		}
		if pacedRefused == 0 {
			t.Errorf("seed %d: the paced flow drew no refusal from a window that started full, which makes the pairing hollow", seed)
		}
	}
}

// rePost runs one set of calls that is cancelled once two submissions have
// been accepted, which is a failed run leaving the window part-spent. It then
// starts a second set at once against the same window, and returns the second
// set's errors and the refusals it drew. With one shared pacer the retry
// queues behind the slots the dead run really used and draws nothing. With a
// pacer per call, which is what an unnamed rate gets, its first submissions
// land inside the leftover minute and are refused.
func rePost(t *testing.T, seed uint64, shared bool) (errs []error, retryRefused int) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 0x7265706f7374))
	clk := &vclock{now: time.Unix(2_000_000, 0)}
	stop := make(chan struct{})
	defer close(stop)
	go clk.drive(stop)

	var rmu sync.Mutex
	win := &window{clk: clk, limit: 2, latency: func() time.Duration {
		rmu.Lock()
		defer rmu.Unlock()
		return 5*time.Second + time.Duration(rng.Int64N(int64(35*time.Second)))
	}}
	cfg := Config{
		PerMinute:  2,
		Attempts:   8,
		Backoff:    10 * time.Second,
		Max:        45 * time.Second,
		RetryAfter: hintOf,
		Now:        clk.Now,
		Sleep:      clk.Sleep,
		randN:      zero,
	}
	var reg *pacerRegistry
	if shared {
		reg = &pacerRegistry{}
		cfg.Name = "repost"
		cfg.registry = reg
	}

	run := func(ctx context.Context) []error {
		const calls = 3
		out := make([]error, calls)
		var wg sync.WaitGroup
		for i := range calls {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out[i] = Retry(ctx, cfg, limitErr, func() error {
					return win.submit(ctx)
				})
			}()
		}
		wg.Wait()
		return out
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		// Watches run A and cancels it once two submissions are accepted.
		// It exits when the test closes stopWatch.
		for {
			select {
			case <-stopWatch:
				return
			case <-time.After(time.Millisecond):
			}
			if _, accepted := win.counts(); accepted >= 2 {
				cancelA()
				return
			}
		}
	}()
	for i, err := range run(ctxA) {
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("seed %d: first run call %d: %v", seed, i, err)
		}
	}
	if shared {
		// Every waiter of the dead run has returned, so none is asleep, and
		// every slot still held must already have arrived. A slot in the
		// future is one the dead run stranded for the next caller.
		now := clk.Now()
		reg.mu.Lock()
		for _, p := range reg.m {
			p.mu.Lock()
			for _, slot := range p.slots {
				if slot.After(now) {
					t.Errorf("seed %d: the cancelled run left a slot %s in the future, so it was never used", seed, slot.Sub(now))
				}
			}
			p.mu.Unlock()
		}
		reg.mu.Unlock()
	}
	before, _ := win.counts() // refusals drawn by the first run
	ctxB, cancelB := context.WithTimeout(context.Background(), time.Minute)
	defer cancelB()
	errs = run(ctxB)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("seed %d: second run call %d: %v", seed, i, err)
		}
	}
	after, _ := win.counts()
	return errs, after - before
}

// TestRerunInsideThePreviousWindowQueuesBehindIt pins the issue's second
// occurrence: a run dies with the window part-spent and the next one starts
// at once. With a shared pacer the retry queues behind the slots the dead run
// really used, its unused slots having been given back, and draws no refusal.
// With a pacer per call, its first submissions land inside the leftover
// minute and are refused.
func TestRerunInsideThePreviousWindowQueuesBehindIt(t *testing.T) {
	for seed := uint64(1); seed <= 4; seed++ {
		_, refused := rePost(t, seed, true)
		if refused != 0 {
			t.Errorf("seed %d, shared pacer: the re-run drew %d refusals, want none", seed, refused)
		}
		_, privateRefused := rePost(t, seed, false)
		if privateRefused == 0 {
			t.Errorf("seed %d, pacer per call: the re-run drew no refusal, so the scenario does not reproduce the leftover-window collision", seed)
		}
	}
}

// TestRetryThroughAGateKeepsTheCap runs the schedule through a one-place
// Gate: one call renders for 100 virtual seconds while three more queue at
// the gate. Every first attempt draws its rate slot after the gate frees, so
// the starts stay spread and the sliding window refuses nothing. A flow that
// took the slot before the gate handed out slots at 31.5s, 63s and 94.5s to
// callers that could only start at 100s and later, and the three starts
// landed back-to-back.
func TestRetryThroughAGateKeepsTheCap(t *testing.T) {
	clk := &vclock{now: time.Unix(2_000_000, 0)}
	stop := make(chan struct{})
	defer close(stop) // the driver's exit
	go clk.drive(stop)

	var submissions atomic.Int64
	win := &window{clk: clk, limit: 2, latency: func() time.Duration {
		if submissions.Add(1) == 1 {
			return 100 * time.Second // the long render that holds the gate
		}
		return 5 * time.Second
	}}
	cfg := Config{
		PerMinute:  2,
		Name:       "gate-order",
		Attempts:   2,
		Backoff:    10 * time.Second,
		Max:        45 * time.Second,
		RetryAfter: hintOf,
		Now:        clk.Now,
		Sleep:      clk.Sleep,
		randN:      zero,
		registry:   &pacerRegistry{},
	}
	gate := New(1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	errs := make([]error, 4)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = gate.Retry(ctx, cfg, limitErr, func() error {
				return win.submit(ctx)
			})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	refused, accepted := win.counts()
	if refused != 0 {
		t.Errorf("%d submissions were refused, want none, since the gate holds the starts behind the rate slots", refused)
	}
	if accepted != 4 {
		t.Errorf("accepted = %d, want 4", accepted)
	}
	win.mu.Lock()
	defer win.mu.Unlock()
	if win.maxIn > 2 {
		t.Errorf("%d accepted submissions sat inside one minute, want at most 2", win.maxIn)
	}
}

// TestRetryGivesTheGateBackWhenThePacerWaitEnds pins the release on the new
// path. A first attempt holds a gate place while it queues for a rate slot,
// and a context that ends in that queue must return the place, so the next
// caller gets in.
func TestRetryGivesTheGateBackWhenThePacerWaitEnds(t *testing.T) {
	clk := &fixedClock{t: time.Unix(1_000_000, 0)}
	g := &gateSleep{}
	g.block.Store(true)
	reg := &pacerRegistry{}
	cfg := Config{PerMinute: 2, Name: "gate-back", Now: clk.now, Sleep: g.sleep, registry: reg}
	gate := New(2)

	started := make(chan struct{})
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	done1 := make(chan error, 1)
	go func() {
		done1 <- gate.Retry(ctx1, cfg, limitErr, func() error {
			close(started) // holds one gate place for the whole test
			<-ctx1.Done()
			return ctx1.Err()
		})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first caller never reached its call")
	}

	// The second caller takes the last place and parks on a future slot.
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- gate.Retry(ctx2, cfg, limitErr, func() error { return nil }) }()
	p := pacerFor(cfg)
	deadline := time.Now().Add(5 * time.Second)
	for p.slotCount() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the second caller never took a rate slot")
		}
		time.Sleep(time.Millisecond)
	}
	cancel2()
	select {
	case err := <-done2:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("second caller err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled queued first attempt did not return")
	}

	// The place is back, so a third caller gets in at once.
	g.block.Store(false)
	done3 := make(chan error, 1)
	go func() { done3 <- gate.Retry(context.Background(), cfg, limitErr, func() error { return nil }) }()
	select {
	case err := <-done3:
		if err != nil {
			t.Fatalf("third caller: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the gate place was not returned when the queued first attempt ended")
	}
}
