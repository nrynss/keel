package job_test

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nrynss/keel/job"
	"github.com/nrynss/keel/job/sqlitestore"
	"github.com/nrynss/keel/sqlite"
	"github.com/nrynss/keel/stream"
)

// openDurableStores opens two independent connections over one database file.
// The runner writes through the first and the test reads through the second,
// so a pin never trusts the runner's own connection.
func openDurableStores(t *testing.T) (writer, reader *sqlitestore.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.db")
	ctx := context.Background()
	writerDB, err := sqlite.Open(ctx, sqlite.Config{Path: path})
	if err != nil {
		t.Fatalf("writer sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = writerDB.Close() }) // cleanup: the temporary file goes with the test directory
	writer, err = sqlitestore.Open(ctx, sqlitestore.Config{DB: writerDB})
	if err != nil {
		t.Fatalf("writer sqlitestore.Open: %v", err)
	}
	readerDB, err := sqlite.Open(ctx, sqlite.Config{Path: path})
	if err != nil {
		t.Fatalf("reader sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = readerDB.Close() }) // cleanup: the temporary file goes with the test directory
	reader, err = sqlitestore.Open(ctx, sqlitestore.Config{DB: readerDB})
	if err != nil {
		t.Fatalf("reader sqlitestore.Open: %v", err)
	}
	return writer, reader
}

// TestCancelledTerminalIsDurable pins the durable cancelled state. A job
// cancelled through Cancel records its cancelled terminal and its last
// progress snapshot, written through a context no caller cancellation can
// abort. The pin reads the state back over a second connection the runner
// never touches, then restarts over the same file. The restart must publish
// no second terminal on the id and run no idempotent resume work.
func TestCancelledTerminalIsDurable(t *testing.T) {
	ctx := context.Background()
	writer, reader := openDurableStores(t)

	var resumes atomic.Int32
	kinds := map[string]job.Kind{
		"encode": {
			Limit:       1,
			Idempotent:  true,
			MaxAttempts: 2,
			Resume: func(job.Record) (job.Func, error) {
				resumes.Add(1)
				return func(context.Context, func(job.Progress)) ([]byte, error) {
					return nil, nil
				}, nil
			},
		},
	}

	broker := stream.New(stream.Config{Heartbeat: time.Hour})
	runner, err := job.Open(ctx, job.Config{Broker: broker, Store: writer, Kinds: kinds})
	if err != nil {
		t.Fatalf("job.Open: %v", err)
	}

	started := make(chan struct{})
	id, err := runner.StartKind(ctx, "encode", func(jobCtx context.Context, progress func(job.Progress)) ([]byte, error) {
		close(started)
		<-jobCtx.Done()
		// The snapshot lands after Cancel, so it exercises the same
		// detached write the terminal uses.
		progress(job.Progress{Stage: "after-cancel"})
		return nil, jobCtx.Err()
	})
	if err != nil {
		t.Fatalf("StartKind: %v", err)
	}
	<-started
	if err := runner.Cancel(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	stored := waitForDurableStatus(t, reader, id, job.StatusCancelled)
	if stored.Progress.Stage != "after-cancel" {
		t.Errorf("stored progress = %+v, want the post-cancel snapshot", stored.Progress)
	}
	if n := resumes.Load(); n != 0 {
		t.Errorf("resume work ran before the restart: count %d, want 0", n)
	}

	// A restart over the same file, with a fresh broker. The cancelled job is
	// finished, so recovery must neither interrupt it nor resume it.
	fresh := stream.New(stream.Config{Heartbeat: time.Hour})
	sub := fresh.Subscribe(ctx, job.Topic(id))
	defer sub.Cancel()
	if _, err := job.Open(ctx, job.Config{Broker: fresh, Store: writer, Kinds: kinds}); err != nil {
		t.Fatalf("restart job.Open: %v", err)
	}
	select {
	case ev, ok := <-sub.Events:
		t.Errorf("terminal after restart = %+v (open=%v), want none on the finished id", ev, ok)
	case <-time.After(300 * time.Millisecond):
	}
	if n := resumes.Load(); n != 0 {
		t.Errorf("resume work ran after the restart: count %d, want 0", n)
	}
}

// TestRecordSurvivesCancelledRequest pins the documented promise that a job
// outlives the request it came from: a request cancelled before the record is
// written must still leave a readable job that runs to a terminal.
func TestRecordSurvivesCancelledRequest(t *testing.T) {
	ctx := context.Background()
	writer, reader := openDurableStores(t)

	broker := stream.New(stream.Config{Heartbeat: time.Hour})
	runner, err := job.Open(ctx, job.Config{Broker: broker, Store: writer})
	if err != nil {
		t.Fatalf("job.Open: %v", err)
	}

	request, cancel := context.WithCancel(ctx)
	cancel()
	id, err := runner.Start(request, func(context.Context, func(job.Progress)) ([]byte, error) {
		return []byte("done"), nil
	})
	if err != nil {
		t.Fatalf("Start under a cancelled request: %v", err)
	}
	waitForDurableStatus(t, reader, id, job.StatusDone)
}

// waitForDurableStatus reads id back through the given store until it reaches
// want, failing the test if it reaches a different terminal state first.
func waitForDurableStatus(t *testing.T, store *sqlitestore.Store, id string, want job.Status) job.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var rec job.Record
	for {
		var err error
		rec, err = store.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if rec.Status == want {
			return rec
		}
		if rec.Status != job.StatusRunning {
			t.Fatalf("durable status = %q, want %q", rec.Status, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("durable status for %s still %q, want %q", id, rec.Status, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
