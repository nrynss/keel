package job_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nrynss/keel/job"
	"github.com/nrynss/keel/job/sqlitestore"
	"github.com/nrynss/keel/sqlite"
	"github.com/nrynss/keel/stream"
)

// The environment the resume-snapshot harness uses. The child writes an
// open marker once recovery has finished, so the parent kills it only after
// every replacement row is durable.
const (
	resumeChildEnv  = "KEEL_JOB_RESUME_CHILD"
	resumeHelperRun = "TestResumeSnapshotHelperProcess"
)

// resumeLedger is the snapshot payload the harness seeds and the resume
// hook decodes. The ref names the logical work, so a hook that finds no
// snapshot has nothing to rebuild and must refuse.
type resumeLedger struct {
	Ref       string   `json:"ref"`
	Confirmed []string `json:"confirmed"`
}

// decodeResumeLedger reads a snapshot payload. An absent or unreadable
// snapshot is an error, because a hook cannot name the work without it.
func decodeResumeLedger(detail json.RawMessage) (resumeLedger, error) {
	var ledger resumeLedger
	if len(detail) == 0 {
		return ledger, errors.New("resume harness: no snapshot")
	}
	if err := json.Unmarshal(detail, &ledger); err != nil {
		return ledger, fmt.Errorf("resume harness: decode snapshot: %w", err)
	}
	return ledger, nil
}

// resumeRunPath names the log an attempt writes when it starts.
func resumeRunPath(dir string) string { return filepath.Join(dir, "resume-starts.log") }

// resumeDonePath names the log a finished attempt writes with the ref its
// hook rebuilt from.
func resumeDonePath(dir string) string { return filepath.Join(dir, "resume-dones.log") }

// resumeOpenPath names the log the child writes once recovery has finished.
func resumeOpenPath(dir string) string { return filepath.Join(dir, "resume-open.log") }

// resumeKinds is the kind table for the resume-snapshot harness. The resume
// hook rebuilds only from a snapshot that names a ref, which is the
// contract every consumer hook faces. A child attempt announces itself and
// blocks until the process dies. A parent attempt finishes and records the
// ref it rebuilt from.
func resumeKinds(t *testing.T, dir string) map[string]job.Kind {
	t.Helper()
	return map[string]job.Kind{
		"resume": {
			Limit:       1,
			Idempotent:  true,
			MaxAttempts: 3,
			Resume: func(rec job.Record) (job.Func, error) {
				ledger, err := decodeResumeLedger(rec.Progress.Detail)
				if err != nil || ledger.Ref == "" {
					return nil, fmt.Errorf("resume hook: no ref for job %s", rec.ID)
				}
				return func(ctx context.Context, progress func(job.Progress)) ([]byte, error) {
					if os.Getenv(resumeChildEnv) != "" {
						appendLine(t, resumeRunPath(dir), "start "+ledger.Ref)
						<-ctx.Done() // parked until the parent kills the child
						return nil, ctx.Err()
					}
					appendLine(t, resumeDonePath(dir), "done "+ledger.Ref)
					return []byte("resumed"), nil
				}, nil
			},
		},
	}
}

// seedResumeJob writes one running first attempt carrying its ledger, the
// state a killed process leaves behind. The ref is the id itself, so every
// completion line names the lineage it was rebuilt from.
func seedResumeJob(t *testing.T, ctx context.Context, store *sqlitestore.Store, id string, at time.Time) {
	t.Helper()
	detail, err := json.Marshal(resumeLedger{Ref: id, Confirmed: []string{"alpha", "beta"}})
	if err != nil {
		t.Fatalf("marshal ledger %s: %v", id, err)
	}
	rec := job.Record{
		ID:        id,
		Kind:      "resume",
		Status:    job.StatusRunning,
		Attempt:   1,
		RootID:    id,
		Progress:  job.Progress{Stage: "cut", Detail: detail},
		UpdatedAt: at,
	}
	if err := store.Create(ctx, rec); err != nil {
		t.Fatalf("seed Create %s: %v", id, err)
	}
}

