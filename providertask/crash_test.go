package providertask_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nrynss/keel/providertask"
	"github.com/nrynss/keel/providertask/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// The environment the parent passes to the child it launches.
const (
	crashChildEnv  = "KEEL_PROVIDERTASK_CRASH_CHILD"
	crashDirEnv    = "KEEL_PROVIDERTASK_CRASH_DIR"
	crashMarkerEnv = "KEEL_PROVIDERTASK_CRASH_MARKER"
	crashRunName   = "TestCrashHelperProcess"
)

// TestRunSurvivesAKilledProcess kills a process between the record and the
// first poll, then resumes the key in this one. The child never closes its
// handles, so the committed frames are still in the WAL, and the reopen
// recovers them the way a real crash recovery runs. The resumed Run finds
// the recorded task id and never calls Create again, which is the promise
// the whole package carries.
func TestRunSurvivesAKilledProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.db")
	marker := filepath.Join(dir, "marker")

	child := exec.Command(os.Args[0], "-test.run="+crashRunName)
	child.Env = append(os.Environ(),
		crashChildEnv+"=1",
		crashDirEnv+"="+dir,
		crashMarkerEnv+"="+marker,
	)
	var childLog strings.Builder
	child.Stdout, child.Stderr = &childLog, &childLog
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	// The child exits by itself only if something is wrong, so the wait
	// runs beside a marker watch and the kill is the way out.
	waitCh := make(chan error, 1)
	go func() { waitCh <- child.Wait() }()
	waitForMarker(t, marker, child, waitCh, &childLog)
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	if err := <-waitCh; err == nil {
		t.Fatalf("child survived the kill. Output:\n%s", childLog.String())
	}

	// The WAL is the crash residue. A process that closed politely would
	// have checkpointed it away, and an empty WAL cannot hold this
	// package's durability promise.
	wal, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatalf("stat the WAL the dead child left: %v", err)
	}
	if wal.Size() == 0 {
		t.Fatalf("the WAL the child left is empty, want the committed frames in it")
	}

	// The restart resumes the recorded task. The parent's own provider
	// never creates, so a second create would fail the test outright.
	_, store := openAt(t, path)
	p := newFakeProvider(rendered{Link: "resumed"}, step{st: providertask.Status{State: providertask.StateSucceeded}})
	cfg := providertask.Config{Log: slog.New(slog.DiscardHandler)}
	outcome, err := providertask.Run(t.Context(), cfg, store, "crashed", p.spec(p.Keep))
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	creates, statuses, results, keeps := p.counts()
	if creates != 0 {
		t.Errorf("resume created %d tasks, want none: the recorded task must be resumed", creates)
	}
	if statuses != 1 {
		t.Errorf("statuses = %d, want one poll of the recorded task", statuses)
	}
	if results != 1 || keeps != 1 {
		t.Errorf("results=%d keeps=%d, want one fetch and one keep", results, keeps)
	}
	if outcome.Value.Link != "resumed" || outcome.TaskID == "" {
		t.Errorf("outcome = %+v, want the resumed task's result", outcome)
	}
	claim, err := store.Get(t.Context(), "crashed")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if claim.State != providertask.StateSucceeded {
		t.Errorf("state = %s, want the resumed verdict recorded", claim.State)
	}
}

// waitForMarker blocks until the child wrote the marker, which happens on
// its first poll and therefore after the task id was recorded durably. The
// watchdog kills a child that never got there.
func waitForMarker(t *testing.T, marker string, child *exec.Cmd, waitCh <-chan error, childLog *strings.Builder) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		select {
		case err := <-waitCh:
			t.Fatalf("child exited before the marker. err = %v. Output:\n%s", err, childLog.String())
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			_ = child.Process.Kill() // the watchdog fired, so the child only needs reaping
			<-waitCh
			t.Fatalf("child never wrote the marker. Output:\n%s", childLog.String())
		}
	}
}

// TestCrashHelperProcess is the child half of the killed-run harness. A
// plain go test run skips it, because only the parent sets the child
// variable.
func TestCrashHelperProcess(t *testing.T) {
	if os.Getenv(crashChildEnv) == "" {
		t.Skip("child of the providertask crash harness")
	}
	dir := os.Getenv(crashDirEnv)
	marker := os.Getenv(crashMarkerEnv)
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "tasks.db")})
	if err != nil {
		t.Fatalf("child sqlite.Open: %v", err)
	}
	store, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: db})
	if err != nil {
		t.Fatalf("child sqlitestore.Open: %v", err)
	}
	p := newFakeProvider(rendered{Link: "child"}, step{st: providertask.Status{State: providertask.StateSucceeded}})
	spec := p.spec(nil)
	once := true
	spec.Status = func(ctx context.Context, taskID string) (providertask.Status, error) {
		if once {
			once = false
			// The record has landed before the first poll, so writing the
			// marker here tells the parent the task id is durable.
			if err := os.WriteFile(marker, []byte("polled"), 0o644); err != nil {
				t.Errorf("child marker: %v", err)
			}
		}
		// The goroutine's exit path: the parent kills the process while
		// the poll blocks here, so nothing after the marker ever runs.
		blockForever(ctx)
		return providertask.Status{State: providertask.StateSucceeded}, nil
	}
	cfg := providertask.Config{Log: slog.New(slog.DiscardHandler)}
	if _, err := providertask.Run(ctx, cfg, store, "crashed", spec); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("child Run: %v", err)
	}
}

// blockForever parks the calling goroutine until the process dies. The
// kill is the only exit path, which is what the harness arranges, and the
// timer keeps the runtime's deadlock detector quiet while it waits.
func blockForever(context.Context) {
	<-time.After(time.Hour)
}
