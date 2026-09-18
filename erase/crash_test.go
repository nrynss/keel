package erase_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nrynss/keel/erase"
	"github.com/nrynss/keel/job"
	jobsql "github.com/nrynss/keel/job/sqlitestore"
	"github.com/nrynss/keel/sqlite"
	"github.com/nrynss/keel/stream"
)

// The environment the parent passes to the child it launches.
const (
	crashChildEnv  = "KEEL_ERASE_CRASH_CHILD"
	crashDirEnv    = "KEEL_ERASE_CRASH_DIR"
	crashHelperRun = "TestEraseCrashHelperProcess"
)

// crashTarget logs every delete to the harness log, which both processes
// read back. The slow one blocks in the child, so the kill lands with its
// delete still unfinished.
type crashTarget struct {
	name    string
	logPath string
	block   bool
}

// Name identifies the target.
func (t *crashTarget) Name() string { return t.name }

// Delete logs the call, then confirms or blocks until the process dies. A
// log line that cannot land fails the delete, so a lost line can never
// look like a confirmed one.
func (t *crashTarget) Delete(ctx context.Context) error {
	if err := appendCrashLine(t.logPath, t.name); err != nil {
		return fmt.Errorf("erase crash harness: %w", err)
	}
	if !t.block {
		return nil
	}
	<-ctx.Done() // the child blocks here, because only the kill ends this delete
	return ctx.Err()
}

// crashTargets builds the three targets the harness registers.
func crashTargets(logPath string, block bool) []erase.Target {
	return []erase.Target{
		&crashTarget{name: "alpha", logPath: logPath},
		&crashTarget{name: "slow", logPath: logPath, block: block},
		&crashTarget{name: "omega", logPath: logPath},
	}
}

// crashSource rebuilds the target list for the harness ref.
func crashSource(logPath string, block bool) erase.Source {
	return func(ctx context.Context, ref string) ([]erase.Target, error) {
		return crashTargets(logPath, block), nil
	}
}

// appendCrashLine adds one line to path, the cross-process counter the
// harness reads back after the crash.
func appendCrashLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	_, err = fmt.Fprintln(f, line)
	closeErr := f.Close()
	if err != nil {
		return fmt.Errorf("append to %s: %w", path, err)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", path, closeErr)
	}
	return nil
}

