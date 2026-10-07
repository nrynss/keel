package providertask_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/job"
	"github.com/nrynss/keel/providertask"
	"github.com/nrynss/keel/stream"
	"github.com/nrynss/keel/wire"
)

// jobStore is the in-memory job Store the progress wiring test runs
// against. It keeps the least the Runner needs, because the run never
// restarts inside the test.
type jobStore struct {
	mu      sync.Mutex
	records map[string]job.Record
}

func newJobStore() *jobStore {
	return &jobStore{records: make(map[string]job.Record)}
}

func (m *jobStore) Create(_ context.Context, rec job.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records[rec.ID] = rec
	return nil
}

func (m *jobStore) Begin(_ context.Context, rec job.Record) error {
	return m.patch(rec.ID, func(r job.Record) job.Record {
		r.Status = rec.Status
		r.UpdatedAt = rec.UpdatedAt
		return r
	})
}

func (m *jobStore) SetProgress(_ context.Context, rec job.Record) error {
	return m.patch(rec.ID, func(r job.Record) job.Record {
		r.Progress = rec.Progress
		r.UpdatedAt = rec.UpdatedAt
		return r
	})
}

func (m *jobStore) Finish(_ context.Context, rec job.Record) error {
	return m.patch(rec.ID, func(r job.Record) job.Record {
		r.Status = rec.Status
		r.Data = rec.Data
		r.Err = rec.Err
		r.UpdatedAt = rec.UpdatedAt
		return r
	})
}

func (m *jobStore) Get(_ context.Context, id string) (job.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[id]
	if !ok {
		return job.Record{}, job.ErrUnknownJob
	}
	return rec, nil
}

func (m *jobStore) Unfinished(_ context.Context) ([]job.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []job.Record
	for _, rec := range m.records {
		if rec.Status == job.StatusRunning || rec.Status == job.StatusQueued {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (m *jobStore) Attempts(_ context.Context, id string) ([]job.Record, error) {
	rec, err := m.Get(context.Background(), id)
	if err != nil {
		return nil, err
	}
	return []job.Record{rec}, nil
}

// patch applies fn to one stored record.
func (m *jobStore) patch(id string, fn func(job.Record) job.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[id]
	if !ok {
		return job.ErrUnknownJob
	}
	m.records[id] = fn(rec)
	return nil
}

// collectEvents reads a subscription until a terminal frame or the bound.
func collectEvents(t *testing.T, sub *stream.Subscription) []stream.Event {
	t.Helper()
	var got []stream.Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-sub.Events:
			got = append(got, ev)
			if ev.Terminal {
				return got
			}
		case <-deadline:
			t.Fatalf("no terminal frame, got %d events", len(got))
		}
	}
}

// TestProgressReachesTheJobTopic: a Run inside a job publishes the created
// stage and the provider progress onto the job's topic, and the job lands
// its done terminal, so the browser sees the whole story over the stream.
func TestProgressReachesTheJobTopic(t *testing.T) {
	h := newHarness(t)
	broker := stream.New(stream.Config{})
	runner, err := job.Open(t.Context(), job.Config{
		Broker: broker,
		Store:  newJobStore(),
		Log:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("job.Open: %v", err)
	}

	p := newFakeProvider(rendered{Link: "s3://injob"},
		step{st: providertask.Status{
			State:    providertask.StateRunning,
			Progress: progressOf("render", 3, 10),
		}},
		step{st: providertask.Status{State: providertask.StateSucceeded}},
	)

	// The job waits for the subscription, so the test observes every
	// progress frame from the first one.
	proceed := make(chan struct{})
	cfg := h.config()
	jobID, err := runner.Start(t.Context(), func(ctx context.Context, progress func(job.Progress)) ([]byte, error) {
		cfg.Progress = progress
		<-proceed
		outcome, err := providertask.Run(ctx, cfg, h.store, "injob", p.spec(nil))
		if err != nil {
			return nil, err
		}
		return []byte(outcome.Value.Link), nil
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sub := broker.Subscribe(t.Context(), job.Topic(jobID))
	defer sub.Cancel()
	close(proceed)

	events := collectEvents(t, sub)

	var stages []string
	terminal := stream.Event{}
	for _, ev := range events {
		if ev.Name != "progress" {
			terminal = ev
			continue
		}
		frame, ok := ev.Data.(wire.ProgressEvent)
		if !ok {
			t.Fatalf("progress frame = %#v, want a wire progress payload", ev.Data)
		}
		stages = append(stages, frame.Stage)
	}
	if len(stages) != 2 || stages[0] != providertask.StageCreated || stages[1] != "render" {
		t.Errorf("stages = %v, want created then render", stages)
	}
	if terminal.Name != string(job.StatusDone) {
		t.Errorf("terminal = %s, want done", terminal.Name)
	}
	result, err := runner.Result(t.Context(), jobID)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if result.Status != job.StatusDone || string(result.Data) != "s3://injob" {
		t.Errorf("result = %s %q, want done with the kept bytes", result.Status, result.Data)
	}
}
