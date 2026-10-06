package cache_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nrynss/keel/cache"
)

// base is the instant the injected clocks start at. It carries a non-zero
// nanosecond part, so a store that lost precision would be caught.
var base = time.Date(2026, 1, 1, 8, 0, 0, 123456789, time.UTC)

// errNoFace stands in for the deterministic refusal a provider returns for
// an input it cannot read.
var errNoFace = errors.New("no face in the input")

// shape is the key struct the tests hash. It carries the provider, the
// model, the parameters and the input hashes, the way a caller builds one.
type shape struct {
	Provider string
	Model    string
	Params   map[string]string
	Inputs   []string
}

// mustKey hashes one shape and fails the test when Key refuses it.
func mustKey(t *testing.T, req shape) string {
	t.Helper()
	key, err := cache.Key(req)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	return key
}

// plainKey is a key for a shape the tests never need to spell out twice.
func plainKey(t *testing.T) string {
	t.Helper()
	return mustKey(t, shape{Provider: "speech", Model: "voice-1", Inputs: []string{"aa11"}})
}

// mapStore is an in-memory cache.Store the core tests drive, with two
// injected faults for the read and the write.
type mapStore struct {
	mu     sync.Mutex
	rows   map[string]cache.Entry
	getErr error
	putErr error
}

func newMapStore() *mapStore { return &mapStore{rows: map[string]cache.Entry{}} }

func (m *mapStore) Get(_ context.Context, key string) (cache.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return cache.Entry{}, m.getErr
	}
	entry, ok := m.rows[key]
	if !ok {
		return cache.Entry{}, fmt.Errorf("mapstore: %w", cache.ErrNotFound)
	}
	return entry, nil
}

func (m *mapStore) Put(_ context.Context, key string, entry cache.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.putErr != nil {
		return m.putErr
	}
	m.rows[key] = entry
	return nil
}

func (m *mapStore) row(key string) (cache.Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.rows[key]
	return entry, ok
}

func (m *mapStore) size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

// fakeClock is the injected clock the tests advance by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock(at time.Time) *fakeClock { return &fakeClock{now: at} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// outcome is what one GetOrMake call reported back to its test.
type outcome struct {
	res cache.Result
	err error
}

// callAsync runs one GetOrMake on its own goroutine. It signals arrived
// before it calls, so a test can hold every caller at the door and then
// release the make they are parked on.
func callAsync(c *cache.Cache, ctx context.Context, key string, maker cache.Maker, arrived chan<- struct{}) <-chan outcome {
	ch := make(chan outcome, 1)
	go func() {
		if arrived != nil {
			arrived <- struct{}{}
		}
		res, err := c.GetOrMake(ctx, key, maker)
		ch <- outcome{res: res, err: err}
	}()
	return ch
}

// blockingMaker returns a maker that records its calls and blocks on release
// until the test opens it. It closes started the first time it runs, which
// happens only after the flight for the key is registered.
func blockingMaker(makes *atomic.Int64, payload string, started chan struct{}, release chan struct{}) cache.Maker {
	var once sync.Once
	return func(ctx context.Context) (cache.Result, error) {
		makes.Add(1)
		if started != nil {
			once.Do(func() { close(started) })
		}
		<-release
		return cache.Result{Payload: []byte(payload), ContentType: "application/json", ChargeRef: "charge-1"}, nil
	}
}

// TestNewRejectsNegativeAges: a Config that cannot be honoured refuses
// before any entry is read or written.
func TestNewRejectsNegativeAges(t *testing.T) {
	if _, err := cache.New(cache.Config{MaxAge: -time.Minute}); !errors.Is(err, cache.ErrInvalidConfig) {
		t.Errorf("MaxAge -time.Minute: err = %v, want errors.Is(.., ErrInvalidConfig)", err)
	}
	if _, err := cache.New(cache.Config{RefusalTTL: -time.Minute}); !errors.Is(err, cache.ErrInvalidConfig) {
		t.Errorf("RefusalTTL -time.Minute: err = %v, want errors.Is(.., ErrInvalidConfig)", err)
	}
}

