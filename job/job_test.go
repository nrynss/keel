package job

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/stream"
	"github.com/nrynss/keel/wire"
)

// memStore is the in-memory Store the streaming tests run against. It keeps
// the exact terminal error value, which a durable store cannot, so a test of
// the in-process catch-up keeps the function's own sentinel chain.
type memStore struct {
	mu      sync.Mutex
	order   []string
	records map[string]Record
}

func newMemStore() *memStore {
	return &memStore{records: make(map[string]Record)}
}

func (m *memStore) Create(_ context.Context, rec Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.records[rec.ID]; ok {
		return errors.New("memStore: duplicate id " + rec.ID)
	}
	m.records[rec.ID] = rec
	m.order = append(m.order, rec.ID)
	return nil
}

func (m *memStore) Begin(_ context.Context, rec Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.records[rec.ID]
	if !ok {
		return ErrUnknownJob
	}
	cur.Status = rec.Status
	cur.UpdatedAt = rec.UpdatedAt
	m.records[rec.ID] = cur
	return nil
}

func (m *memStore) SetProgress(_ context.Context, rec Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.records[rec.ID]
	if !ok {
		return ErrUnknownJob
	}
	cur.Progress = rec.Progress
	cur.UpdatedAt = rec.UpdatedAt
	m.records[rec.ID] = cur
	return nil
}

func (m *memStore) Finish(_ context.Context, rec Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.records[rec.ID]
	if !ok {
		return ErrUnknownJob
	}
	cur.Status = rec.Status
	cur.Data = rec.Data
	cur.Err = rec.Err
	cur.UpdatedAt = rec.UpdatedAt
	m.records[rec.ID] = cur
	return nil
}

func (m *memStore) Get(_ context.Context, id string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[id]
	if !ok {
		return Record{}, ErrUnknownJob
	}
	return rec, nil
}

