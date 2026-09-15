package job_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nrynss/keel/job"
	"github.com/nrynss/keel/job/sqlitestore"
	"github.com/nrynss/keel/sqlite"
	"github.com/nrynss/keel/stream"
	"github.com/nrynss/keel/wire"
)

// The environment the parent passes to the child it launches.
const (
	childEnv       = "KEEL_JOB_CRASH_CHILD"
	childDirEnv    = "KEEL_JOB_CRASH_DIR"
	helperRun      = "TestCrashHelperProcess"
	harnessJobWant = 2
)

// crashDir returns the directory the harness keeps its files in. The parent
// sets it when the evidence has to outlive the test process. Otherwise the
// test's temporary directory is used.
func crashDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv(childDirEnv); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		return dir
	}
	return t.TempDir()
}

// appendLine adds one line to path, the out-of-process counter the harness
// reads back after the crash. The line lands through a separate file
// descriptor, so nothing in the job package can coalesce or drop it.
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if _, err := fmt.Fprintln(f, line); err != nil {
		f.Close()
		t.Fatalf("append to %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// lines returns path's non-empty lines, or an error when it is unreadable.
func lines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

// waitForLines blocks until path holds at least n non-empty lines.
func waitForLines(t *testing.T, path string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var got []string
	for {
		got, _ = lines(path)
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d line(s) in %s, have %v", n, path, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// crashKinds is the kind table both processes share. Both kinds declare
// resume work that appends one line to donePath, so the harness counts how
// many times each ran. Only the idempotent kind may run again, so a single
// line proves the gate held.
func crashKinds(t *testing.T, donePath string) map[string]job.Kind {
	t.Helper()
	resume := func(rec job.Record) (job.Func, error) {
		return func(ctx context.Context, progress func(job.Progress)) ([]byte, error) {
			appendLine(t, donePath, rec.Kind+" "+rec.ID)
			return []byte("resumed"), nil
		}, nil
	}
	return map[string]job.Kind{
		"index":  {Limit: 4, Idempotent: true, MaxAttempts: 3, Resume: resume},
		"report": {Limit: 4, MaxAttempts: 3, Resume: resume},
	}
}

// TestCrashHelperProcess is the child half of the harness. A plain `go test`
// run skips it, because the parent sets childEnv only on the process it
// launches.
func TestCrashHelperProcess(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip("child of the live-process harness")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the harness kills the child with SIGKILL")
	}
	dir := os.Getenv(childDirEnv)
	donePath := filepath.Join(dir, "completions.log")
	startPath := filepath.Join(dir, "attempts.log")
	idsPath := filepath.Join(dir, "ids.txt")

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
	runner, err := job.Open(ctx, job.Config{Broker: broker, Store: store, Kinds: crashKinds(t, donePath)})
	if err != nil {
		t.Fatalf("child job.Open: %v", err)
	}

	// Both jobs announce themselves, then wait for a context nobody
	// cancels. The parent kills this process instead, which is the only way
	// to leave a job running across a process exit.
	block := func(kind string) job.Func {
		return func(ctx context.Context, progress func(job.Progress)) ([]byte, error) {
			appendLine(t, startPath, kind)
			progress(job.Progress{Stage: kind})
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	indexID, err := runner.StartKind(ctx, "index", block("index"))
	if err != nil {
		t.Fatalf("child StartKind index: %v", err)
	}
	reportID, err := runner.StartKind(ctx, "report", block("report"))
	if err != nil {
		t.Fatalf("child StartKind report: %v", err)
	}
	appendLine(t, idsPath, indexID)
	appendLine(t, idsPath, reportID)

	for {
		time.Sleep(time.Hour)
	}
}

// TestCrashRecoveryAcrossProcessKill kills a process with two running jobs
// and then recovers them in this one. It is the durability proof: the state
// is read from the file after the crash, not from the process that wrote it.
func TestCrashRecoveryAcrossProcessKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the harness kills the child with SIGKILL")
	}
	dir := crashDir(t)
	dbPath := filepath.Join(dir, "jobs.db")
	donePath := filepath.Join(dir, "completions.log")
	startPath := filepath.Join(dir, "attempts.log")
	idsPath := filepath.Join(dir, "ids.txt")
	for _, path := range []string{dbPath, donePath, startPath, idsPath} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatalf("clear %s: %v", path, err)
		}
	}

	child := exec.Command(os.Args[0], "-test.run="+helperRun)
	child.Env = append(os.Environ(), childEnv+"=1", childDirEnv+"="+dir)
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

	// Wait until both jobs are genuinely running, so the kill lands on work
	// in flight rather than on a process still starting up.
	started := waitForLines(t, startPath, harnessJobWant)
	ids := waitForLines(t, idsPath, harnessJobWant)
	indexID, reportID := ids[0], ids[1]
	t.Logf("child jobs running: %v", started)

	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	if err := child.Wait(); err == nil {
		t.Fatalf("child exited cleanly, want a kill. Output:\n%s", childLog.String())
	}

	// Independent state: the file on disk, read through a connection this
	// test opens itself, before any recovery has run.
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("recovery sqlite.Open: %v", err)
	}
	defer db.Close()
	store, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: db})
	if err != nil {
		t.Fatalf("recovery sqlitestore.Open: %v", err)
	}

	before, err := store.Get(ctx, indexID)
	if err != nil {
		t.Fatalf("Get %s before recovery: %v", indexID, err)
	}
	if before.Status != job.StatusRunning {
		t.Errorf("status before recovery = %q, want %q from the killed process", before.Status, job.StatusRunning)
	}
	if before.Attempt != 1 || before.RootID != indexID {
		t.Errorf("lineage before recovery = %+v, want attempt 1 rooted at %s", before, indexID)
	}
	if _, err := lines(donePath); err == nil {
		t.Errorf("%s exists before recovery, want the killed process to have completed nothing", donePath)
	}

	broker := stream.New(stream.Config{Heartbeat: time.Hour})
	runner, err := job.Open(ctx, job.Config{Broker: broker, Store: store, Kinds: crashKinds(t, donePath)})
	if err != nil {
		t.Fatalf("recovery job.Open: %v", err)
	}

	// The interrupted original: recorded as interrupted, and its terminal is
	// on its topic for a subscriber that arrives late.
	interrupted, err := store.Get(ctx, indexID)
	if err != nil {
		t.Fatalf("Get %s: %v", indexID, err)
	}
	if interrupted.Status != job.StatusInterrupted {
		t.Errorf("status after recovery = %q, want %q", interrupted.Status, job.StatusInterrupted)
	}
	assertRetainedTerminal(t, broker.Subscribe(ctx, job.Topic(indexID)), indexID, job.StatusInterrupted)

	// The replacement attempt: a fresh id, one attempt further on, and it
	// completes the work the killed process never finished.
	attempts := waitForTerminalAttempt(t, runner, indexID, 2)
	next := attempts[1]
	if next.ParentID != indexID || next.RootID != indexID || next.Attempt != 2 {
		t.Errorf("replacement = %+v, want attempt 2 with parent and root %s", next, indexID)
	}
	if next.ID == indexID {
		t.Error("the replacement reused the interrupted id")
	}
	assertRetainedTerminal(t, broker.Subscribe(ctx, job.Topic(next.ID)), next.ID, job.StatusDone)

	completions := waitForLines(t, donePath, 1)
	if len(completions) != 1 {
		t.Errorf("completions = %v, want exactly one across the crash", completions)
	}
	// The resumed work reports the kind and the interrupted record it was
	// handed, which is the logical job the replacement serves.
	if want := "index " + indexID; completions[0] != want {
		t.Errorf("completion %q, want %q", completions[0], want)
	}
	// The non-idempotent kind is left interrupted and never rerun.
	report, err := store.Get(ctx, reportID)
	if err != nil {
		t.Fatalf("Get %s: %v", reportID, err)
	}
	if report.Status != job.StatusInterrupted {
		t.Errorf("report status = %q, want %q", report.Status, job.StatusInterrupted)
	}
	reportAttempts, err := runner.Attempts(ctx, reportID)
	if err != nil {
		t.Fatalf("Attempts %s: %v", reportID, err)
	}
	if len(reportAttempts) != 1 {
		t.Errorf("report attempts = %+v, want the single interrupted original", reportAttempts)
	}
	if completions, err := lines(donePath); err != nil || len(completions) != 1 {
		t.Errorf("completions after the report check = %v, %v, want exactly one", completions, err)
	}
}

