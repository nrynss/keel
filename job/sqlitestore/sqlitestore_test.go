package sqlitestore_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nrynss/keel/job"
	"github.com/nrynss/keel/job/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// openDB opens a database file under the test's temporary directory.
func openDB(t *testing.T, path string) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: path})
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})
	return db
}

// openStore opens a store over a fresh database file and returns both, so a
// test can reopen the file independently.
func openStore(t *testing.T) (*sqlitestore.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.db")
	store, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: openDB(t, path)})
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	return store, path
}

// running is a record in the running state, the shape Create stores.
func running(id, kind string) job.Record {
	return job.Record{
		ID:        id,
		Kind:      kind,
		Status:    job.StatusRunning,
		Attempt:   1,
		RootID:    id,
		UpdatedAt: time.Unix(1700000000, 0),
	}
}

// TestOpenRejectsNilDB: a store with no database refuses to open rather than
// panicking on the first write.
func TestOpenRejectsNilDB(t *testing.T) {
	if _, err := sqlitestore.Open(t.Context(), sqlitestore.Config{}); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Errorf("err = %v, want errors.Is(.., ErrInvalid)", err)
	}
}

// TestOpenIsIdempotent: opening the same database twice applies the schema
// once, and data written through the first store is readable through the
// second.
func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	first, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: openDB(t, path)})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.Create(t.Context(), running("job-1", "index")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	second, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: openDB(t, path)})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	got, err := second.Get(t.Context(), "job-1")
	if err != nil {
		t.Fatalf("Get through the second store: %v", err)
	}
	if got.ID != "job-1" || got.Kind != "index" || got.Status != job.StatusRunning {
		t.Errorf("Get = %+v, want the row the first store wrote", got)
	}
}