// readCrashLines returns path's non-empty lines.
func readCrashLines(path string) ([]string, error) {
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

// waitForID blocks until the child has written the job id.
func waitForID(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		lines, err := readCrashLines(path)
		if err == nil && len(lines) >= 1 {
			return lines[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the job id in %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// countName counts how many log lines name the target.
func countName(lines []string, name string) int {
	n := 0
	for _, line := range lines {
		if line == name {
			n++
		}
	}
	return n
}

// TestEraseCrashHelperProcess is the child half of the harness. A plain
// `go test` run skips it, because the parent sets crashChildEnv only on the
// process it launches.
func TestEraseCrashHelperProcess(t *testing.T) {
	if os.Getenv(crashChildEnv) == "" {
		t.Skip("child of the crash harness")
	}
	dir := os.Getenv(crashDirEnv)
	logPath := filepath.Join(dir, "deletes.log")

	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "jobs.db")})
	if err != nil {
		t.Fatalf("child sqlite open: %v", err)
	}
	defer db.Close()
	store, err := jobsql.Open(ctx, jobsql.Config{DB: db})
	if err != nil {
		t.Fatalf("child store open: %v", err)
	}
	er, err := erase.New(crashSource(logPath, true), erase.Config{})
	if err != nil {
		t.Fatalf("child erase new: %v", err)
	}
	broker := stream.New(stream.Config{Heartbeat: time.Hour})
	runner, err := job.Open(ctx, job.Config{
		Broker: broker,
		Store:  store,
		Kinds:  map[string]job.Kind{erase.KindName: er.Kind()},
	})
	if err != nil {
		t.Fatalf("child job open: %v", err)
	}

	id, err := er.Start(ctx, runner, "crash", crashTargets(logPath, true))
	if err != nil {
		t.Fatalf("child start: %v", err)
	}
	if err := appendCrashLine(filepath.Join(dir, "id.txt"), id); err != nil {
		t.Fatalf("write job id: %v", err)
	}

	// The parent kills this process, which is the only way a delete stays
	// unfinished across an exit.
	for {
		time.Sleep(time.Hour)
	}
}

// TestCrashResumptionSkipsConfirmedTargets kills a process mid fan-out and
// restarts the erasure over the same store. The pin is the delete log: the
// targets that confirmed before the kill are never called again, and only
// the unfinished one runs a second time.
func TestCrashResumptionSkipsConfirmedTargets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the harness kills the child with SIGKILL")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")
	logPath := filepath.Join(dir, "deletes.log")
	idPath := filepath.Join(dir, "id.txt")

	child := exec.Command(os.Args[0], "-test.run="+crashHelperRun)
	child.Env = append(os.Environ(), crashChildEnv+"=1", crashDirEnv+"="+dir)
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

	id := waitForID(t, idPath)

	// Poll the ledger through a connection the runner never sees, so the
	// kill lands only after two confirmations are durable and the slow
	// delete is genuinely stuck mid fan-out.
	ctx := context.Background()
	pollDB, err := sqlite.Open(ctx, sqlite.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("poll sqlite open: %v", err)
	}
	pollStore, err := jobsql.Open(ctx, jobsql.Config{DB: pollDB})
	if err != nil {
		t.Fatalf("poll store open: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	var rec job.Record
	for {
		rec, err = pollStore.Get(ctx, id)
		if err != nil {
			t.Fatalf("poll get %s: %v", id, err)
		}
		var snap struct {
			Confirmed []string `json:"confirmed"`
		}
		_ = json.Unmarshal(rec.Progress.Detail, &snap) // a snapshot not written yet reads as empty, which the loop retries
		lines, _ := readCrashLines(logPath)
		confirmed := 0
		for _, name := range snap.Confirmed {
			if name == "alpha" || name == "omega" {
				confirmed++
			}
		}
		if confirmed == 2 &&
			countName(lines, "alpha") == 1 &&
			countName(lines, "omega") == 1 &&
			countName(lines, "slow") == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for durable confirmations, detail %s, log %v", rec.Progress.Detail, lines)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rec.Status != job.StatusRunning {
		t.Errorf("status before the kill = %q, want %q", rec.Status, job.StatusRunning)
	}

	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	if err := child.Wait(); err == nil {
		t.Fatalf("child exited cleanly, want a kill. Output:\n%s", childLog.String())
	}
	if err := pollDB.Close(); err != nil {
		t.Fatalf("close poll db: %v", err)
	}

	// The restart opens the same store and the same kind, which is what a
	// recovered process does. Its targets do not block, so the resumed
	// run finishes the erasure.
	db, err := sqlite.Open(ctx, sqlite.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("recovery sqlite open: %v", err)
	}
	defer db.Close()
	store, err := jobsql.Open(ctx, jobsql.Config{DB: db})
	if err != nil {
		t.Fatalf("recovery store open: %v", err)
	}
	er, err := erase.New(crashSource(logPath, false), erase.Config{})
	if err != nil {
		t.Fatalf("recovery erase new: %v", err)
	}
	broker := stream.New(stream.Config{Heartbeat: time.Hour})
	runner, err := job.Open(ctx, job.Config{
		Broker: broker,
		Store:  store,
		Kinds:  map[string]job.Kind{erase.KindName: er.Kind()},
	})
	if err != nil {
		t.Fatalf("recovery job open: %v", err)
	}

	attempts := waitAttempts(t, ctx, runner, id, 2)
	if attempts[0].Status != job.StatusInterrupted {
		t.Errorf("killed attempt status = %q, want %q", attempts[0].Status, job.StatusInterrupted)
	}
	if attempts[1].Status != job.StatusDone {
		t.Errorf("resumed attempt status = %q with error %v, want %q", attempts[1].Status, attempts[1].Err, job.StatusDone)
	}

	lines, err := readCrashLines(logPath)
	if err != nil {
		t.Fatalf("read %s: %v", logPath, err)
	}
	if got := countName(lines, "alpha"); got != 1 {
		t.Errorf("alpha deletes = %d, want 1, because it confirmed before the kill", got)
	}
	if got := countName(lines, "omega"); got != 1 {
		t.Errorf("omega deletes = %d, want 1, because it confirmed before the kill", got)
	}
	if got := countName(lines, "slow"); got != 2 {
		t.Errorf("slow deletes = %d, want 2, one per attempt", got)
	}

	rep, err := er.Inspect(ctx, runner, id)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !rep.Complete() {
		t.Errorf("Complete() = false with stuck %v, want true", rep.Stuck)
	}
	if got, want := rep.Confirmed, []string{"alpha", "omega", "slow"}; !equal(got, want) {
		t.Errorf("Confirmed = %v, want %v", got, want)
	}
	if rep.Ref != "crash" {
		t.Errorf("Ref = %q, want crash", rep.Ref)
	}
}