// seedResumeDatabase clears the harness files and writes one running first
// attempt per id. The rising timestamps fix the recovery order, so the
// first id always claims the single slot.
func seedResumeDatabase(t *testing.T, dir string, ids []string) {
	t.Helper()
	for _, path := range []string{
		filepath.Join(dir, "jobs.db"),
		resumeRunPath(dir),
		resumeDonePath(dir),
		resumeOpenPath(dir),
	} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatalf("clear %s: %v", path, err)
		}
	}
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "jobs.db")})
	if err != nil {
		t.Fatalf("seed sqlite.Open: %v", err)
	}
	store, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: db})
	if err != nil {
		t.Fatalf("seed sqlitestore.Open: %v", err)
	}
	for i, id := range ids {
		seedResumeJob(t, ctx, store, id, time.Unix(int64(1_700_000_000+i), 0))
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed database: %v", err)
	}
}

// openResumeStore opens one independent connection over the harness
// database. Reads and recovery run on separate connections, so no pin
// trusts a runner's own view of the state.
func openResumeStore(t *testing.T, dir string) *sqlitestore.Store {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "jobs.db")})
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() }) // cleanup: the file goes with the test directory
	store, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: db})
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	return store
}

// launchResumeChild starts the child half over the seeded database and
// kills it once recovery has finished and one attempt has started. The kill
// therefore lands after every replacement row is durable, with one attempt
// parked before its first report.
func launchResumeChild(t *testing.T, dir string) {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run="+resumeHelperRun)
	child.Env = append(os.Environ(), resumeChildEnv+"=1", childDirEnv+"="+dir)
	var childLog strings.Builder
	child.Stdout, child.Stderr = &childLog, &childLog
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill() // cleanup: the test is over, so a lost kill race changes no result
			_ = child.Wait()         // cleanup: reaped here only to avoid a zombie, its status is unread
		}
	})
	waitForLines(t, resumeOpenPath(dir), 1)
	waitForLines(t, resumeRunPath(dir), 1)
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	if err := child.Wait(); err == nil {
		t.Fatalf("child exited cleanly, want a kill. Output:\n%s", childLog.String())
	}
}

// assertInheritedLedger reads one lineage after the kill and pins the
// replacement's state and snapshot. The ref comes straight from the
// interrupted record, which is what a resume hook rebuilds from.
func assertInheritedLedger(t *testing.T, store *sqlitestore.Store, id string, want job.Status) {
	t.Helper()
	attempts, err := store.Attempts(context.Background(), id)
	if err != nil {
		t.Fatalf("Attempts %s: %v", id, err)
	}
	if len(attempts) != 2 {
		t.Fatalf("%s attempts = %d, want 2: the replacement must be durable before the kill", id, len(attempts))
	}
	if first := attempts[0]; first.Attempt != 1 || first.Status != job.StatusInterrupted || first.RootID != id {
		t.Errorf("%s first attempt = %+v, want interrupted attempt 1 rooted at %s", id, first, id)
	}
	next := attempts[1]
	if next.Attempt != 2 || next.ParentID != id || next.RootID != id {
		t.Errorf("%s replacement = %+v, want attempt 2 with parent and root %s", id, next, id)
	}
	if next.Status != want {
		t.Errorf("%s replacement status = %q, want %q", id, next.Status, want)
	}
	ledger, err := decodeResumeLedger(next.Progress.Detail)
	if err != nil {
		t.Fatalf("%s replacement snapshot = %s, %v, want the ledger inherited at birth", id, next.Progress.Detail, err)
	}
	if ledger.Ref != id {
		t.Errorf("%s replacement ref = %q, want %q inherited from the interrupted record", id, ledger.Ref, id)
	}
}

// TestResumeSnapshotHelperProcess is the child half of the harness. It
// opens a runner over the seeded records, which recovers them and writes
// every replacement row, marks the open complete, then parks. The parent
// kills it there.
func TestResumeSnapshotHelperProcess(t *testing.T) {
	if os.Getenv(resumeChildEnv) == "" {
		t.Skip("child of the resume-snapshot harness")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the harness kills the child with SIGKILL")
	}
	dir := os.Getenv(childDirEnv)
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "jobs.db")})
	if err != nil {
		t.Fatalf("child sqlite.Open: %v", err)
	}
	defer db.Close()
	store, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: db})
	if err != nil {
		t.Fatalf("child sqlitestore.Open: %v", err)
	}
	broker := stream.New(stream.Config{Heartbeat: time.Hour})
	if _, err := job.Open(ctx, job.Config{
		Broker: broker,
		Store:  store,
		Kinds:  resumeKinds(t, dir),
	}); err != nil {
		t.Fatalf("child job.Open: %v", err)
	}
	appendLine(t, resumeOpenPath(dir), "open")
	for {
		time.Sleep(time.Hour) // parked until the parent kills the child
	}
}

