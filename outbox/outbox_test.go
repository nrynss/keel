package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/id"
	"github.com/nrynss/keel/outbox"
)

// errSinkDown is the error the test sink refuses with, so a Flush failure
// can be traced back to the sink by errors.Is.
var errSinkDown = errors.New("outbox test: sink down")

// errStoreDown is the error the test store refuses with when a hook makes
// it.
var errStoreDown = errors.New("outbox test: store down")

// base is the instant the injected clock reads, so every stamp in the tests
// is predictable.
var base = time.Date(2025, 1, 1, 12, 0, 0, 5, time.UTC)

// clock returns a Now function that always reads base.
func clock() func() time.Time {
	return func() time.Time { return base }
}

// openBox opens an Outbox for a test and fails the test when Open refuses.
func openBox(t *testing.T, cfg outbox.Config) *outbox.Outbox {
	t.Helper()
	box, err := outbox.Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("outbox.Open: %v", err)
	}
	return box
}

// memStore is an in-memory outbox.Store the core tests drive. It copies
// payloads on the way in, so an entry stored stays independent of the bytes
// its caller mutates afterwards.
type memStore struct {
	mu      sync.Mutex
	entries []outbox.Entry
}

// Add appends one entry, keeping insertion order.
func (s *memStore) Add(ctx context.Context, e outbox.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := e
	stored.Payload = append([]byte(nil), e.Payload...)
	s.entries = append(s.entries, stored)
	return nil
}

// Pending returns the owed entries with fewer than maxFailures, oldest
// first.
func (s *memStore) Pending(ctx context.Context, maxFailures int) ([]outbox.Entry, error) {
	return s.selectEntries(func(failures int) bool { return failures < maxFailures }), nil
}

// Exhausted returns the owed entries with at least maxFailures, oldest
// first.
func (s *memStore) Exhausted(ctx context.Context, maxFailures int) ([]outbox.Entry, error) {
	return s.selectEntries(func(failures int) bool { return failures >= maxFailures }), nil
}

// Delivered drops the named entries. A name with no row is not an error.
func (s *memStore) Delivered(ctx context.Context, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	drop := make(map[string]bool, len(ids))
	for _, entryID := range ids {
		drop[entryID] = true
	}
	kept := s.entries[:0]
	for _, e := range s.entries {
		if !drop[e.ID] {
			kept = append(kept, e)
		}
	}
	s.entries = kept
	return nil
}

// RecordFailures adds one failure to each named entry.
func (s *memStore) RecordFailures(ctx context.Context, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	bump := make(map[string]bool, len(ids))
	for _, entryID := range ids {
		bump[entryID] = true
	}
	for i := range s.entries {
		if bump[s.entries[i].ID] {
			s.entries[i].Failures++
		}
	}
	return nil
}

// selectEntries returns the entries whose failures keep accepts, oldest
// first, as copies with their own payload bytes.
func (s *memStore) selectEntries(keep func(int) bool) []outbox.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []outbox.Entry
	for _, e := range s.entries {
		if keep(e.Failures) {
			copied := e
			copied.Payload = append([]byte(nil), e.Payload...)
			out = append(out, copied)
		}
	}
	return out
}

// hookStore wraps a store and lets a test fail one call.
type hookStore struct {
	outbox.Store
	delivered func(ids []string) error
}

// Delivered runs the hook first, so a failing hook stops the retirement.
func (s *hookStore) Delivered(ctx context.Context, ids []string) error {
	if s.delivered != nil {
		if err := s.delivered(ids); err != nil {
			return err
		}
	}
	return s.Store.Delivered(ctx, ids)
}

// sinkFunc adapts a function to outbox.Sink.
type sinkFunc func(ctx context.Context, batch []outbox.Entry) error

// Deliver calls the function.
func (f sinkFunc) Deliver(ctx context.Context, batch []outbox.Entry) error {
	return f(ctx, batch)
}