// TestGetOrMakeRunsOneMakeForConcurrentCallers: two callers who arrive
// together for one key trigger one make, and both receive its outcome.
func TestGetOrMakeRunsOneMakeForConcurrentCallers(t *testing.T) {
	store := newMapStore()
	clk := newClock(base)
	c, err := cache.New(cache.Config{Store: store, Now: clk.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key := plainKey(t)

	var makes atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	maker := blockingMaker(&makes, "shared-payload", started, release)

	arrived := make(chan struct{}, 2)
	first := callAsync(c, t.Context(), key, maker, arrived)
	<-started // started closes inside the make, so the flight is registered
	second := callAsync(c, t.Context(), key, maker, arrived)
	<-arrived
	<-arrived // both callers are on their way into GetOrMake
	close(release)

	firstOut, secondOut := <-first, <-second
	if firstOut.err != nil || secondOut.err != nil {
		t.Fatalf("GetOrMake errors: %v, %v", firstOut.err, secondOut.err)
	}
	if makes.Load() != 1 {
		t.Errorf("maker ran %d times, want 1", makes.Load())
	}
	if string(firstOut.res.Payload) != "shared-payload" || string(secondOut.res.Payload) != "shared-payload" {
		t.Errorf("payloads %q and %q, want both %q", firstOut.res.Payload, secondOut.res.Payload, "shared-payload")
	}
	if firstOut.res.ChargeRef != "charge-1" || secondOut.res.ChargeRef != "charge-1" {
		t.Errorf("charge refs %q and %q, want both charge-1", firstOut.res.ChargeRef, secondOut.res.ChargeRef)
	}
	if _, ok := store.row(key); !ok {
		t.Error("the flight stored nothing, want one entry under the key")
	}
}

// TestHitSkipsMakeAndHandsOutCopies: a stored entry answers without a make,
// and every caller receives bytes it owns.
func TestHitSkipsMakeAndHandsOutCopies(t *testing.T) {
	store := newMapStore()
	clk := newClock(base)
	c, err := cache.New(cache.Config{Store: store, Now: clk.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key := plainKey(t)

	var makes atomic.Int64
	plain := func(ctx context.Context) (cache.Result, error) {
		makes.Add(1)
		return cache.Result{Payload: []byte("stored-payload"), ContentType: "application/json", ChargeRef: "charge-1"}, nil
	}
	if _, err := c.GetOrMake(t.Context(), key, plain); err != nil {
		t.Fatalf("first GetOrMake: %v", err)
	}
	// The hit must skip the make entirely, so a maker that reports itself
	// stands in on the second call and must never run.
	hitMaker := func(ctx context.Context) (cache.Result, error) {
		makes.Add(1000)
		return cache.Result{Payload: []byte("must-not-run"), ContentType: "application/json"}, nil
	}
	if _, err := c.GetOrMake(t.Context(), key, hitMaker); err != nil {
		t.Fatalf("second GetOrMake: %v", err)
	}
	if makes.Load() != 1 {
		t.Errorf("maker ran %d times, want 1 on the hit", makes.Load())
	}

	// The bytes a hit hands out are a copy, so a caller that mutates
	// them never reaches the entry or another caller.
	first, err := c.GetOrMake(t.Context(), key, plain)
	if err != nil {
		t.Fatalf("third GetOrMake: %v", err)
	}
	first.Payload[0] = 'X'
	second, err := c.GetOrMake(t.Context(), key, plain)
	if err != nil {
		t.Fatalf("fourth GetOrMake: %v", err)
	}
	if string(second.Payload) != "stored-payload" {
		t.Errorf("payload after a caller mutation reads %q, want stored-payload", second.Payload)
	}
}

// TestExpiryIsAMissThroughTheSameFlight: an entry past its age reads as a
// miss, and two callers who arrive together after expiry trigger one make.
func TestExpiryIsAMissThroughTheSameFlight(t *testing.T) {
	store := newMapStore()
	clk := newClock(base)
	c, err := cache.New(cache.Config{Store: store, MaxAge: time.Hour, Now: clk.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key := plainKey(t)

	var makes atomic.Int64
	firstMaker := func(ctx context.Context) (cache.Result, error) {
		makes.Add(1)
		return cache.Result{Payload: []byte("payload-stale"), ContentType: "application/json"}, nil
	}
	if res, err := c.GetOrMake(t.Context(), key, firstMaker); err != nil || string(res.Payload) != "payload-stale" {
		t.Fatalf("first GetOrMake: res %q err %v", res.Payload, err)
	}

	clk.Advance(2 * time.Hour) // past MaxAge, the entry reads as a miss now

	started := make(chan struct{})
	release := make(chan struct{})
	secondMaker := blockingMaker(&makes, "payload-fresh", started, release)

	arrived := make(chan struct{}, 2)
	left := callAsync(c, t.Context(), key, secondMaker, arrived)
	<-started
	right := callAsync(c, t.Context(), key, secondMaker, arrived)
	<-arrived
	<-arrived
	close(release)

	leftOut, rightOut := <-left, <-right
	if leftOut.err != nil || rightOut.err != nil {
		t.Fatalf("GetOrMake errors after expiry: %v, %v", leftOut.err, rightOut.err)
	}
	if makes.Load() != 2 {
		t.Errorf("maker ran %d times in total, want 2 after expiry", makes.Load())
	}
	if string(leftOut.res.Payload) != "payload-fresh" || string(rightOut.res.Payload) != "payload-fresh" {
		t.Errorf("payloads %q and %q, want both payload-fresh", leftOut.res.Payload, rightOut.res.Payload)
	}
}

// TestFailedMakeIsNotCachedAndARetryWorks: a maker that fails leaves no
// entry behind, so the next caller runs the make again.
func TestFailedMakeIsNotCachedAndARetryWorks(t *testing.T) {
	store := newMapStore()
	clk := newClock(base)
	c, err := cache.New(cache.Config{Store: store, Now: clk.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key := plainKey(t)

	var makes atomic.Int64
	boom := errors.New("provider unreachable")
	maker := func(ctx context.Context) (cache.Result, error) {
		if makes.Add(1) == 1 {
			return cache.Result{}, boom
		}
		return cache.Result{Payload: []byte("recovered"), ContentType: "application/json"}, nil
	}

	res, err := c.GetOrMake(t.Context(), key, maker)
	if res.Payload != nil {
		t.Errorf("a failed make returned payload %q, want none", res.Payload)
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the maker error", err)
	}
	if store.size() != 0 {
		t.Errorf("the store holds %d rows after a failed make, want 0", store.size())
	}

	res, err = c.GetOrMake(t.Context(), key, maker)
	if err != nil {
		t.Fatalf("retry GetOrMake: %v", err)
	}
	if string(res.Payload) != "recovered" {
		t.Errorf("retry payload %q, want recovered", res.Payload)
	}
	if makes.Load() != 2 {
		t.Errorf("maker ran %d times, want 2", makes.Load())
	}
}

// TestPermanentRefusalIsCachedUntilTheTTL: a failure the classifier marks
// permanent is served as a refusal for the TTL, then reads as a miss again.
func TestPermanentRefusalIsCachedUntilTheTTL(t *testing.T) {
	store := newMapStore()
	clk := newClock(base)
	classifier := func(err error) bool { return errors.Is(err, errNoFace) }
	c, err := cache.New(cache.Config{Store: store, RefusalTTL: time.Hour, Classifier: classifier, Now: clk.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key := plainKey(t)

	var makes atomic.Int64
	failing := func(ctx context.Context) (cache.Result, error) {
		makes.Add(1)
		return cache.Result{}, errNoFace
	}

	res, err := c.GetOrMake(t.Context(), key, failing)
	if res.Payload != nil {
		t.Errorf("a refused make returned payload %q, want none", res.Payload)
	}
	if !errors.Is(err, errNoFace) {
		t.Errorf("first err = %v, want the maker error", err)
	}
	if errors.Is(err, cache.ErrRefused) {
		t.Errorf("the caller that ran the make received a refusal wrapper: %v", err)
	}
	if makes.Load() != 1 {
		t.Errorf("maker ran %d times, want 1", makes.Load())
	}

	clk.Advance(30 * time.Minute) // inside the refusal TTL
	_, refused := c.GetOrMake(t.Context(), key, failing)
	if !errors.Is(refused, cache.ErrRefused) {
		t.Errorf("err inside the TTL = %v, want errors.Is(.., ErrRefused)", refused)
	}
	if !strings.Contains(refused.Error(), errNoFace.Error()) {
		t.Errorf("refusal %q does not carry the stored message %q", refused.Error(), errNoFace.Error())
	}
	if makes.Load() != 1 {
		t.Errorf("maker ran %d times inside the TTL, want 1", makes.Load())
	}

	clk.Advance(30 * time.Minute) // exactly at the TTL, the refusal expires
	_, expired := c.GetOrMake(t.Context(), key, failing)
	if !errors.Is(expired, errNoFace) {
		t.Errorf("err at the TTL = %v, want the maker error again", expired)
	}
	if errors.Is(expired, cache.ErrRefused) {
		t.Errorf("the expired refusal was still served: %v", expired)
	}
	if makes.Load() != 2 {
		t.Errorf("maker ran %d times after the TTL, want 2", makes.Load())
	}

	clk.Advance(59 * time.Minute) // a fresh refusal serves within its own life
	_, fresh := c.GetOrMake(t.Context(), key, failing)
	if !errors.Is(fresh, cache.ErrRefused) {
		t.Errorf("err on the fresh refusal = %v, want errors.Is(.., ErrRefused)", fresh)
	}
	if makes.Load() != 2 {
		t.Errorf("maker ran %d times inside the fresh TTL, want 2", makes.Load())
	}
}

// TestTransientFailureIsNeverCached: a failure the classifier does not mark
// permanent leaves the store empty, whatever the error reads like.
func TestTransientFailureIsNeverCached(t *testing.T) {
	store := newMapStore()
	clk := newClock(base)
	classifier := func(err error) bool { return false }
	c, err := cache.New(cache.Config{Store: store, Classifier: classifier, Now: clk.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key := plainKey(t)

	var makes atomic.Int64
	failing := func(ctx context.Context) (cache.Result, error) {
		makes.Add(1)
		return cache.Result{}, errNoFace
	}
	for range 2 {
		if _, err := c.GetOrMake(t.Context(), key, failing); !errors.Is(err, errNoFace) {
			t.Fatalf("GetOrMake err = %v, want the maker error", err)
		}
	}
	if makes.Load() != 2 {
		t.Errorf("maker ran %d times, want 2 without a cached refusal", makes.Load())
	}
	if store.size() != 0 {
		t.Errorf("the store holds %d rows after transient failures, want 0", store.size())
	}
}

// TestChargeReferenceTravelsWithTheEntry: the charge the maker reports is
// stored and served on every hit.
func TestChargeReferenceTravelsWithTheEntry(t *testing.T) {
	store := newMapStore()
	clk := newClock(base)
	c, err := cache.New(cache.Config{Store: store, Now: clk.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key := plainKey(t)

	maker := func(ctx context.Context) (cache.Result, error) {
		return cache.Result{
			Payload:     []byte(`{"line":"hello"}`),
			ContentType: "application/json",
			ChargeRef:   "charge-42",
		}, nil
	}
	first, err := c.GetOrMake(t.Context(), key, maker)
	if err != nil {
		t.Fatalf("first GetOrMake: %v", err)
	}
	second, err := c.GetOrMake(t.Context(), key, maker)
	if err != nil {
		t.Fatalf("hit GetOrMake: %v", err)
	}
	if first.ChargeRef != "charge-42" || second.ChargeRef != "charge-42" {
		t.Errorf("charge refs %q and %q, want charge-42 on make and hit", first.ChargeRef, second.ChargeRef)
	}
	entry, ok := store.row(key)
	if !ok {
		t.Fatal("no entry under the key")
	}
	if entry.ChargeRef != "charge-42" {
		t.Errorf("stored charge ref %q, want charge-42", entry.ChargeRef)
	}
	if entry.Refusal {
		t.Error("a success entry is marked as a refusal")
	}
}

// TestRefusalEntryShape: a cached refusal stores the message and no charge
// reference, so a report never joins savings against a refusal.
func TestRefusalEntryShape(t *testing.T) {
	store := newMapStore()
	clk := newClock(base)
	classifier := func(err error) bool { return errors.Is(err, errNoFace) }
	c, err := cache.New(cache.Config{Store: store, Classifier: classifier, Now: clk.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key := plainKey(t)
	failing := func(ctx context.Context) (cache.Result, error) { return cache.Result{}, errNoFace }
	if _, err := c.GetOrMake(t.Context(), key, failing); !errors.Is(err, errNoFace) {
		t.Fatalf("GetOrMake err = %v, want the maker error", err)
	}
	entry, ok := store.row(key)
	if !ok {
		t.Fatal("no refusal entry under the key")
	}
	if !entry.Refusal {
		t.Error("the entry is not marked as a refusal")
	}
	if string(entry.Payload) != errNoFace.Error() {
		t.Errorf("stored message %q, want %q", entry.Payload, errNoFace.Error())
	}
	if entry.ChargeRef != "" {
		t.Errorf("refusal carries charge ref %q, want none", entry.ChargeRef)
	}
	if entry.ContentType != "text/plain" {
		t.Errorf("refusal content type %q, want text/plain", entry.ContentType)
	}
}

// TestCacheInstancesStayIndependent: flights do not cross instances, and
// each instance expires on its own age.
func TestCacheInstancesStayIndependent(t *testing.T) {
	clk := newClock(base)

	// Two instances without a store run one make each for the same key.
	// A shared flight would collapse them into one.
	firstCache, err := cache.New(cache.Config{Now: clk.Now})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	secondCache, err := cache.New(cache.Config{Now: clk.Now})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	key := plainKey(t)

	var makes atomic.Int64
	release := make(chan struct{})
	maker := blockingMaker(&makes, "own-copy", nil, release)
	arrived := make(chan struct{}, 2)
	left := callAsync(firstCache, t.Context(), key, maker, arrived)
	right := callAsync(secondCache, t.Context(), key, maker, arrived)
	<-arrived
	<-arrived
	close(release)
	if leftOut, rightOut := <-left, <-right; leftOut.err != nil || rightOut.err != nil {
		t.Fatalf("GetOrMake errors across instances: %v, %v", leftOut.err, rightOut.err)
	}
	if makes.Load() != 2 {
		t.Errorf("maker ran %d times across two instances, want 2", makes.Load())
	}

	// Two instances over their own stores keep their own MaxAge.
	storeA, storeB := newMapStore(), newMapStore()
	shortLived, err := cache.New(cache.Config{Store: storeA, MaxAge: time.Hour, Now: clk.Now})
	if err != nil {
		t.Fatalf("short-lived New: %v", err)
	}
	longLived, err := cache.New(cache.Config{Store: storeB, MaxAge: 48 * time.Hour, Now: clk.Now})
	if err != nil {
		t.Fatalf("long-lived New: %v", err)
	}

	var makesA, makesB atomic.Int64
	makerA := func(ctx context.Context) (cache.Result, error) {
		n := makesA.Add(1)
		return cache.Result{Payload: []byte(fmt.Sprintf("a-%d", n)), ContentType: "application/json"}, nil
	}
	makerB := func(ctx context.Context) (cache.Result, error) {
		n := makesB.Add(1)
		return cache.Result{Payload: []byte(fmt.Sprintf("b-%d", n)), ContentType: "application/json"}, nil
	}
	if _, err := shortLived.GetOrMake(t.Context(), key, makerA); err != nil {
		t.Fatalf("short-lived first make: %v", err)
	}
	if _, err := longLived.GetOrMake(t.Context(), key, makerB); err != nil {
		t.Fatalf("long-lived first make: %v", err)
	}

	clk.Advance(2 * time.Hour) // past one age, inside the other
	resA, err := shortLived.GetOrMake(t.Context(), key, makerA)
	if err != nil {
		t.Fatalf("short-lived after expiry: %v", err)
	}
	if string(resA.Payload) != "a-2" {
		t.Errorf("short-lived payload %q, want a fresh make", resA.Payload)
	}
	resB, err := longLived.GetOrMake(t.Context(), key, makerB)
	if err != nil {
		t.Fatalf("long-lived after the same time: %v", err)
	}
	if string(resB.Payload) != "b-1" {
		t.Errorf("long-lived payload %q, want the stored b-1", resB.Payload)
	}
	if makesA.Load() != 2 || makesB.Load() != 1 {
		t.Errorf("makes a=%d b=%d, want 2 and 1", makesA.Load(), makesB.Load())
	}
}

// TestKeyIsDeterministicAndStableAcrossRuns: the same value hashes to the
// same key every time, against the pinned digest of the canonical encoding.
func TestKeyIsDeterministicAndStableAcrossRuns(t *testing.T) {
	req := shape{
		Provider: "image",
		Model:    "render-2",
		Params:   map[string]string{"size": "1024x1024", "style": "flat"},
		Inputs:   []string{"aa11", "bb22"},
	}
	first, err := cache.Key(req)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	second, err := cache.Key(req)
	if err != nil {
		t.Fatalf("Key again: %v", err)
	}
	if first != second {
		t.Errorf("keys %s and %s differ for one value", first, second)
	}
	// The digest is what the canonical JSON encoding of this struct hashes
	// to. Any drift in field order, escaping or map sorting lands here.
	const golden = "c5603f180ce4c53833e8a3865ac520796f76a4267ab0727f7442624ba93acf83"
	if first != golden {
		t.Errorf("key %s, want the pinned canonical digest %s", first, golden)
	}
	if len(first) != 64 {
		t.Errorf("key is %d characters, want 64 hex characters", len(first))
	}
}

// TestKeySeparatesInputsAndMapOrders: a different input changes the key, and
// map iteration order never does.
func TestKeySeparatesInputsAndMapOrders(t *testing.T) {
	original := shape{Provider: "image", Model: "render-2", Inputs: []string{"aa11"}}
	other := shape{Provider: "image", Model: "render-2", Inputs: []string{"cc33"}}
	kept, err := cache.Key(original)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	changed, err := cache.Key(other)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if kept == changed {
		t.Error("two different inputs hashed to one key")
	}

	// The maps hold the same pairs. Canonical JSON sorts their keys, so
	// the order the caller populated a map in never reaches the hash.
	mixed := shape{Provider: "x", Params: map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"}}
	again := shape{Provider: "x", Params: map[string]string{"d": "4", "c": "3", "b": "2", "a": "1"}}
	one, err := cache.Key(mixed)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	two, err := cache.Key(again)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if one != two {
		t.Errorf("keys %s and %s differ for maps with the same pairs", one, two)
	}
}

// TestNilStoreSharesOnlyConcurrentCalls: without a store a Cache
// deduplicates the callers it sees at the same moment, and nothing else.
func TestNilStoreSharesOnlyConcurrentCalls(t *testing.T) {
	clk := newClock(base)
	c, err := cache.New(cache.Config{Now: clk.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key := plainKey(t)

	var makes atomic.Int64
	maker := func(ctx context.Context) (cache.Result, error) {
		makes.Add(1)
		return cache.Result{Payload: []byte("gone-after-the-call"), ContentType: "application/json"}, nil
	}
	for range 2 {
		if _, err := c.GetOrMake(t.Context(), key, maker); err != nil {
			t.Fatalf("GetOrMake: %v", err)
		}
	}
	if makes.Load() != 2 {
		t.Errorf("maker ran %d times without a store, want 2", makes.Load())
	}

	release := make(chan struct{})
	blocking := blockingMaker(&makes, "together", nil, release)
	arrived := make(chan struct{}, 2)
	left := callAsync(c, t.Context(), key, blocking, arrived)
	right := callAsync(c, t.Context(), key, blocking, arrived)
	<-arrived
	<-arrived
	close(release)
	if leftOut, rightOut := <-left, <-right; leftOut.err != nil || rightOut.err != nil {
		t.Fatalf("GetOrMake errors without a store: %v, %v", leftOut.err, rightOut.err)
	}
	if makes.Load() != 3 {
		t.Errorf("maker ran %d times, want one shared make on top of the two", makes.Load())
	}
}

// TestStoreFaultStopsTheMake: a read the store cannot vouch for is returned,
// and a read that reports a missing entry makes.
func TestStoreFaultStopsTheMake(t *testing.T) {
	store := newMapStore()
	clk := newClock(base)
	c, err := cache.New(cache.Config{Store: store, Now: clk.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key := plainKey(t)

	var makes atomic.Int64
	maker := func(ctx context.Context) (cache.Result, error) {
		makes.Add(1)
		return cache.Result{Payload: []byte("made"), ContentType: "application/json"}, nil
	}

	fault := errors.New("reader pool is closed")
	store.getErr = fault
	if _, err := c.GetOrMake(t.Context(), key, maker); !errors.Is(err, fault) {
		t.Errorf("err = %v, want the store fault", err)
	}
	if makes.Load() != 0 {
		t.Errorf("maker ran %d times on a store fault, want 0", makes.Load())
	}

	store.getErr = fmt.Errorf("classified: %w", cache.ErrNotFound)
	if _, err := c.GetOrMake(t.Context(), key, maker); err != nil {
		t.Fatalf("GetOrMake on a miss: %v", err)
	}
	if makes.Load() != 1 {
		t.Errorf("maker ran %d times on a miss, want 1", makes.Load())
	}
}

// TestStoreWriteFailureKeepsTheResult: the caller paid for the make, so a
// failed store write never takes the result away from it.
func TestStoreWriteFailureKeepsTheResult(t *testing.T) {
	store := newMapStore()
	clk := newClock(base)
	c, err := cache.New(cache.Config{Store: store, Now: clk.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key := plainKey(t)

	var makes atomic.Int64
	maker := func(ctx context.Context) (cache.Result, error) {
		makes.Add(1)
		return cache.Result{Payload: []byte("paid-for"), ContentType: "application/json"}, nil
	}

	store.putErr = errors.New("disk full")
	res, err := c.GetOrMake(t.Context(), key, maker)
	if err != nil {
		t.Fatalf("GetOrMake with a failing store write: %v", err)
	}
	if string(res.Payload) != "paid-for" {
		t.Errorf("payload %q, want paid-for", res.Payload)
	}
	if store.size() != 0 {
		t.Errorf("the store holds %d rows, want 0 after failed writes", store.size())
	}

	store.putErr = nil
	if _, err := c.GetOrMake(t.Context(), key, maker); err != nil {
		t.Fatalf("GetOrMake after the write heals: %v", err)
	}
	if makes.Load() != 2 {
		t.Errorf("maker ran %d times, want 2 while nothing could be stored", makes.Load())
	}
}

// TestGetOrMakeRefusesAnEmptyKeyAndANilMaker: the two programming errors
// refuse before anything is read or run.
func TestGetOrMakeRefusesAnEmptyKeyAndANilMaker(t *testing.T) {
	c, err := cache.New(cache.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.GetOrMake(t.Context(), "", func(ctx context.Context) (cache.Result, error) { return cache.Result{}, nil }); err == nil {
		t.Error("an empty key was accepted")
	}
	if _, err := c.GetOrMake(t.Context(), "anything", nil); err == nil {
		t.Error("a nil maker was accepted")
	}
}