// TestKillBeforeFirstReportStillResumes kills the process after the
// replacement attempt started but before it reported anything. The
// replacement must carry the interrupted record's snapshot from birth, so
// the next Open rebuilds the work from it instead of stranding the chain.
func TestKillBeforeFirstReportStillResumes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the harness kills the child with SIGKILL")
	}
	dir := crashDir(t)
	seedResumeDatabase(t, dir, []string{"lead-0"})
	launchResumeChild(t, dir)

	// The killed state, read over a connection no runner ever sees. The
	// replacement is running with the ledger it inherited at birth.
	assertInheritedLedger(t, openResumeStore(t, dir), "lead-0", job.StatusRunning)

	broker := stream.New(stream.Config{Heartbeat: time.Hour})
	runner, err := job.Open(context.Background(), job.Config{
		Broker: broker,
		Store:  openResumeStore(t, dir),
		Kinds:  resumeKinds(t, dir),
	})
	if err != nil {
		t.Fatalf("recovery job.Open: %v", err)
	}

	// The killed attempt becomes interrupted and keeps its snapshot. The
	// hook rebuilds attempt three from it, so the chain lands done.
	interrupted := waitAttemptStatus(t, runner, "lead-0", 2, job.StatusInterrupted)
	if ledger, err := decodeResumeLedger(interrupted.Progress.Detail); err != nil || ledger.Ref != "lead-0" {
		t.Errorf("interrupted attempt snapshot = %s, %v, want the inherited ledger", interrupted.Progress.Detail, err)
	}
	third := waitAttemptStatus(t, runner, "lead-0", 3, job.StatusDone)
	if third.ParentID != interrupted.ID || third.RootID != "lead-0" {
		t.Errorf("attempt three = %+v, want parent %s and root lead-0", third, interrupted.ID)
	}
	done := waitForLines(t, resumeDonePath(dir), 1)
	if len(done) != 1 || done[0] != "done lead-0" {
		t.Errorf("completions = %v, want exactly [done lead-0]", done)
	}
}

// TestQueuedReplacementKeepsItsSnapshot kills the process while two
// replacements wait for the kind's single slot. Each queued row must carry
// its own inherited snapshot, so the next Open requeues it and every hook
// rebuilds from the ledger its own lineage recorded.
func TestQueuedReplacementKeepsItsSnapshot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the harness kills the child with SIGKILL")
	}
	dir := crashDir(t)
	ids := []string{"hold-0", "hold-1", "hold-2"}
	seedResumeDatabase(t, dir, ids)
	launchResumeChild(t, dir)

	// The killed state: the first replacement holds the slot and the other
	// two wait, each with the ledger its interrupted record carried.
	reader := openResumeStore(t, dir)
	wantStatus := map[string]job.Status{
		"hold-0": job.StatusRunning,
		"hold-1": job.StatusQueued,
		"hold-2": job.StatusQueued,
	}
	for _, id := range ids {
		assertInheritedLedger(t, reader, id, wantStatus[id])
	}

	broker := stream.New(stream.Config{Heartbeat: time.Hour})
	runner, err := job.Open(context.Background(), job.Config{
		Broker: broker,
		Store:  openResumeStore(t, dir),
		Kinds:  resumeKinds(t, dir),
	})
	if err != nil {
		t.Fatalf("recovery job.Open: %v", err)
	}

	// Every lineage finishes. The started attempt runs a third time, and
	// the two queued replacements are queued again and run from their own
	// snapshots.
	for i, id := range ids {
		attempt := 2
		if i == 0 {
			attempt = 3
		}
		waitAttemptStatus(t, runner, id, attempt, job.StatusDone)
	}
	done := waitForLines(t, resumeDonePath(dir), 3)
	sort.Strings(done)
	for i, want := range []string{"done hold-0", "done hold-1", "done hold-2"} {
		if done[i] != want {
			t.Errorf("completion %d = %q, want %q, full log %v", i, done[i], want, done)
		}
	}
}
