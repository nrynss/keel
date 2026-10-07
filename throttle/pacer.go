package throttle

import (
	"context"
	"sync"
	"time"
)

// Pacer spaces the starts of calls one interval apart, so a fan-out of
// concurrent calls stays inside a per-minute cap instead of spending its
// attempts on refusals. The first start after a quiet spell goes at once and
// every later start takes the earliest slot a full interval clear of the
// slots already handed out.
//
// A slot belongs to a waiter only until it arrives. A waiter whose context
// ends before its slot gives the slot back, so a dead call does not leave its
// queue behind to delay the next one. A slot that has arrived stays spent,
// because its owner may already have submitted. The guarantee is about slots,
// not about the exact instants of submission: a waiter submits after its
// sleep returns and any scheduling delay, which is what the interval's margin
// absorbs.
//
// A pacer holds no goroutine, timer or file, only the slice of slots still
// relevant. It is safe for concurrent use. Build one with NewPacer, or share
// one through Config.Name when a cap is account-wide.
type Pacer struct {
	interval time.Duration // zero means unpaced
	now      func() time.Time
	sleep    func(ctx context.Context, d time.Duration) error

	// mu guards slots and every read of now: the clock is sampled inside the
	// same critical section that reserves, so callers take slots in the
	// order they sampled time and no reservation is reckoned from a moment
	// older than one a previous caller already used. mu never covers a
	// sleep.
	mu    sync.Mutex
	slots []time.Time // handed out and still relevant, ascending
}

// NewPacer returns a Pacer that spaces starts for cfg's per-minute cap. The
// interval is 60s divided by PerMinute, plus a margin of a twentieth of that
// interval, so a wake-up that runs late cannot land two submissions inside
// one provider window. A PerMinute of zero or less returns an unpaced pacer
// whose Wait does nothing, which is what a config with no rate gets. The
// clock and sleep come from cfg, with the real ones for nil fields.
func NewPacer(cfg Config) *Pacer {
	p := &Pacer{now: cfg.Now, sleep: cfg.Sleep}
	if p.now == nil {
		p.now = time.Now
	}
	if p.sleep == nil {
		p.sleep = sleep
	}
	if cfg.PerMinute > 0 {
		base := time.Minute / time.Duration(cfg.PerMinute)
		p.interval = base + base/20
	}
	return p
}

// Wait reserves the earliest free slot and sleeps until it. If ctx ends
// before the slot arrives, the slot is given back for the next caller and
// ctx's error is returned. A context that has already ended gets no slot at
// all. An unpaced pacer returns nil at once.
func (p *Pacer) Wait(ctx context.Context) error {
	if p.interval <= 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	slot, now := p.reserve()
	if d := slot.Sub(now); d > 0 {
		if err := p.sleep(ctx, d); err != nil {
			p.release(slot)
			return err
		}
	}
	return nil
}

// reserve takes the earliest slot at or after now that is a full interval
// clear of every slot already handed out, which may be a gap a cancelled
// waiter left rather than the end of the queue. It returns the slot and the
// instant it was reckoned from. The caller holds no lock.
func (p *Pacer) reserve() (slot, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now = p.now()
	// Slots a full interval behind now can no longer conflict with anything,
	// so they are dropped and the list stays bounded by live callers.
	keep := p.slots[:0]
	for _, s := range p.slots {
		if s.Add(p.interval).After(now) {
			keep = append(keep, s)
		}
	}
	p.slots = keep
	slot = now
	at := len(p.slots)
	for i, s := range p.slots {
		if !s.Add(p.interval).After(slot) {
			continue // wholly before the candidate
		}
		if !s.Before(slot.Add(p.interval)) {
			at = i // the candidate fits before s
			break
		}
		slot = s.Add(p.interval)
	}
	p.slots = append(p.slots, time.Time{})
	copy(p.slots[at+1:], p.slots[at:])
	p.slots[at] = slot
	return slot, now
}

// release returns slot to the pacer if it has not arrived yet. A slot that
// has arrived is left spent, because its owner may already have submitted.
func (p *Pacer) release(slot time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !slot.After(p.now()) {
		return
	}
	for i, s := range p.slots {
		if s.Equal(slot) {
			p.slots = append(p.slots[:i], p.slots[i+1:]...)
			return
		}
	}
}

// pacerKey identifies one provider-side rate window: a cap is per name, such
// as per model, and per rate.
type pacerKey struct {
	name      string
	perMinute int
}

// pacerRegistry hands out one pacer per (name, per-minute). The keys are
// bounded by the names and rates the process is configured with, so entries
// are never evicted. The first creator's clock wins. That is harmless in
// production, where everyone uses the real one, and it is why a config that
// injects a clock must share a registry only with callers injecting the same.
type pacerRegistry struct {
	mu sync.Mutex
	m  map[pacerKey]*Pacer
}

// processPacers is the registry every paced call of the process shares when
// it names its window and injects no clock of its own.
var processPacers = &pacerRegistry{}

// get returns the pacer for name at perMinute, creating it on first use.
func (g *pacerRegistry) get(name string, perMinute int, cfg Config) *Pacer {
	k := pacerKey{name: name, perMinute: perMinute}
	g.mu.Lock()
	defer g.mu.Unlock()
	if p, ok := g.m[k]; ok {
		return p
	}
	if g.m == nil {
		g.m = map[pacerKey]*Pacer{}
	}
	p := NewPacer(cfg)
	g.m[k] = p
	return p
}

// pacerFor picks the pacer one call draws on. An unpaced rate bypasses every
// registry. A named window with no injected clock shares the process-wide
// pacer, because the cap is account-wide and not the caller's. A config that
// injects Now or Sleep, or carries no name, gets a private pacer, so a test's
// clock never leaks into production's and an unnamed rate is nobody else's.
func pacerFor(cfg Config) *Pacer {
	if cfg.PerMinute <= 0 {
		return NewPacer(cfg)
	}
	if cfg.registry != nil {
		return cfg.registry.get(cfg.Name, cfg.PerMinute, cfg)
	}
	if cfg.Name != "" && cfg.Now == nil && cfg.Sleep == nil {
		return processPacers.get(cfg.Name, cfg.PerMinute, cfg)
	}
	return NewPacer(cfg)
}