// TestCreateGetRoundTrip: every stored field comes back, including the
// progress snapshot's optional counters and its raw detail.
func TestCreateGetRoundTrip(t *testing.T) {
	store, _ := openStore(t)
	rec := running("job-1", "render")
	rec.ParentID = "job-0"
	rec.RootID = "job-0"
	rec.Attempt = 2
	rec.Progress = job.Progress{
		Stage:   "encode",
		Current: int64p(3),
		Total:   int64p(9),
		Detail:  json.RawMessage(`{"shard":2}`),
	}
	rec.UpdatedAt = time.Unix(1700000123, 456)
	if err := store.Create(t.Context(), rec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Get(t.Context(), "job-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != rec.ID || got.Kind != rec.Kind || got.Status != rec.Status {
		t.Errorf("Get identity = %+v, want %+v", got, rec)
	}
	if got.Attempt != 2 || got.ParentID != "job-0" || got.RootID != "job-0" {
		t.Errorf("Get lineage = attempt %d parent %q root %q, want attempt 2 parent job-0", got.Attempt, got.ParentID, got.RootID)
	}
	if got.Progress.Stage != "encode" || got.Progress.Current == nil || *got.Progress.Current != 3 {
		t.Errorf("Get progress = %+v, want stage encode current 3", got.Progress)
	}
	if got.Progress.Total == nil || *got.Progress.Total != 9 || string(got.Progress.Detail) != `{"shard":2}` {
		t.Errorf("Get progress = %+v, want total 9 and the raw detail", got.Progress)
	}
	if !got.UpdatedAt.Equal(rec.UpdatedAt) {
		t.Errorf("Get UpdatedAt = %v, want %v", got.UpdatedAt, rec.UpdatedAt)
	}
}

// TestGetUnknownJob: an id the store never held reports ErrUnknownJob.
func TestGetUnknownJob(t *testing.T) {
	store, _ := openStore(t)
	if _, err := store.Get(t.Context(), "no-such-job"); !errors.Is(err, job.ErrUnknownJob) {
		t.Errorf("err = %v, want errors.Is(.., job.ErrUnknownJob)", err)
	}
}

// TestSetProgress: the latest snapshot replaces the earlier one, and an
// unknown id reports ErrUnknownJob.
func TestSetProgress(t *testing.T) {
	store, _ := openStore(t)
	if err := store.Create(t.Context(), running("job-1", "render")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	snap := job.Record{
		ID:        "job-1",
		Progress:  job.Progress{Stage: "late", Detail: json.RawMessage(`{"shard":7}`)},
		UpdatedAt: time.Unix(1700000900, 0),
	}
	if err := store.SetProgress(t.Context(), snap); err != nil {
		t.Fatalf("SetProgress: %v", err)
	}
	got, err := store.Get(t.Context(), "job-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Progress.Stage != "late" || string(got.Progress.Detail) != `{"shard":7}` {
		t.Errorf("Progress = %+v, want the latest snapshot", got.Progress)
	}
	if got.Status != job.StatusRunning || got.Kind != "render" {
		t.Errorf("SetProgress changed identity: %+v", got)
	}

	snap.ID = "no-such-job"
	if err := store.SetProgress(t.Context(), snap); !errors.Is(err, job.ErrUnknownJob) {
		t.Errorf("SetProgress on an unknown id = %v, want ErrUnknownJob", err)
	}
}

// TestFinishKeepsKindAndLineage: the terminal write replaces the status, the
// result and the error, and leaves the kind and the lineage Create stored.
func TestFinishKeepsKindAndLineage(t *testing.T) {
	store, _ := openStore(t)
	rec := running("job-2", "render")
	rec.Attempt = 3
	rec.ParentID = "job-1"
	rec.RootID = "job-0"
	if err := store.Create(t.Context(), rec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	terminal := job.Record{
		ID:        "job-2",
		Status:    job.StatusDone,
		Data:      []byte(`{"book":1}`),
		UpdatedAt: time.Unix(1700001000, 0),
	}
	if err := store.Finish(t.Context(), terminal); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got, err := store.Get(t.Context(), "job-2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != job.StatusDone || string(got.Data) != `{"book":1}` || got.Err != nil {
		t.Errorf("Get = %+v, want done with the stored bytes", got)
	}
	if got.Kind != "render" || got.Attempt != 3 || got.ParentID != "job-1" || got.RootID != "job-0" {
		t.Errorf("Finish lost Kind or lineage: %+v", got)
	}
	if err := store.Finish(t.Context(), job.Record{ID: "no-such-job"}); !errors.Is(err, job.ErrUnknownJob) {
		t.Errorf("Finish on an unknown id = %v, want ErrUnknownJob", err)
	}
}

// TestTerminalErrorTextRoundTrip: a durable store keeps an error's message,
// not its chain, so a reader classifies on status and reads the text.
func TestTerminalErrorTextRoundTrip(t *testing.T) {
	store, _ := openStore(t)
	if err := store.Create(t.Context(), running("job-3", "render")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sentinel := errors.New("media: transient")
	terminal := job.Record{ID: "job-3", Status: job.StatusError, Err: sentinel, UpdatedAt: time.Unix(1700001100, 0)}
	if err := store.Finish(t.Context(), terminal); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got, err := store.Get(t.Context(), "job-3")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != job.StatusError || got.Err == nil || got.Err.Error() != sentinel.Error() {
		t.Errorf("Err = %v, want the stored message %q", got.Err, sentinel.Error())
	}
	if got.Data != nil {
		t.Errorf("Data = %q alongside an error, want nil", got.Data)
	}
}

// TestUnfinishedOldestFirst: the running and queued rows come back, ordered
// by their last change, and a terminal row stays out.
func TestUnfinishedOldestFirst(t *testing.T) {
	store, _ := openStore(t)
	newer := running("job-newer", "render")
	newer.UpdatedAt = time.Unix(1700000200, 0)
	queued := running("job-queued", "render")
	queued.Status = job.StatusQueued
	queued.UpdatedAt = time.Unix(1700000150, 0)
	older := running("job-older", "render")
	older.UpdatedAt = time.Unix(1700000100, 0)
	done := running("job-done", "render")
	done.Status = job.StatusDone
	done.UpdatedAt = time.Unix(1700000090, 0)
	for _, rec := range []job.Record{newer, queued, older, done} {
		if err := store.Create(t.Context(), rec); err != nil {
			t.Fatalf("Create %s: %v", rec.ID, err)
		}
	}

	got, err := store.Unfinished(t.Context())
	if err != nil {
		t.Fatalf("Unfinished: %v", err)
	}
	if len(got) != 3 || got[0].ID != "job-older" || got[1].ID != "job-queued" || got[2].ID != "job-newer" {
		t.Errorf("Unfinished = %+v, want the running and queued rows oldest first", got)
	}
}

// TestBeginMarksRunning: Begin moves a queued row to running and updates its
// timestamp, and an unknown id reports ErrUnknownJob.
func TestBeginMarksRunning(t *testing.T) {
	store, _ := openStore(t)
	rec := running("job-queued", "index")
	rec.Status = job.StatusQueued
	if err := store.Create(t.Context(), rec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec.Status = job.StatusRunning
	rec.UpdatedAt = time.Unix(1700000300, 0)
	if err := store.Begin(t.Context(), rec); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	got, err := store.Get(t.Context(), rec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != job.StatusRunning || !got.UpdatedAt.Equal(rec.UpdatedAt) {
		t.Errorf("Get = %+v, want running with the Begin timestamp %v", got, rec.UpdatedAt)
	}
	if err := store.Begin(t.Context(), job.Record{ID: "no-such-job"}); !errors.Is(err, job.ErrUnknownJob) {
		t.Errorf("Begin on an unknown id = %v, want ErrUnknownJob", err)
	}
}

// TestAttemptsLineage: the whole chain of a logical job comes back oldest
// first, reachable from any attempt in it.
func TestAttemptsLineage(t *testing.T) {
	store, _ := openStore(t)
	first := running("job-a1", "index")
	first.UpdatedAt = time.Unix(1700000000, 0)
	second := running("job-a2", "index")
	second.Attempt = 2
	second.ParentID = "job-a1"
	second.RootID = "job-a1"
	second.UpdatedAt = time.Unix(1700000100, 0)
	other := running("job-b1", "index")
	other.UpdatedAt = time.Unix(1700000050, 0)
	for _, rec := range []job.Record{second, first, other} {
		if err := store.Create(t.Context(), rec); err != nil {
			t.Fatalf("Create %s: %v", rec.ID, err)
		}
	}

	got, err := store.Attempts(t.Context(), "job-a2")
	if err != nil {
		t.Fatalf("Attempts: %v", err)
	}
	if len(got) != 2 || got[0].ID != "job-a1" || got[1].ID != "job-a2" {
		t.Fatalf("Attempts = %+v, want the two attempts oldest first", got)
	}
	if got[1].ParentID != "job-a1" || got[1].RootID != "job-a1" {
		t.Errorf("second attempt lineage = %+v, want parent and root job-a1", got[1])
	}
	if _, err := store.Attempts(t.Context(), "no-such-job"); !errors.Is(err, job.ErrUnknownJob) {
		t.Errorf("Attempts on an unknown id = %v, want ErrUnknownJob", err)
	}
}

// TestStateSurvivesReopen: a fresh connection to the same file reads the
// state a previous store wrote, which is what a restart depends on.
func TestStateSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	writer, err := sqlitestore.Open(context.Background(), sqlitestore.Config{DB: openDB(t, path)})
	if err != nil {
		t.Fatalf("Open writer: %v", err)
	}
	rec := running("job-restart", "index")
	if err := writer.Create(context.Background(), rec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	terminal := job.Record{ID: rec.ID, Status: job.StatusInterrupted, Err: job.ErrInterrupted, UpdatedAt: time.Unix(1700002000, 0)}
	if err := writer.Finish(context.Background(), terminal); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	reader, err := sqlitestore.Open(context.Background(), sqlitestore.Config{DB: openDB(t, path)})
	if err != nil {
		t.Fatalf("Open reader: %v", err)
	}
	got, err := reader.Get(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != job.StatusInterrupted || got.Err == nil || got.Err.Error() != job.ErrInterrupted.Error() {
		t.Errorf("Get = %+v, want the interrupted state a fresh connection reads", got)
	}
	if unfinished, err := reader.Unfinished(context.Background()); err != nil || len(unfinished) != 0 {
		t.Errorf("Unfinished = %+v, %v, want none after the interrupted terminal", unfinished, err)
	}
}

// int64p returns a pointer to v, for the optional numeric progress members.
func int64p(v int64) *int64 { return &v }

// chain builds a two-attempt chain under the store: attempt one finished at
// firstAt, attempt two finished at secondAt.
func chain(t *testing.T, store *sqlitestore.Store, kind string, firstAt, secondAt time.Time) {
	t.Helper()
	first := running("chain-a1", kind)
	first.UpdatedAt = firstAt
	if err := store.Create(t.Context(), first); err != nil {
		t.Fatalf("Create %s: %v", first.ID, err)
	}
	second := running("chain-a2", kind)
	second.Attempt = 2
	second.ParentID = "chain-a1"
	second.RootID = "chain-a1"
	second.UpdatedAt = secondAt
	if err := store.Create(t.Context(), second); err != nil {
		t.Fatalf("Create %s: %v", second.ID, err)
	}
	for _, rec := range []job.Record{
		{ID: first.ID, Status: job.StatusDone, UpdatedAt: firstAt},
		{ID: second.ID, Status: job.StatusDone, UpdatedAt: secondAt},
	} {
		if err := store.Finish(t.Context(), rec); err != nil {
			t.Fatalf("Finish %s: %v", rec.ID, err)
		}
	}
}

// gone reports that a fresh connection to the same file finds no row for id
// and no attempt of the chain id rooted, which is what erasure depends on.
func gone(t *testing.T, path string, root string, ids ...string) {
	t.Helper()
	reader, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: openDB(t, path)})
	if err != nil {
		t.Fatalf("Open reader: %v", err)
	}
	for _, id := range ids {
		if _, err := reader.Get(t.Context(), id); !errors.Is(err, job.ErrUnknownJob) {
			t.Errorf("Get %s through a fresh connection = %v, want ErrUnknownJob", id, err)
		}
	}
	if _, err := reader.Attempts(t.Context(), root); !errors.Is(err, job.ErrUnknownJob) {
		t.Errorf("Attempts %s through a fresh connection = %v, want ErrUnknownJob", root, err)
	}
}

// TestDeleteRemovesAttemptChain: deleting one finished record removes every
// row of its attempt chain, and a fresh connection finds none of them.
func TestDeleteRemovesAttemptChain(t *testing.T) {
	store, path := openStore(t)
	chain(t, store, "index", time.Unix(1700000000, 0), time.Unix(1700000100, 0))
	other := running("job-other", "index")
	other.UpdatedAt = time.Unix(1700000050, 0)
	if err := store.Create(t.Context(), other); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.Delete(t.Context(), "chain-a2"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	gone(t, path, "chain-a1", "chain-a1", "chain-a2")
	if _, err := store.Get(t.Context(), "job-other"); err != nil {
		t.Errorf("Get of an untouched job after Delete = %v, want nil", err)
	}
}

// TestDeleteRefusesActiveChain: a chain that still holds a running row is
// refused with ErrChainActive, and every row of it survives.
func TestDeleteRefusesActiveChain(t *testing.T) {
	store, _ := openStore(t)
	first := running("chain-a1", "index")
	first.UpdatedAt = time.Unix(1700000000, 0)
	second := running("chain-a2", "index")
	second.Attempt = 2
	second.ParentID = "chain-a1"
	second.RootID = "chain-a1"
	second.UpdatedAt = time.Unix(1700000100, 0)
	if err := store.Create(t.Context(), first); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Create(t.Context(), second); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.Delete(t.Context(), "chain-a1"); !errors.Is(err, sqlitestore.ErrChainActive) {
		t.Fatalf("Delete over a running chain = %v, want ErrChainActive", err)
	}
	for _, id := range []string{"chain-a1", "chain-a2"} {
		if _, err := store.Get(t.Context(), id); err != nil {
			t.Errorf("Get %s after the refusal = %v, want the row kept", id, err)
		}
	}
}

// TestDeleteUnknownJobIsRepeatable: an absent id answers ErrUnknownJob, and
// deleting an already deleted chain answers it again.
func TestDeleteUnknownJobIsRepeatable(t *testing.T) {
	store, path := openStore(t)
	chain(t, store, "index", time.Unix(1700000000, 0), time.Unix(1700000100, 0))
	if err := store.Delete(t.Context(), "no-such-job"); !errors.Is(err, job.ErrUnknownJob) {
		t.Fatalf("Delete of an unknown id = %v, want ErrUnknownJob", err)
	}
	if err := store.Delete(t.Context(), "chain-a1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := store.Delete(t.Context(), "chain-a2"); !errors.Is(err, job.ErrUnknownJob) {
			t.Fatalf("repeat Delete %d = %v, want ErrUnknownJob", i+1, err)
		}
	}
	gone(t, path, "chain-a1", "chain-a1", "chain-a2")
}

// TestPruneFinishedRemovesAgedTerminalChains: exactly the terminal chains
// whose newest update is before the cutoff go, and a fresh terminal chain and
// a running chain stay.
func TestPruneFinishedRemovesAgedTerminalChains(t *testing.T) {
	store, path := openStore(t)
	aged := time.Unix(1700000000, 0)
	fresh := time.Unix(1700009000, 0)
	cutoff := time.Unix(1700005000, 0)
	chain(t, store, "index", aged.Add(-time.Minute), aged)

	freshFirst := running("fresh-a1", "index")
	freshFirst.UpdatedAt = fresh
	if err := store.Create(t.Context(), freshFirst); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Finish(t.Context(), job.Record{ID: "fresh-a1", Status: job.StatusDone, UpdatedAt: fresh}); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	active := running("job-active", "index")
	active.UpdatedAt = aged
	if err := store.Create(t.Context(), active); err != nil {
		t.Fatalf("Create: %v", err)
	}

	n, err := store.PruneFinished(t.Context(), cutoff)
	if err != nil {
		t.Fatalf("PruneFinished: %v", err)
	}
	if n != 1 {
		t.Fatalf("PruneFinished = %d, want 1 chain removed", n)
	}
	gone(t, path, "chain-a1", "chain-a1", "chain-a2")
	for _, id := range []string{"fresh-a1", "job-active"} {
		if _, err := store.Get(t.Context(), id); err != nil {
			t.Errorf("Get %s after prune = %v, want the row kept", id, err)
		}
	}

	if n, err := store.PruneFinished(t.Context(), cutoff); err != nil || n != 0 {
		t.Errorf("second PruneFinished = %d, %v, want 0, nil", n, err)
	}
}