// assertRetainedTerminal reads sub's one event and pins its name and its
// payload's job and status. Because the broker replays a terminal once and
// then closes the subscription, a second event means the job published two
// terminals.
func assertRetainedTerminal(t *testing.T, sub *stream.Subscription, id string, want job.Status) {
	t.Helper()
	defer sub.Cancel()
	select {
	case ev, ok := <-sub.Events:
		if !ok {
			t.Fatalf("%s: subscription closed with no terminal", id)
		}
		if ev.Name != string(want) {
			t.Errorf("%s: terminal name = %q, want %q", id, ev.Name, want)
		}
		payload, ok := ev.Data.(wire.StatusEvent)
		if !ok {
			t.Errorf("%s: payload = %T, want wire.StatusEvent", id, ev.Data)
		} else if payload.JobID != id || payload.Status != string(want) {
			t.Errorf("%s: payload = %+v, want its own job and status %q", id, payload, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no terminal on the topic after recovery", id)
	}
	select {
	case ev, ok := <-sub.Events:
		if ok {
			t.Errorf("%s: second terminal %+v, want exactly one", id, ev)
		}
	case <-time.After(200 * time.Millisecond):
		t.Errorf("%s: subscription still open after its terminal", id)
	}
}

// waitForTerminalAttempt polls the lineage of id until it holds want attempts
// and the last one has reached a terminal state.
func waitForTerminalAttempt(t *testing.T, runner *job.Runner, id string, want int) []job.Record {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var attempts []job.Record
	for {
		var err error
		attempts, err = runner.Attempts(context.Background(), id)
		if err != nil {
			t.Fatalf("Attempts %s: %v", id, err)
		}
		if len(attempts) == want && attempts[want-1].Status != job.StatusRunning && attempts[want-1].Status != job.StatusQueued {
			return attempts
		}
		if time.Now().After(deadline) {
			t.Fatalf("attempts = %+v, want %d with the last one terminal", attempts, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitAttemptStatus polls a logical job's lineage until the attempt with the
// given number reaches want, so a caller can read a replacement's state.
func waitAttemptStatus(t *testing.T, runner *job.Runner, id string, attempt int, want job.Status) job.Record {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		attempts, err := runner.Attempts(context.Background(), id)
		if err != nil {
			t.Fatalf("Attempts %s: %v", id, err)
		}
		for _, a := range attempts {
			if a.Attempt == attempt && a.Status == want {
				return a
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("attempts = %+v, want attempt %d of %s in status %q", attempts, attempt, id, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The environment the saturated-capacity harness uses. The child writes an
// open marker after recovery, so the parent kills after every replacement row
// is durable rather than mid-write.
const (
	saturatedChildEnv  = "KEEL_JOB_SATURATED_CHILD"
	saturatedHelperRun = "TestCrashSaturatedHelperProcess"
)

// saturatedKinds is the kind table for the saturated crash harness. The one
// kind has a single slot and two attempts, so three unfinished records force
// two of them to wait for capacity. Its resume work blocks until the process
// dies, which holds the started attempt while the rest stay queued.
func saturatedKinds(t *testing.T, startPath string) map[string]job.Kind {
	t.Helper()
	return map[string]job.Kind{
		"index": {
			Limit:       1,
			Idempotent:  true,
			MaxAttempts: 2,
			Resume: func(rec job.Record) (job.Func, error) {
				return func(ctx context.Context, progress func(job.Progress)) ([]byte, error) {
					appendLine(t, startPath, "index "+rec.ID)
					<-ctx.Done()
					return nil, ctx.Err()
				}, nil
			},
		},
	}
}

// TestCrashSaturatedHelperProcess is the child half of the saturated harness.
// It opens a runner over seeded unfinished records, which recovers and resumes
// them, marks the open complete, then blocks. The parent kills it in the wait.
func TestCrashSaturatedHelperProcess(t *testing.T) {
	if os.Getenv(saturatedChildEnv) == "" {
		t.Skip("child of the saturated-capacity crash harness")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the harness kills the child with SIGKILL")
	}
	dir := os.Getenv(childDirEnv)
	startPath := filepath.Join(dir, "saturated-starts.log")
	openPath := filepath.Join(dir, "saturated-open.log")
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
	if _, err := job.Open(ctx, job.Config{Broker: broker, Store: store, Kinds: saturatedKinds(t, startPath)}); err != nil {
		t.Fatalf("child job.Open: %v", err)
	}
	appendLine(t, openPath, "open")
	for {
		time.Sleep(time.Hour)
	}
}

// TestCrashSaturatedKindKeepsEveryRetry kills a process whose kind is at its
// limit with more unfinished records than slots. The wait for a slot must not
// charge an attempt, so a replacement that never started is durable as queued
// and the next Open starts it rather than stranding it.
//
// The pin reads the file over a connection the runner never sees.
func TestCrashSaturatedKindKeepsEveryRetry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the harness kills the child with SIGKILL")
	}
	dir := crashDir(t)
	dbPath := filepath.Join(dir, "jobs.db")
	startPath := filepath.Join(dir, "saturated-starts.log")
	openPath := filepath.Join(dir, "saturated-open.log")
	for _, path := range []string{dbPath, startPath, openPath} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatalf("clear %s: %v", path, err)
		}
	}

	ctx := context.Background()
	ids := []string{"sat-0", "sat-1", "sat-2"}
	seed, err := sqlite.Open(ctx, sqlite.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("seed sqlite.Open: %v", err)
	}
	seedStore, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: seed})
	if err != nil {
		t.Fatalf("seed sqlitestore.Open: %v", err)
	}
	for _, id := range ids {
		rec := job.Record{ID: id, Kind: "index", Status: job.StatusRunning, Attempt: 1, RootID: id}
		if err := seedStore.Create(ctx, rec); err != nil {
			t.Fatalf("seed Create %s: %v", id, err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	child := exec.Command(os.Args[0], "-test.run="+saturatedHelperRun)
	child.Env = append(os.Environ(), saturatedChildEnv+"=1", childDirEnv+"="+dir)
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

	// The open marker proves the child finished recovery, so every durable
	// replacement row is already written. One attempt has started and blocks,
	// so the kill lands after the writes rather than during them.
	waitForLines(t, openPath, 1)
	started := waitForLines(t, startPath, 1)
	t.Logf("child started attempts: %v", started)

	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	if err := child.Wait(); err == nil {
		t.Fatalf("child exited cleanly, want a kill. Output:\n%s", childLog.String())
	}

	// Independent state: the file on disk, read through a connection the
	// runner never sees and before any recovery has run. Every seeded record
	// owns its replacement row. Only the attempt that started is running,
	// because it holds the single slot. The rest wait for a slot and stay
	// queued, so a wait never charged their attempt.
	readDB, err := sqlite.Open(ctx, sqlite.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("reader sqlite.Open: %v", err)
	}
	defer readDB.Close() // cleanup: the test is over, so a close error changes no result
	reader, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: readDB})
	if err != nil {
		t.Fatalf("reader sqlitestore.Open: %v", err)
	}
	wantBefore := map[string]job.Status{
		"sat-0": job.StatusRunning,
		"sat-1": job.StatusQueued,
		"sat-2": job.StatusQueued,
	}
	for _, id := range ids {
		attempts, err := reader.Attempts(ctx, id)
		if err != nil {
			t.Fatalf("Attempts %s before recovery: %v", id, err)
		}
		if len(attempts) != 2 {
			t.Fatalf("%s attempts before recovery = %d, want 2: the replacement must be durable before the kill", id, len(attempts))
		}
		if got := attempts[0]; got.Attempt != 1 || got.Status != job.StatusInterrupted || got.RootID != id {
			t.Errorf("%s first attempt before recovery = %+v, want interrupted attempt 1 rooted at %s", id, got, id)
		}
		if got := attempts[1]; got.Attempt != 2 || got.ParentID != id || got.RootID != id {
			t.Errorf("%s replacement before recovery = %+v, want attempt 2 with parent and root %s", id, got, id)
		}
		if got := attempts[1]; got.Status != wantBefore[id] {
			t.Errorf("%s replacement status before recovery = %q, want %q: only the started attempt is running", id, got.Status, wantBefore[id])
		}
	}

	// The runner writes through its own connection to the same file, so the
	// reader above stays a connection the runner never sees.
	db, err := sqlite.Open(ctx, sqlite.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("recovery sqlite.Open: %v", err)
	}
	defer db.Close() // cleanup: the test is over, so a close error changes no result
	store, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: db})
	if err != nil {
		t.Fatalf("recovery sqlitestore.Open: %v", err)
	}

	broker := stream.New(stream.Config{Heartbeat: time.Hour})
	runner, err := job.Open(ctx, job.Config{Broker: broker, Store: store, Kinds: saturatedKinds(t, startPath)})
	if err != nil {
		t.Fatalf("recovery job.Open: %v", err)
	}

	// The retry that started, sat-0's, is interrupted: it may have done
	// work. The two retries that never started are queued again and run, so
	// a saturated kind loses no retry and charges no attempt twice.
	if got := waitAttemptStatus(t, runner, "sat-0", 2, job.StatusInterrupted); got.ParentID != "sat-0" {
		t.Errorf("sat-0 retry after recovery = %+v, want parent sat-0", got)
	}
	if got := waitAttemptStatus(t, runner, "sat-1", 2, job.StatusRunning); got.ParentID != "sat-1" {
		t.Errorf("sat-1 retry after recovery = %+v, want parent sat-1", got)
	}
	waitAttemptStatus(t, runner, "sat-2", 2, job.StatusQueued)
	started = waitForLines(t, startPath, 2)
	t.Logf("attempts started after recovery: %v", started)

	// Attempt counting: each lineage holds two records, exactly one retry
	// never started and stays queued, and no lineage holds an attempt past
	// the cap. Two charged attempts ran, one waited, so no attempt was
	// charged twice and none was charged for work that never ran.
	queued := 0
	for _, id := range ids {
		attempts, err := reader.Attempts(ctx, id)
		if err != nil {
			t.Fatalf("Attempts %s after recovery: %v", id, err)
		}
		if len(attempts) != 2 {
			t.Fatalf("%s attempts after recovery = %d, want 2", id, len(attempts))
		}
		t.Logf("%s lineage: %+v", id, attempts)
		for _, a := range attempts {
			if a.Attempt > 2 {
				t.Errorf("%s attempt = %+v, want no attempt past MaxAttempts 2", id, a)
			}
			if a.Status == job.StatusQueued {
				queued++
			}
		}
	}
	if queued != 1 {
		t.Errorf("queued retries = %d, want exactly the one that never started", queued)
	}
}