// recSink records every batch Deliver receives and fails the first failFor
// calls with errSinkDown, so a test can stage a sink that comes back.
type recSink struct {
	mu      sync.Mutex
	batches [][]outbox.Entry
	times   []time.Time
	failFor int
}

// Deliver records the batch and reports errSinkDown while any failed calls
// remain.
func (s *recSink) Deliver(ctx context.Context, batch []outbox.Entry) error {
	copied := make([]outbox.Entry, len(batch))
	for i, e := range batch {
		copied[i] = e
		copied[i].Payload = append([]byte(nil), e.Payload...)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, copied)
	s.times = append(s.times, time.Now())
	if s.failFor > 0 {
		s.failFor--
		return errSinkDown
	}
	return nil
}

// calls returns the batches Deliver received, in order.
func (s *recSink) calls() [][]outbox.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]outbox.Entry(nil), s.batches...)
}

// callTimes returns the moment each Deliver call started.
func (s *recSink) callTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.times...)
}

// allIDs returns every id the sink saw, in delivery order.
func (s *recSink) allIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, batch := range s.batches {
		for _, e := range batch {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// addEntries adds n entries with payloads event-0 to event-n-1 and returns
// them in the order they were added.
func addEntries(t *testing.T, box *outbox.Outbox, n int) []outbox.Entry {
	t.Helper()
	var added []outbox.Entry
	for i := 0; i < n; i++ {
		e, err := box.Add(t.Context(), []byte(fmt.Sprintf("event-%d", i)))
		if err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
		added = append(added, e)
	}
	return added
}

// TestOpenRefusesMissingPieces: an outbox that could not remember or could
// not deliver refuses to open rather than pretending later.
func TestOpenRefusesMissingPieces(t *testing.T) {
	store := &memStore{}
	sink := &recSink{}
	if _, err := outbox.Open(t.Context(), outbox.Config{Sink: sink}); !errors.Is(err, outbox.ErrNoStore) {
		t.Errorf("err = %v, want errors.Is(.., ErrNoStore)", err)
	}
	if _, err := outbox.Open(t.Context(), outbox.Config{Store: store}); !errors.Is(err, outbox.ErrNoSink) {
		t.Errorf("err = %v, want errors.Is(.., ErrNoSink)", err)
	}
}

// TestOpenPreflightFailsWhenStoreDoes: an open over a store that cannot be
// read stops at the open, not at the first flush.
func TestOpenPreflightFailsWhenStoreDoes(t *testing.T) {
	hook := &hookStore{Store: &memStore{}}
	store := &failPendingStore{Store: hook, err: errStoreDown}
	if _, err := outbox.Open(t.Context(), outbox.Config{Store: store, Sink: &recSink{}}); !errors.Is(err, errStoreDown) {
		t.Errorf("err = %v, want errors.Is(.., errStoreDown)", err)
	}
}

// failPendingStore fails every Pending read with its error.
type failPendingStore struct {
	outbox.Store
	err error
}

// Pending fails with the stored error.
func (s *failPendingStore) Pending(ctx context.Context, maxFailures int) ([]outbox.Entry, error) {
	return nil, s.err
}

// TestAddAssignsStableIDs: Add assigns an unguessable id per entry, stamps
// the configured clock, and hands the payload back unchanged.
func TestAddAssignsStableIDs(t *testing.T) {
	store := &memStore{}
	box := openBox(t, outbox.Config{Store: store, Sink: &recSink{}, Now: clock()})

	first, err := box.Add(t.Context(), []byte("event-a"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	second, err := box.Add(t.Context(), []byte("event-b"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if !id.Valid(first.ID) || !id.Valid(second.ID) {
		t.Errorf("ids %q %q, want two valid 32-hex ids", first.ID, second.ID)
	}
	if first.ID == second.ID {
		t.Errorf("both adds returned id %q, want distinct ids", first.ID)
	}
	if !first.AddedAt.Equal(base) || !second.AddedAt.Equal(base) {
		t.Errorf("AddedAt %v %v, want the configured clock %v", first.AddedAt, second.AddedAt, base)
	}
	if first.Failures != 0 || second.Failures != 0 {
		t.Errorf("Failures %d %d, want fresh entries at zero", first.Failures, second.Failures)
	}
	if string(first.Payload) != "event-a" {
		t.Errorf("payload = %q, want event-a", first.Payload)
	}
}

// TestFlushDeliversInInsertionOrderAndRetires: one pass hands every pending
// entry to the sink oldest first and retires exactly what the sink accepted.
func TestFlushDeliversInInsertionOrderAndRetires(t *testing.T) {
	store := &memStore{}
	sink := &recSink{}
	box := openBox(t, outbox.Config{Store: store, Sink: sink})
	added := addEntries(t, box, 5)

	sum, err := box.Flush(t.Context())
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if sum.Delivered != 5 {
		t.Errorf("Delivered = %d, want 5", sum.Delivered)
	}
	calls := sink.calls()
	if len(calls) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(calls))
	}
	for i, e := range calls[0] {
		if e.ID != added[i].ID {
			t.Errorf("batch[%d].ID = %s, want %s", i, e.ID, added[i].ID)
		}
		if string(e.Payload) != fmt.Sprintf("event-%d", i) {
			t.Errorf("batch[%d].Payload = %q, want event-%d", i, e.Payload, i)
		}
	}
	if pending, err := box.Pending(t.Context()); err != nil || len(pending) != 0 {
		t.Errorf("Pending after flush = %v, %v, want none owed", pending, err)
	}
	if exhausted, err := box.Exhausted(t.Context()); err != nil || len(exhausted) != 0 {
		t.Errorf("Exhausted after flush = %v, %v, want none", exhausted, err)
	}
}

// TestFlushBatchesByConfigSize: the batch size bounds one Deliver call, and
// the batches walk the queue in insertion order.
func TestFlushBatchesByConfigSize(t *testing.T) {
	store := &memStore{}
	sink := &recSink{}
	box := openBox(t, outbox.Config{Store: store, Sink: sink, BatchSize: 2})
	added := addEntries(t, box, 5)

	sum, err := box.Flush(t.Context())
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if sum.Delivered != 5 {
		t.Errorf("Delivered = %d, want 5", sum.Delivered)
	}
	calls := sink.calls()
	if len(calls) != 3 {
		t.Fatalf("sink calls = %d, want 3 batches of 2, 2 and 1", len(calls))
	}
	sizes := []int{len(calls[0]), len(calls[1]), len(calls[2])}
	if sizes[0] != 2 || sizes[1] != 2 || sizes[2] != 1 {
		t.Fatalf("batch sizes = %v, want 2 2 1", sizes)
	}
	var got []string
	for _, batch := range calls {
		for _, e := range batch {
			got = append(got, e.ID)
		}
	}
	for i, want := range added {
		if got[i] != want.ID {
			t.Errorf("delivered[%d] = %s, want %s", i, got[i], want.ID)
		}
	}
}

// TestFlushWithNothingPendingIsQuiet: a pass over an empty queue calls the
// sink no times and reports no error.
func TestFlushWithNothingPendingIsQuiet(t *testing.T) {
	sink := &recSink{}
	box := openBox(t, outbox.Config{Store: &memStore{}, Sink: sink})
	sum, err := box.Flush(t.Context())
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if sum.Delivered != 0 {
		t.Errorf("Delivered = %d, want 0", sum.Delivered)
	}
	if calls := len(sink.calls()); calls != 0 {
		t.Errorf("sink calls = %d, want 0", calls)
	}
}

// TestFailedBatchEndsPassAndRecordsFailures: a batch the sink refuses ends
// the pass, records one failure per entry in that batch, and leaves the
// entries behind it untouched until the next pass.
func TestFailedBatchEndsPassAndRecordsFailures(t *testing.T) {
	store := &memStore{}
	sink := &recSink{failFor: 1}
	box := openBox(t, outbox.Config{Store: store, Sink: sink, BatchSize: 2})
	added := addEntries(t, box, 4)

	sum, err := box.Flush(t.Context())
	if !errors.Is(err, errSinkDown) {
		t.Fatalf("err = %v, want errors.Is(.., errSinkDown)", err)
	}
	if sum.Delivered != 0 {
		t.Errorf("Delivered = %d, want 0 on a failed pass", sum.Delivered)
	}
	if calls := len(sink.calls()); calls != 1 {
		t.Fatalf("sink calls = %d, want 1, because a refused batch ends the pass", calls)
	}
	pending, err := box.Pending(t.Context())
	if err != nil || len(pending) != 4 {
		t.Fatalf("Pending = %v, %v, want all four entries still owed", pending, err)
	}
	for i, e := range pending {
		want := 0
		if i < 2 {
			want = 1
		}
		if e.Failures != want {
			t.Errorf("entry %d Failures = %d, want %d", i, e.Failures, want)
		}
	}
	for i, e := range added {
		if e.Failures != 0 {
			t.Errorf("Add return %d already carried Failures %d, want the stored count to change, not the caller's copy", i, e.Failures)
		}
	}

	sum, err = box.Flush(t.Context())
	if err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if sum.Delivered != 4 {
		t.Errorf("Delivered on the recovered pass = %d, want 4", sum.Delivered)
	}
	if pending, err := box.Pending(t.Context()); err != nil || len(pending) != 0 {
		t.Errorf("Pending = %v, %v, want an empty queue", pending, err)
	}
}

// TestCancelledPassRecordsNoFailure: a pass that ends because its context
// ended is not a delivery verdict, so the attempt budget is not spent.
func TestCancelledPassRecordsNoFailure(t *testing.T) {
	store := &memStore{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sink := sinkFunc(func(ctx context.Context, batch []outbox.Entry) error {
		cancel()
		return context.Canceled
	})
	box := openBox(t, outbox.Config{Store: store, Sink: sink})
	addEntries(t, box, 2)

	if _, err := box.Flush(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want errors.Is(.., context.Canceled)", err)
	}
	pending, err := box.Pending(t.Context())
	if err != nil || len(pending) != 2 {
		t.Fatalf("Pending = %v, %v, want both entries still owed", pending, err)
	}
	for _, e := range pending {
		if e.Failures != 0 {
			t.Errorf("Failures = %d, want 0 after a cancelled pass", e.Failures)
		}
	}
}

// TestExhaustedEntriesStayAndQueueMovesOn: an entry that spent its attempts
// stays stored with its count, is reported, and no longer blocks the
// entries behind it.
func TestExhaustedEntriesStayAndQueueMovesOn(t *testing.T) {
	store := &memStore{}
	sink := &recSink{failFor: 2}
	box := openBox(t, outbox.Config{Store: store, Sink: sink, BatchSize: 1, MaxAttempts: 2})
	added := addEntries(t, box, 2)

	for pass := 0; pass < 2; pass++ {
		if _, err := box.Flush(t.Context()); !errors.Is(err, errSinkDown) {
			t.Fatalf("pass %d err = %v, want errors.Is(.., errSinkDown)", pass, err)
		}
	}

	exhausted, err := box.Exhausted(t.Context())
	if err != nil {
		t.Fatalf("Exhausted: %v", err)
	}
	if len(exhausted) != 1 || exhausted[0].ID != added[0].ID || exhausted[0].Failures != 2 {
		t.Fatalf("Exhausted = %+v, want the first entry with two failures", exhausted)
	}
	pending, err := box.Pending(t.Context())
	if err != nil || len(pending) != 2 {
		t.Fatalf("Pending = %v, %v, want the exhausted entry still stored", pending, err)
	}

	sum, err := box.Flush(t.Context())
	if err != nil {
		t.Fatalf("Flush past the exhausted entry: %v", err)
	}
	if sum.Delivered != 1 {
		t.Errorf("Delivered = %d, want the entry behind the exhausted one", sum.Delivered)
	}
	calls := sink.calls()
	if len(calls) != 3 || calls[2][0].ID != added[1].ID {
		t.Fatalf("sink calls = %d, want the third call to carry the second entry, got %+v", len(calls), calls)
	}
	if remaining, err := box.Exhausted(t.Context()); err != nil || len(remaining) != 1 {
		t.Errorf("Exhausted = %v, %v, want the spent entry still reported", remaining, err)
	}
	if after, err := box.Pending(t.Context()); err != nil || len(after) != 1 || after[0].ID != added[0].ID {
		t.Errorf("Pending = %v, %v, want only the exhausted entry still owed", after, err)
	}
}

// TestReplayCarriesSameStableIDs: a batch that replays carries the ids it
// carried the first time, so a sink that records them can drop the repeat.
func TestReplayCarriesSameStableIDs(t *testing.T) {
	store := &memStore{}
	sink := &recSink{failFor: 1}
	box := openBox(t, outbox.Config{Store: store, Sink: sink, BatchSize: 2})
	addEntries(t, box, 3)

	if _, err := box.Flush(t.Context()); !errors.Is(err, errSinkDown) {
		t.Fatalf("first Flush err = %v, want errors.Is(.., errSinkDown)", err)
	}
	if _, err := box.Flush(t.Context()); err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	calls := sink.calls()
	if len(calls) != 3 {
		t.Fatalf("sink calls = %d, want the failed batch, its replay, and the last entry", len(calls))
	}
	first, replay := calls[0], calls[1]
	if len(first) != len(replay) {
		t.Fatalf("replay = %d entries, want the %d the first attempt carried", len(replay), len(first))
	}
	for i := range first {
		if first[i].ID != replay[i].ID {
			t.Errorf("replay[%d].ID = %s, want the stable %s", i, replay[i].ID, first[i].ID)
		}
	}
	if got := sink.allIDs(); len(got) != 5 {
		t.Errorf("sink saw %d ids, want 3 entries over 5 deliveries", len(got))
	}
}

// TestFlushAfterAbandonedBox: entries an abandoned outbox added are still
// there for the next one, which is the durability the outbox sells.
func TestFlushAfterAbandonedBox(t *testing.T) {
	store := &memStore{}
	first := openBox(t, outbox.Config{Store: store, Sink: &recSink{}})
	added := addEntries(t, first, 2)

	sink := &recSink{}
	second := openBox(t, outbox.Config{Store: store, Sink: sink})
	sum, err := second.Flush(t.Context())
	if err != nil {
		t.Fatalf("Flush through the second outbox: %v", err)
	}
	if sum.Delivered != 2 {
		t.Errorf("Delivered = %d, want the two entries the first outbox stored", sum.Delivered)
	}
	calls := sink.calls()
	if len(calls) != 1 || len(calls[0]) != 2 {
		t.Fatalf("sink calls = %v, want one batch of two", calls)
	}
	for i, want := range added {
		if calls[0][i].ID != want.ID {
			t.Errorf("batch[%d].ID = %s, want %s", i, calls[0][i].ID, want.ID)
		}
	}
}

// TestStoreRetireFailureReplays: a batch the store cannot retire stays
// owed, so the next pass replays it to the sink.
func TestStoreRetireFailureReplays(t *testing.T) {
	inner := &memStore{}
	hook := &hookStore{Store: inner}
	fail := true
	hook.delivered = func(ids []string) error {
		if fail {
			return errStoreDown
		}
		return nil
	}
	sink := &recSink{}
	box := openBox(t, outbox.Config{Store: hook, Sink: sink})
	addEntries(t, box, 2)

	if _, err := box.Flush(t.Context()); !errors.Is(err, errStoreDown) {
		t.Fatalf("err = %v, want errors.Is(.., errStoreDown)", err)
	}
	if pending, err := box.Pending(t.Context()); err != nil || len(pending) != 2 {
		t.Fatalf("Pending = %v, %v, want the batch still owed", pending, err)
	}

	fail = false
	sum, err := box.Flush(t.Context())
	if err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if sum.Delivered != 2 {
		t.Errorf("Delivered = %d, want the replayed batch", sum.Delivered)
	}
}

// TestConcurrentAddAndFlush: adds racing a running loop lose nothing and
// deliver exactly once per entry, under the race detector.
func TestConcurrentAddAndFlush(t *testing.T) {
	store := &memStore{}
	sink := &recSink{}
	box := openBox(t, outbox.Config{Store: store, Sink: sink, Interval: time.Millisecond})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The goroutine's exit path: Loop returns when cancel runs below, which
	// the deferred cancel guarantees even on a test failure.
	go box.Loop(ctx)

	const writers = 4
	const perWriter = 50
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		added []string
	)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				e, err := box.Add(t.Context(), []byte(fmt.Sprintf("writer-%d-event-%d", w, i)))
				if err != nil {
					t.Errorf("Add: %v", err)
					return
				}
				mu.Lock()
				added = append(added, e.ID)
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for {
		pending, err := box.Pending(t.Context())
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		if len(pending) == 0 {
			break
		}
		if _, err := box.Flush(t.Context()); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the queue never drained")
		}
	}
	cancel()

	got := sink.allIDs()
	if len(got) != writers*perWriter {
		t.Fatalf("sink saw %d ids, want %d", len(got), writers*perWriter)
	}
	seen := make(map[string]bool, len(added))
	for _, entryID := range got {
		if seen[entryID] {
			t.Errorf("id %s delivered twice with a clean sink", entryID)
		}
		seen[entryID] = true
	}
	for _, want := range added {
		if !seen[want] {
			t.Errorf("added id %s never delivered", want)
		}
	}
}

// TestLoopStopsWhenContextIsDone: Loop is its goroutine's exit path, and a
// done context ends it promptly.
func TestLoopStopsWhenContextIsDone(t *testing.T) {
	sink := &recSink{}
	box := openBox(t, outbox.Config{Store: &memStore{}, Sink: sink, Interval: time.Millisecond})
	if _, err := box.Add(t.Context(), []byte("event-a")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	// The goroutine's exit path: Loop returns when cancel runs, and close
	// below observes it.
	go func() {
		defer close(done)
		box.Loop(ctx)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for len(sink.allIDs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the loop never delivered the entry")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Loop did not return after its context was done")
	}
}

// TestLoopBacksOffAfterFailedPasses: the wait between passes grows after a
// failure, so a down sink is retried on a slowing curve.
func TestLoopBacksOffAfterFailedPasses(t *testing.T) {
	sink := &recSink{failFor: 2}
	box := openBox(t, outbox.Config{
		Store:       &memStore{},
		Sink:        sink,
		Interval:    time.Millisecond,
		RetryWait:   30 * time.Millisecond,
		RetryMax:    50 * time.Millisecond,
		MaxAttempts: 10,
	})
	if _, err := box.Add(t.Context(), []byte("event-a")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The goroutine's exit path: Loop returns when the deferred cancel runs.
	go box.Loop(ctx)

	deadline := time.Now().Add(10 * time.Second)
	for len(sink.callTimes()) < 3 {
		if time.Now().After(deadline) {
			t.Fatal("the loop never reached the recovery pass")
		}
		time.Sleep(time.Millisecond)
	}
	for {
		pending, err := box.Pending(t.Context())
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		if len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the recovery pass never retired the entry")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	times := sink.callTimes()
	if len(times) < 3 {
		t.Fatalf("sink calls = %d, want the two failed passes and the recovery", len(times))
	}
	if gap := times[1].Sub(times[0]); gap < 15*time.Millisecond {
		t.Errorf("first retry after %v, want at least half of the %v retry wait", gap, 30*time.Millisecond)
	}
	if gap := times[2].Sub(times[1]); gap < 15*time.Millisecond {
		t.Errorf("second retry after %v, want the doubled curve to hold", gap)
	}
}