func (m *memStore) Unfinished(_ context.Context) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Record
	for _, id := range m.order {
		if rec := m.records[id]; rec.Status == StatusRunning || rec.Status == StatusQueued {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (m *memStore) Attempts(_ context.Context, id string) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[id]
	if !ok {
		return nil, ErrUnknownJob
	}
	var out []Record
	for _, other := range m.order {
		if row := m.records[other]; row.RootID == rec.RootID {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Attempt < out[j].Attempt })
	return out, nil
}

// newBrokerAndRunner returns a broker and a Runner on it, sharing one
// in-memory store, which is the shape a consumer wires: one broker, one
// runner, topics namespaced by TopicPrefix.
func newBrokerAndRunner(t *testing.T) (*stream.Broker, *Runner) {
	t.Helper()
	return newBrokerAndRunnerKinds(t, nil)
}

// newBrokerAndRunnerKinds is newBrokerAndRunner with configured kinds.
func newBrokerAndRunnerKinds(t *testing.T, kinds map[string]Kind) (*stream.Broker, *Runner) {
	t.Helper()
	b := stream.New(stream.Config{Heartbeat: time.Hour})
	r, err := Open(context.Background(), Config{Broker: b, Store: newMemStore(), Kinds: kinds})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return b, r
}

// subscribeAfter returns the subscription on the job's topic made after
// Start returned, the race every real client faces. Deterministic tests gate
// the Func so events cannot outrun the subscribe.
func subscribeAfter(t *testing.T, b *stream.Broker, id string) *stream.Subscription {
	t.Helper()
	return b.Subscribe(context.Background(), Topic(id))
}

// startGated starts fn behind a gate, subscribes to its topic, and only then
// releases it, so every event fn publishes, the first progress and the
// terminal included, lands in the subscriber's buffer.
//
// Without the gate a test racing Start against Subscribe asserts on a stream
// whose opening events the broker legitimately never delivered: Subscribe is
// from-now-on with no replay, so an fn that publishes on its goroutine's
// first instruction can beat the subscribe and the assertion waits forever
// for an event that was never owed to it.
func startGated(t *testing.T, b *stream.Broker, r *Runner, fn Func) (string, *stream.Subscription) {
	t.Helper()
	gate := make(chan struct{})
	id, err := r.Start(context.Background(), func(ctx context.Context, progress func(Progress)) ([]byte, error) {
		<-gate
		return fn(ctx, progress)
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sub := subscribeAfter(t, b, id)
	close(gate)
	return id, sub
}

// drain collects the next n events from sub, failing the test on timeout.
func drain(t *testing.T, sub *stream.Subscription, n int) []stream.Event {
	t.Helper()
	events := make([]stream.Event, 0, n)
	for len(events) < n {
		select {
		case ev, ok := <-sub.Events:
			if !ok {
				t.Fatalf("stream closed after %d event(s), want %d", len(events), n)
			}
			events = append(events, ev)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out after %d event(s): %+v", len(events), events)
		}
	}
	return events
}

// int64p returns a pointer to v, for the optional numeric progress members.
func int64p(v int64) *int64 { return &v }

// waitStatus polls a job's Result until it reaches want.
func waitStatus(t *testing.T, r *Runner, id string, want Status) Result {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		res, err := r.Result(context.Background(), id)
		if err != nil {
			t.Fatalf("Result(%s): %v", id, err)
		}
		if res.Status == want {
			return res
		}
		if time.Now().After(deadline) {
			t.Fatalf("Result(%s).Status = %q, want %q", id, res.Status, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestTopicFormat pins the topic convention: the broker topic for a job is
// TopicPrefix + id.
func TestTopicFormat(t *testing.T) {
	if got, want := Topic("abc"), "job:abc"; got != want {
		t.Errorf("Topic = %q, want %q", got, want)
	}
}

// TestStartReturnsImmediately: Start hands back an id while fn is still
// running, the short-POST contract.
func TestStartReturnsImmediately(t *testing.T) {
	_, r := newBrokerAndRunner(t)
	release := make(chan struct{})
	started := make(chan struct{})

	id, err := r.Start(context.Background(), func(ctx context.Context, progress func(Progress)) ([]byte, error) {
		close(started)
		<-release
		return []byte("late"), nil
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id == "" {
		t.Fatal("Start returned an empty id")
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("fn never started")
	}
	close(release)

	if _, err := r.Result(context.Background(), id); err != nil {
		t.Errorf("Result: %v", err)
	}
}

// TestProgressAndTerminalExactlyOnce is the core streaming contract: the
// progress events flow to the topic in the wire shape, the terminal done
// event carries the wire status payload, and exactly one terminal event is
// published.
func TestProgressAndTerminalExactlyOnce(t *testing.T) {
	b, r := newBrokerAndRunner(t)

	id, sub := startGated(t, b, r, func(ctx context.Context, progress func(Progress)) ([]byte, error) {
		progress(Progress{Stage: "render", Current: int64p(1), Total: int64p(2)})
		progress(Progress{Stage: "encode", Detail: []byte(`{"step":2}`)})
		return []byte(`{"book":1}`), nil
	})

	events := drain(t, sub, 3)
	first, ok := events[0].Data.(wire.ProgressEvent)
	if !ok {
		t.Fatalf("first event payload = %T, want wire.ProgressEvent", events[0].Data)
	}
	if events[0].Name != wire.EventProgress || first.JobID != id || first.Stage != "render" {
		t.Errorf("first event = %+v, want progress render for %s", events[0], id)
	}
	if first.Current == nil || *first.Current != 1 || first.Total == nil || *first.Total != 2 {
		t.Errorf("first progress counters = %+v, want current 1 total 2", first)
	}
	second, ok := events[1].Data.(wire.ProgressEvent)
	if !ok {
		t.Fatalf("second event payload = %T, want wire.ProgressEvent", events[1].Data)
	}
	if events[1].Name != wire.EventProgress || second.Stage != "encode" || string(second.Detail) != `{"step":2}` {
		t.Errorf("second event = %+v, want progress encode with its detail", events[1])
	}
	if events[2].Name != wire.EventDone {
		t.Errorf("terminal event = %+v, want name %q", events[2], wire.EventDone)
	}
	done, ok := events[2].Data.(wire.StatusEvent)
	if !ok {
		t.Fatalf("terminal payload = %T, want wire.StatusEvent", events[2].Data)
	}
	if done.JobID != id || done.Status != string(StatusDone) {
		t.Errorf("terminal payload = %+v, want job %s status done", done, id)
	}

	// Exactly once: after the terminal, nothing else arrives.
	select {
	case ev := <-sub.Events:
		t.Errorf("unexpected event after terminal: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}

	// Catch-up: the result by id carries the returned bytes.
	res, err := r.Result(context.Background(), id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if res.Status != StatusDone || string(res.Data) != `{"book":1}` || res.Err != nil {
		t.Errorf("Result = %+v, want done with the returned bytes", res)
	}
}

// TestEmptyProgressDropped pins the Func contract: an empty progress report
// is not published, because an eventless data line is wire noise.
func TestEmptyProgressDropped(t *testing.T) {
	b, r := newBrokerAndRunner(t)
	gate := make(chan struct{})

	id, err := r.Start(context.Background(), func(ctx context.Context, progress func(Progress)) ([]byte, error) {
		progress(Progress{})
		<-gate
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	sub := subscribeAfter(t, b, id)
	close(gate)
	events := drain(t, sub, 1)
	if events[0].Name != wire.EventDone {
		t.Errorf("first event = %+v, want the done event with no progress before it", events[0])
	}
}

// TestPanicBecomesErrorTerminal pins the panic policy: a panicking Func is
// recovered at the job boundary, surfaces as exactly one error terminal, and
// the Result carries ErrPanic in its chain. The Runner keeps working
// afterwards.
func TestPanicBecomesErrorTerminal(t *testing.T) {
	b, r := newBrokerAndRunner(t)

	id, sub := startGated(t, b, r, func(ctx context.Context, progress func(Progress)) ([]byte, error) {
		panic("illustrator exploded")
	})
	events := drain(t, sub, 1)
	if events[0].Name != wire.EventError {
		t.Fatalf("terminal event = %+v, want %q", events[0], wire.EventError)
	}
	payload, ok := events[0].Data.(wire.ErrorEvent)
	if !ok {
		t.Fatalf("terminal payload = %T, want wire.ErrorEvent", events[0].Data)
	}
	if payload.JobID != id || !strings.Contains(string(payload.Error), "illustrator exploded") {
		t.Errorf("terminal payload = %s, want the panic value in the envelope", payload.Error)
	}

	res, err := r.Result(context.Background(), id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if res.Status != StatusError {
		t.Errorf("Status = %q, want %q", res.Status, StatusError)
	}
	if !errors.Is(res.Err, ErrPanic) {
		t.Errorf("Err = %v, want errors.Is(.., ErrPanic)", res.Err)
	}
	if res.Data != nil {
		t.Errorf("Data = %q alongside an error, want nil", res.Data)
	}

	// The runner survives the panic: the next job runs normally.
	_, sub2 := startGated(t, b, r, func(ctx context.Context, progress func(Progress)) ([]byte, error) {
		return []byte("ok"), nil
	})
	drain(t, sub2, 1)
}

// TestFnErrorSurfacesSentinel: an error returned by fn, not a panic, becomes
// the error terminal and keeps its sentinel chain for errors.Is at the
// catch-up site.
func TestFnErrorSurfacesSentinel(t *testing.T) {
	b, r := newBrokerAndRunner(t)
	sentinel := errors.New("media: transient")

	id, sub := startGated(t, b, r, func(ctx context.Context, progress func(Progress)) ([]byte, error) {
		return nil, sentinel
	})
	events := drain(t, sub, 1)
	if events[0].Name != wire.EventError {
		t.Fatalf("terminal event = %+v, want %q", events[0], wire.EventError)
	}
	res, err := r.Result(context.Background(), id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if !errors.Is(res.Err, sentinel) {
		t.Errorf("Err = %v, want the fn's own error", res.Err)
	}
}

// TestJobOutlivesRequestContext pins the context policy: cancelling the
// STARTING request's context must not kill the job, because a page
// navigation must not abort a generation. Only values are inherited.
func TestJobOutlivesRequestContext(t *testing.T) {
	b, r := newBrokerAndRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	gate := make(chan struct{})

	id, err := r.Start(ctx, func(jobCtx context.Context, progress func(Progress)) ([]byte, error) {
		<-gate
		if jobCtx.Err() != nil {
			return nil, jobCtx.Err()
		}
		return []byte("survived"), nil
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Subscribe before the job may publish, then cancel the STARTING
	// context while the job is still gated: the terminal it publishes
	// after the release is observed in full.
	sub := subscribeAfter(t, b, id)
	cancel()
	close(gate)

	events := drain(t, sub, 1)
	if events[0].Name != wire.EventDone {
		t.Errorf("terminal event = %+v, want done despite the cancelled start context", events[0])
	}
}

// TestResultUnknownJob pins the catch-up error path: an id this Runner never
// started yields ErrUnknownJob, not a zero Result.
func TestResultUnknownJob(t *testing.T) {
	_, r := newBrokerAndRunner(t)
	res, err := r.Result(context.Background(), "no-such-job")
	if !errors.Is(err, ErrUnknownJob) {
		t.Errorf("err = %v, want errors.Is(.., ErrUnknownJob)", err)
	}
	if res.ID != "" || res.Status != "" {
		t.Errorf("res = %+v alongside an error, want the zero Result", res)
	}
}

// TestOpenRejectsMissingDependencies: a Runner that cannot publish refuses
// to open without a broker, and one that cannot remember refuses without a
// store, rather than running jobs in silence.
func TestOpenRejectsMissingDependencies(t *testing.T) {
	if _, err := Open(context.Background(), Config{Store: newMemStore()}); !errors.Is(err, ErrNoBroker) {
		t.Errorf("err = %v, want errors.Is(.., ErrNoBroker)", err)
	}
	b := stream.New(stream.Config{})
	if _, err := Open(context.Background(), Config{Broker: b}); !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want errors.Is(.., ErrInvalid)", err)
	}
}

// TestStartAtLimit pins the capacity contract: an explicit kind limit and
// the default both return ErrLimit without starting the job once slots are
// full, and a finished job frees its slot.
func TestStartAtLimit(t *testing.T) {
	t.Run("explicit-limit", func(t *testing.T) {
		b, r := newBrokerAndRunnerKinds(t, map[string]Kind{DefaultKind: {Limit: 1}})
		release := make(chan struct{})
		if _, err := r.Start(context.Background(), func(ctx context.Context, progress func(Progress)) ([]byte, error) {
			<-release
			return nil, nil
		}); err != nil {
			t.Fatalf("first Start: %v", err)
		}
		if _, err := r.Start(context.Background(), func(ctx context.Context, progress func(Progress)) ([]byte, error) {
			return nil, nil
		}); !errors.Is(err, ErrLimit) {
			t.Errorf("err = %v, want errors.Is(.., ErrLimit)", err)
		}
		close(release)

		// The slot frees when the job's goroutine has fully exited. Wait
		// for the terminal event, then give Start a bounded window to
		// observe the release. A failed Start begins nothing, so the
		// retry loop leaves exactly one job on the gate.
		gate := make(chan struct{})
		startGatedJob := func() (string, error) {
			return r.Start(context.Background(), func(ctx context.Context, progress func(Progress)) ([]byte, error) {
				<-gate
				return nil, nil
			})
		}
		id, err := startGatedJob()
		deadline := time.Now().Add(2 * time.Second)
		for err != nil {
			if !errors.Is(err, ErrLimit) || time.Now().After(deadline) {
				t.Fatalf("Start after release: %v", err)
			}
			time.Sleep(time.Millisecond)
			id, err = startGatedJob()
		}
		sub := subscribeAfter(t, b, id)
		close(gate)
		drain(t, sub, 1)
	})

	t.Run("default-limit", func(t *testing.T) {
		if DefaultLimit <= 0 {
			t.Fatalf("DefaultLimit = %d, want positive", DefaultLimit)
		}
		_, r := newBrokerAndRunner(t)
		release := make(chan struct{})
		for i := 0; i < DefaultLimit; i++ {
			if _, err := r.Start(context.Background(), func(ctx context.Context, progress func(Progress)) ([]byte, error) {
				<-release
				return nil, nil
			}); err != nil {
				t.Fatalf("Start %d of %d: %v", i+1, DefaultLimit, err)
			}
		}
		if _, err := r.Start(context.Background(), func(ctx context.Context, progress func(Progress)) ([]byte, error) {
			return nil, nil
		}); !errors.Is(err, ErrLimit) {
			t.Errorf("job %d: err = %v, want ErrLimit at the default limit", DefaultLimit+1, err)
		}
		close(release)
	})
}

// TestPerKindLimitsAreIndependent: each kind gates its own jobs, so one kind
// at its limit does not stop another.
func TestPerKindLimitsAreIndependent(t *testing.T) {
	_, r := newBrokerAndRunnerKinds(t, map[string]Kind{
		"render": {Limit: 1},
		"index":  {Limit: 1},
	})
	release := make(chan struct{})
	hold := func(ctx context.Context, progress func(Progress)) ([]byte, error) {
		<-release
		return nil, nil
	}
	if _, err := r.StartKind(context.Background(), "render", hold); err != nil {
		t.Fatalf("render Start: %v", err)
	}
	if _, err := r.StartKind(context.Background(), "render", hold); !errors.Is(err, ErrLimit) {
		t.Errorf("second render Start = %v, want ErrLimit", err)
	}
	if _, err := r.StartKind(context.Background(), "index", hold); err != nil {
		t.Errorf("index Start = %v, want a free slot in its own kind", err)
	}
	// An unconfigured kind still gets the default limit.
	if _, err := r.StartKind(context.Background(), "extra", hold); err != nil {
		t.Errorf("extra Start = %v, want the zero Kind's default limit", err)
	}
	close(release)
}

// TestMidJobSubscriberCatchesUp: a subscriber joining mid-job receives the
// progress published after it joined plus the terminal, and a subscriber
// joining after the terminal catches up on the result by id, which is the
// subscribe-then-Result order.
func TestMidJobSubscriberCatchesUp(t *testing.T) {
	b, r := newBrokerAndRunner(t)
	joined := make(chan struct{})
	gate := make(chan struct{})

	id, err := r.Start(context.Background(), func(ctx context.Context, progress func(Progress)) ([]byte, error) {
		progress(Progress{Stage: "early"})
		close(joined)
		<-gate
		progress(Progress{Stage: "late"})
		return []byte(`{"book":"final"}`), nil
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	<-joined // the early progress has already fired, so this subscriber missed it
	sub := subscribeAfter(t, b, id)
	close(gate)

	events := drain(t, sub, 2)
	late, ok := events[0].Data.(wire.ProgressEvent)
	if !ok || events[0].Name != wire.EventProgress || late.Stage != "late" {
		t.Errorf("first event = %+v, want the late progress", events[0])
	}
	if events[1].Name != wire.EventDone {
		t.Errorf("second event = %+v, want the terminal", events[1])
	}

	// A page reloading after the terminal: full catch-up by id.
	res, err := r.Result(context.Background(), id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if res.Status != StatusDone || string(res.Data) != `{"book":"final"}` {
		t.Errorf("catch-up Result = %+v, want the done result with bytes", res)
	}

	// The from-now-on stream contract: progress never replays.
	select {
	case ev := <-sub.Events:
		t.Errorf("unexpected replay after terminal: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestLateSubscriberGetsTerminal pins the retained terminal: a subscriber
// joining after the job ended still receives the terminal event once, which
// requires the publish to be marked terminal.
func TestLateSubscriberGetsTerminal(t *testing.T) {
	b, r := newBrokerAndRunner(t)
	id, _ := startGated(t, b, r, func(ctx context.Context, progress func(Progress)) ([]byte, error) {
		return []byte("ok"), nil
	})
	waitStatus(t, r, id, StatusDone)

	sub := b.Subscribe(context.Background(), Topic(id))
	defer sub.Cancel()
	events := drain(t, sub, 1)
	if events[0].Name != wire.EventDone {
		t.Errorf("retained event = %+v, want the done terminal", events[0])
	}
}

// TestRunningStatusBeforeTerminal: Result reports StatusRunning while fn is
// still going, so a poller joining mid-job can distinguish the two.
func TestRunningStatusBeforeTerminal(t *testing.T) {
	_, r := newBrokerAndRunner(t)
	gate := make(chan struct{})
	release := make(chan struct{})

	id, err := r.Start(context.Background(), func(ctx context.Context, progress func(Progress)) ([]byte, error) {
		close(gate)
		<-release
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-gate

	res, err := r.Result(context.Background(), id)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if res.Status != StatusRunning {
		t.Errorf("Status mid-job = %q, want %q", res.Status, StatusRunning)
	}
	close(release)
}

// TestCancelRunningJobRecoversSlot: idiomatic fns, the shape that returns on
// ctx.Done(), hold both slots. Cancel ends each job, the cancelled terminal
// is published exactly once through run's single site, and the slots
// recover.
func TestCancelRunningJobRecoversSlot(t *testing.T) {
	b, r := newBrokerAndRunnerKinds(t, map[string]Kind{DefaultKind: {Limit: 2}})
	started := make(chan struct{}, 2)
	blocked := func(ctx context.Context, progress func(Progress)) ([]byte, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	id1, err := r.Start(context.Background(), blocked)
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	id2, err := r.Start(context.Background(), blocked)
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	<-started
	<-started
	if _, err := r.Start(context.Background(), blocked); !errors.Is(err, ErrLimit) {
		t.Fatalf("err = %v, want ErrLimit with both slots held", err)
	}

	// Subscribe before cancelling: neither fn can return before Cancel, so
	// no terminal can outrun its subscription.
	subs := map[string]*stream.Subscription{
		id1: subscribeAfter(t, b, id1),
		id2: subscribeAfter(t, b, id2),
	}
	if err := r.Cancel(id1); err != nil {
		t.Fatalf("Cancel(%s): %v", id1, err)
	}
	if err := r.Cancel(id2); err != nil {
		t.Fatalf("Cancel(%s): %v", id2, err)
	}

	for _, id := range []string{id1, id2} {
		events := drain(t, subs[id], 1)
		if events[0].Name != string(StatusCancelled) {
			t.Errorf("terminal event = %+v, want name %q", events[0], StatusCancelled)
		}
		payload, ok := events[0].Data.(wire.StatusEvent)
		if !ok || payload.JobID != id || payload.Status != string(StatusCancelled) {
			t.Errorf("cancelled payload = %+v, want job %s status cancelled", events[0].Data, id)
		}
		select {
		case ev := <-subs[id].Events:
			t.Errorf("second terminal after Cancel on %s: %+v", id, ev)
		case <-time.After(100 * time.Millisecond):
		}
		res, err := r.Result(context.Background(), id)
		if err != nil {
			t.Fatalf("Result(%s): %v", id, err)
		}
		if res.Status != StatusCancelled || res.Data != nil || !errors.Is(res.Err, context.Canceled) {
			t.Errorf("Result(%s) = %+v, want cancelled with context.Canceled in the chain and nil data", id, res)
		}
	}

	// The slots recovered: a fresh Start succeeds on a bounded retry,
	// because the slot releases when run's defer fires, just after the
	// terminal.
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := r.Start(context.Background(), func(ctx context.Context, progress func(Progress)) ([]byte, error) {
			return []byte("fresh"), nil
		})
		if err == nil {
			break
		}
		if !errors.Is(err, ErrLimit) || time.Now().After(deadline) {
			t.Fatalf("Start after cancelling both jobs: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestCancelIdempotentAndUnknown pins Cancel's handle contract: an unknown id
// reports ErrUnknownJob, a second Cancel on a registered job is a nil no-op,
// and once the job has terminated the entry is gone, so Cancel reports
// ErrUnknownJob instead of leaking the handle.
func TestCancelIdempotentAndUnknown(t *testing.T) {
	b, r := newBrokerAndRunner(t)
	if err := r.Cancel("no-such-job"); !errors.Is(err, ErrUnknownJob) {
		t.Errorf("err = %v, want errors.Is(.., ErrUnknownJob)", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	id, err := r.Start(context.Background(), func(ctx context.Context, progress func(Progress)) ([]byte, error) {
		close(started)
		<-ctx.Done()
		<-release // hold the job so the second Cancel lands while registered
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-started

	if err := r.Cancel(id); err != nil {
		t.Fatalf("first Cancel: %v", err)
	}
	if err := r.Cancel(id); err != nil {
		t.Errorf("second Cancel = %v, want nil while the job is registered", err)
	}
	// Subscribe before releasing the job: the cancelled terminal is
	// published the moment fn returns, so it must not run ahead of the
	// subscription.
	sub := subscribeAfter(t, b, id)
	close(release)
	drain(t, sub, 1) // the cancelled terminal, exactly once

	// After the goroutine exits the entry is gone.
	deadline := time.Now().Add(2 * time.Second)
	for err := r.Cancel(id); !errors.Is(err, ErrUnknownJob); err = r.Cancel(id) {
		if time.Now().After(deadline) {
			t.Fatal("Cancel still succeeds after the job terminated, the registry entry leaked")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestResultCapFailsJob pins the result cap: a result over the configured
// size fails the job with ErrResultTooLarge and no data, and the sentinel
// Unbounded disables the cap.
func TestResultCapFailsJob(t *testing.T) {
	t.Run("over-cap", func(t *testing.T) {
		b := stream.New(stream.Config{Heartbeat: time.Hour})
		r, err := Open(context.Background(), Config{Broker: b, Store: newMemStore(), MaxResultBytes: 8})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		id, sub := startGated(t, b, r, func(ctx context.Context, progress func(Progress)) ([]byte, error) {
			return []byte("123456789"), nil
		})
		events := drain(t, sub, 1)
		if events[0].Name != wire.EventError {
			t.Fatalf("terminal event = %+v, want %q", events[0], wire.EventError)
		}
		res := waitStatus(t, r, id, StatusError)
		if !errors.Is(res.Err, ErrResultTooLarge) {
			t.Errorf("Err = %v, want errors.Is(.., ErrResultTooLarge)", res.Err)
		}
		if res.Data != nil {
			t.Errorf("Data = %q, want nil alongside the cap failure", res.Data)
		}
	})

	t.Run("unbounded", func(t *testing.T) {
		b := stream.New(stream.Config{Heartbeat: time.Hour})
		r, err := Open(context.Background(), Config{Broker: b, Store: newMemStore(), MaxResultBytes: Unbounded})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		big := make([]byte, 4*DefaultMaxResultBytes)
		id, _ := startGated(t, b, r, func(ctx context.Context, progress func(Progress)) ([]byte, error) {
			return big, nil
		})
		res := waitStatus(t, r, id, StatusDone)
		if len(res.Data) != len(big) {
			t.Errorf("len(Data) = %d, want %d with the cap disabled", len(res.Data), len(big))
		}
	})
}

// TestFailedJobKeepsReturnedBytes pins the resolved result contract: a failed
// job keeps the bytes its function returned, and only a result over the cap
// drops them, which is what the Result docs state.
func TestFailedJobKeepsReturnedBytes(t *testing.T) {
	_, r := newBrokerAndRunner(t)
	id, err := r.Start(context.Background(), func(context.Context, func(Progress)) ([]byte, error) {
		return []byte("partial"), errors.New("boom")
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := waitStatus(t, r, id, StatusError)
	if string(res.Data) != "partial" {
		t.Errorf("Data = %q, want the returned bytes kept on failure", res.Data)
	}
}

// TestRestartInterruptsUnfinished pins the restart policy: a job a previous
// process left running becomes interrupted, publishes that terminal, and
// reports ErrInterrupted from its result.
func TestRestartInterruptsUnfinished(t *testing.T) {
	store := newMemStore()
	rec := Record{ID: "interrupted-1", Kind: "render", Status: StatusRunning, Attempt: 1, RootID: "interrupted-1"}
	if err := store.Create(context.Background(), rec); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	b := stream.New(stream.Config{Heartbeat: time.Hour})
	r, err := Open(context.Background(), Config{Broker: b, Store: store})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	sub := b.Subscribe(context.Background(), Topic(rec.ID))
	defer sub.Cancel()
	events := drain(t, sub, 1)
	if events[0].Name != wire.EventInterrupted {
		t.Errorf("terminal event = %+v, want %q", events[0], wire.EventInterrupted)
	}
	payload, ok := events[0].Data.(wire.StatusEvent)
	if !ok || payload.JobID != rec.ID || payload.Status != string(StatusInterrupted) {
		t.Errorf("interrupted payload = %+v, want job %s status interrupted", events[0].Data, rec.ID)
	}

	res, err := r.Result(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if res.Status != StatusInterrupted || !errors.Is(res.Err, ErrInterrupted) {
		t.Errorf("Result = %+v, want interrupted with ErrInterrupted", res)
	}
}

// TestRestartResumesIdempotentKind pins the resume policy: an idempotent kind
// with resume work runs a fresh attempt on its own id, records the lineage,
// and can be walked from the interrupted attempt to the replacement.
func TestRestartResumesIdempotentKind(t *testing.T) {
	store := newMemStore()
	rec := Record{ID: "attempt-1", Kind: "index", Status: StatusRunning, Attempt: 1, RootID: "attempt-1"}
	if err := store.Create(context.Background(), rec); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	kinds := map[string]Kind{
		"index": {
			Idempotent:  true,
			MaxAttempts: 2,
			Resume: func(got Record) (Func, error) {
				if got.ID != rec.ID || got.Kind != rec.Kind {
					t.Errorf("Resume record = %+v, want the interrupted job", got)
				}
				return func(ctx context.Context, progress func(Progress)) ([]byte, error) {
					return []byte("resumed"), nil
				}, nil
			},
		},
	}
	b := stream.New(stream.Config{Heartbeat: time.Hour})
	r, err := Open(context.Background(), Config{Broker: b, Store: store, Kinds: kinds})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	attempts, err := r.Attempts(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("Attempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(attempts))
	}
	if attempts[0].ID != rec.ID || attempts[0].Status != StatusInterrupted || attempts[0].Attempt != 1 {
		t.Errorf("first attempt = %+v, want the interrupted original", attempts[0])
	}
	next := attempts[1]
	if next.Attempt != 2 || next.ParentID != rec.ID || next.RootID != rec.ID {
		t.Errorf("second attempt = %+v, want attempt 2 with parent %s", next, rec.ID)
	}
	if next.ID == rec.ID {
		t.Error("the resumed attempt reused the interrupted id")
	}
	final := waitStatus(t, r, next.ID, StatusDone)
	if string(final.Data) != "resumed" {
		t.Errorf("resumed Data = %q, want the resumed bytes", final.Data)
	}
}

// TestRestartDoesNotResume covers the kinds the restart policy leaves alone:
// a kind that is not idempotent, one with no resume work, and one whose
// attempt limit is spent.
func TestRestartDoesNotResume(t *testing.T) {
	cases := map[string]Kind{
		"not-idempotent": {MaxAttempts: 3, Resume: func(Record) (Func, error) { return nil, nil }},
		"no-resume-work": {Idempotent: true, MaxAttempts: 3},
		"attempts-spent": {Idempotent: true, MaxAttempts: 1, Resume: func(Record) (Func, error) {
			return func(context.Context, func(Progress)) ([]byte, error) { return nil, nil }, nil
		}},
	}
	for name, kind := range cases {
		t.Run(name, func(t *testing.T) {
			store := newMemStore()
			rec := Record{ID: name, Kind: "render", Status: StatusRunning, Attempt: 1, RootID: name}
			if err := store.Create(context.Background(), rec); err != nil {
				t.Fatalf("seed Create: %v", err)
			}
			b := stream.New(stream.Config{Heartbeat: time.Hour})
			r, err := Open(context.Background(), Config{Broker: b, Store: store, Kinds: map[string]Kind{"render": kind}})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			attempts, err := r.Attempts(context.Background(), rec.ID)
			if err != nil {
				t.Fatalf("Attempts: %v", err)
			}
			if len(attempts) != 1 || attempts[0].Status != StatusInterrupted {
				t.Errorf("attempts = %+v, want the one interrupted original", attempts)
			}
		})
	}
}

// TestAttemptsUnknownJob: walking the lineage of an id the store never held
// reports ErrUnknownJob.
func TestAttemptsUnknownJob(t *testing.T) {
	_, r := newBrokerAndRunner(t)
	if _, err := r.Attempts(context.Background(), "no-such-job"); !errors.Is(err, ErrUnknownJob) {
		t.Errorf("err = %v, want errors.Is(.., ErrUnknownJob)", err)
	}
}

// TestRestartResumesEveryUnfinishedRecord pins the capacity case: when more
// unfinished records of a kind exist than that kind has slots, every eligible
// record still gets its replacement attempt once a slot frees, rather than
// being dropped.
func TestRestartResumesEveryUnfinishedRecord(t *testing.T) {
	store := newMemStore()
	ids := []string{"pending-1", "pending-2", "pending-3"}
	for _, id := range ids {
		rec := Record{ID: id, Kind: "index", Status: StatusRunning, Attempt: 1, RootID: id}
		if err := store.Create(context.Background(), rec); err != nil {
			t.Fatalf("seed Create %s: %v", id, err)
		}
	}
	kinds := map[string]Kind{
		"index": {
			Limit:       1,
			Idempotent:  true,
			MaxAttempts: 2,
			Resume: func(Record) (Func, error) {
				return func(context.Context, func(Progress)) ([]byte, error) {
					return []byte("resumed"), nil
				}, nil
			},
		},
	}
	b := stream.New(stream.Config{Heartbeat: time.Hour})
	r, err := Open(context.Background(), Config{Broker: b, Store: store, Kinds: kinds})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, id := range ids {
		attempts := waitForAttempts(t, r, id, 2)
		if first := attempts[0]; first.Status != StatusInterrupted || first.Attempt != 1 {
			t.Errorf("%s first attempt = %+v, want the interrupted original", id, first)
		}
		next := attempts[1]
		if next.Attempt != 2 || next.ParentID != id || next.RootID != id {
			t.Errorf("%s second attempt = %+v, want attempt 2 with parent %s", id, next, id)
		}
	}
}

// TestQueuedRecordReportsQueued: a replacement waiting for a slot reports
// StatusQueued, not StatusRunning, so a client that fetches state is not told
// work is running when it is not.
func TestQueuedRecordReportsQueued(t *testing.T) {
	store := newMemStore()
	ids := []string{"q-0", "q-1"}
	for _, id := range ids {
		rec := Record{ID: id, Kind: "index", Status: StatusRunning, Attempt: 1, RootID: id}
		if err := store.Create(context.Background(), rec); err != nil {
			t.Fatalf("seed Create %s: %v", id, err)
		}
	}
	kinds := map[string]Kind{
		"index": {
			Limit:       1,
			Idempotent:  true,
			MaxAttempts: 2,
			Resume: func(Record) (Func, error) {
				return func(ctx context.Context, _ func(Progress)) ([]byte, error) {
					<-ctx.Done()
					return nil, ctx.Err()
				}, nil
			},
		},
	}
	b := stream.New(stream.Config{Heartbeat: time.Hour})
	r, err := Open(context.Background(), Config{Broker: b, Store: store, Kinds: kinds})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	next := func(id string) Record {
		t.Helper()
		attempts, err := r.Attempts(context.Background(), id)
		if err != nil {
			t.Fatalf("Attempts %s: %v", id, err)
		}
		if len(attempts) != 2 {
			t.Fatalf("%s attempts = %+v, want the interrupted original and its replacement", id, attempts)
		}
		return attempts[1]
	}
	started := next("q-0").ID
	if res := waitStatus(t, r, started, StatusRunning); res.Status != StatusRunning {
		t.Errorf("started replacement = %+v, want StatusRunning", res)
	}

	queued := next("q-1").ID
	res, err := r.Result(context.Background(), queued)
	if err != nil {
		t.Fatalf("Result %s: %v", queued, err)
	}
	if res.Status != StatusQueued {
		t.Errorf("queued replacement status = %q, want %q", res.Status, StatusQueued)
	}
}

// TestQueuedRecordRequeueGuards covers how recovery treats a durable queued
// record. A kind that cannot resume, one past its cap and one whose resume
// work fails leave the record alone, and an eligible one runs it in place.
func TestQueuedRecordRequeueGuards(t *testing.T) {
	resume := func(_ context.Context, _ func(Progress)) ([]byte, error) { return []byte("ok"), nil }
	cases := map[string]struct {
		kind Kind
		rec  Record
		want Status
	}{
		"not-idempotent": {
			kind: Kind{MaxAttempts: 3, Resume: func(Record) (Func, error) { return resume, nil }},
			rec:  Record{ID: "queued", Kind: "render", Status: StatusQueued, Attempt: 1, RootID: "queued"},
			want: StatusQueued,
		},
		"no-resume-work": {
			kind: Kind{Idempotent: true, MaxAttempts: 3},
			rec:  Record{ID: "queued", Kind: "render", Status: StatusQueued, Attempt: 1, RootID: "queued"},
			want: StatusQueued,
		},
		"attempts-spent": {
			kind: Kind{Idempotent: true, MaxAttempts: 1, Resume: func(Record) (Func, error) { return resume, nil }},
			rec:  Record{ID: "queued", Kind: "render", Status: StatusQueued, Attempt: 2, ParentID: "root", RootID: "root"},
			want: StatusQueued,
		},
		"resume-error": {
			kind: Kind{Idempotent: true, MaxAttempts: 2, Resume: func(Record) (Func, error) { return nil, ErrLimit }},
			rec:  Record{ID: "queued", Kind: "render", Status: StatusQueued, Attempt: 1, RootID: "queued"},
			want: StatusQueued,
		},
		"eligible": {
			kind: Kind{Idempotent: true, MaxAttempts: 2, Resume: func(Record) (Func, error) { return resume, nil }},
			rec:  Record{ID: "queued", Kind: "render", Status: StatusQueued, Attempt: 1, RootID: "queued"},
			want: StatusDone,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := newMemStore()
			if err := store.Create(context.Background(), tc.rec); err != nil {
				t.Fatalf("seed Create: %v", err)
			}
			b := stream.New(stream.Config{Heartbeat: time.Hour})
			r, err := Open(context.Background(), Config{Broker: b, Store: store, Kinds: map[string]Kind{"render": tc.kind}})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if res := waitStatus(t, r, tc.rec.ID, tc.want); res.Status != tc.want {
				t.Errorf("status = %q, want %q", res.Status, tc.want)
			}
			attempts, err := r.Attempts(context.Background(), tc.rec.ID)
			if err != nil {
				t.Fatalf("Attempts: %v", err)
			}
			if len(attempts) != 1 {
				t.Errorf("attempts = %+v, want the one record: a queued record is reused, not replaced", attempts)
			}
		})
	}
}

// TestPastCapQueuedRecordIsNotStarted pins the requeue attempt gate. A durable
// queued record whose attempt number is past its kind's cap is refused, so it
// stays queued and never runs. An eligible queued record of the same kind is
// scheduled after it in the one drain queue. Waiting for that record to finish
// proves the drain walked past the refused one.
func TestPastCapQueuedRecordIsNotStarted(t *testing.T) {
	store := newMemStore()
	work := func(_ context.Context, _ func(Progress)) ([]byte, error) { return []byte("ok"), nil }
	resume := func(Record) (Func, error) { return work, nil }
	past := Record{ID: "past-cap", Kind: "render", Status: StatusQueued, Attempt: 2, ParentID: "root", RootID: "root"}
	eligible := Record{ID: "eligible", Kind: "render", Status: StatusQueued, Attempt: 1, RootID: "root"}
	for _, rec := range []Record{past, eligible} {
		if err := store.Create(context.Background(), rec); err != nil {
			t.Fatalf("seed Create %s: %v", rec.ID, err)
		}
	}
	b := stream.New(stream.Config{Heartbeat: time.Hour})
	kinds := map[string]Kind{"render": {Limit: 1, Idempotent: true, MaxAttempts: 1, Resume: resume}}
	r, err := Open(context.Background(), Config{Broker: b, Store: store, Kinds: kinds})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res := waitStatus(t, r, eligible.ID, StatusDone); res.Status != StatusDone {
		t.Fatalf("eligible queued record = %q, want %q: the drain never walked the queue", res.Status, StatusDone)
	}
	res, err := r.Result(context.Background(), past.ID)
	if err != nil {
		t.Fatalf("Result %s: %v", past.ID, err)
	}
	if res.Status != StatusQueued {
		t.Errorf("past-cap queued record = %q, want %q: the requeue gate let it run", res.Status, StatusQueued)
	}
}

// waitForAttempts polls a logical job's lineage until it holds want attempts
// and the last one is terminal, so a caller can read the replacement attempt.
func waitForAttempts(t *testing.T, r *Runner, id string, want int) []Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var attempts []Record
	for {
		var err error
		attempts, err = r.Attempts(context.Background(), id)
		if err != nil {
			t.Fatalf("Attempts %s: %v", id, err)
		}
		if len(attempts) == want && attempts[want-1].Status != StatusRunning && attempts[want-1].Status != StatusQueued {
			return attempts
		}
		if time.Now().After(deadline) {
			t.Fatalf("attempts for %s = %+v, want %d with the last terminal", id, attempts, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
