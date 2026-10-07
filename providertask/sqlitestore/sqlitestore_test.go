package sqlitestore_test

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nrynss/keel/providertask"
	"github.com/nrynss/keel/providertask/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// base is the instant the tests stamp claims with. It carries a non-zero
// nanosecond part, so a store that lost precision would be caught.
var base = time.Date(2026, 3, 1, 9, 30, 0, 123456789, time.UTC)

// openDB opens the database file at path for a test and closes it at
// cleanup.
func openDB(t *testing.T, path string) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path:   path,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open sqlite %q: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() }) // a reopened file closes once per handle, and a close is idempotent
	return db
}

// openStore applies the schema at path and returns the store.
func openStore(t *testing.T, path string) *sqlitestore.Store {
	t.Helper()
	store, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: openDB(t, path)})
	if err != nil {
		t.Fatalf("open store %q: %v", path, err)
	}
	return store
}

// openFresh opens the database file at path over a new connection with no
// pragmas from any package. It is the independent observer the tests query.
func openFresh(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() }) // the handle is discarded here, so a close failure cannot fail the test
	return db
}

// TestOpenRejectsNilDB: a store with no database refuses to open rather
// than panicking on the first write.
func TestOpenRejectsNilDB(t *testing.T) {
	if _, err := sqlitestore.Open(t.Context(), sqlitestore.Config{}); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Errorf("err = %v, want errors.Is(.., ErrInvalid)", err)
	}
}

// TestOpenAppliesMigrationsOnce: a second open on the same file changes
// nothing in the ledger, and a claim the first store wrote stays readable.
func TestOpenAppliesMigrationsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	first, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: openDB(t, path)})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, _, err := first.Claim(t.Context(), "kept", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	second := openStore(t, path)
	claim, err := second.Get(t.Context(), "kept")
	if err != nil {
		t.Fatalf("Get through the second store: %v", err)
	}
	if claim.Key != "kept" || !claim.CreatedAt.Equal(base) {
		t.Errorf("claim = %+v, want the row the first store wrote", claim)
	}
	var applied int
	if err := openFresh(t, path).QueryRow("SELECT COUNT(*) FROM providertask_schema_migrations").Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != 1 {
		t.Errorf("migration ledger holds %d rows, want 1", applied)
	}
}

// TestClaimInsertsOnceAndReportsTheWinner: concurrent claims of one key
// pick exactly one creator, and the file holds exactly one row for the key.
func TestClaimInsertsOnceAndReportsTheWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	store := openStore(t, path)

	const contenders = 8
	wins := make(chan bool, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := store.Claim(t.Context(), "raced", base)
			if err != nil {
				t.Errorf("Claim: %v", err)
				return
			}
			wins <- created
		}()
	}
	wg.Wait()
	close(wins)
	creators := 0
	for created := range wins {
		if created {
			creators++
		}
	}
	if creators != 1 {
		t.Errorf("created = %d times, want exactly one creator", creators)
	}
	var key, taskID, state string
	if err := openFresh(t, path).QueryRow(
		`SELECT key, task_id, state FROM providertask_task WHERE key = 'raced'`,
	).Scan(&key, &taskID, &state); err != nil {
		t.Fatalf("fresh query: %v", err)
	}
	if key != "raced" || taskID != "" || state != "running" {
		t.Errorf("row = %s %q %s, want the fresh running claim", key, taskID, state)
	}
	if got := countRows(t, openFresh(t, path), "raced"); got != 1 {
		t.Errorf("rows for the key = %d, want one", got)
	}
}

// countRows counts the rows a key has, through the independent connection.
func countRows(t *testing.T, db *sql.DB, key string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM providertask_task WHERE key = ?`, key,
	).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// TestClaimReturnsTheExistingRowUnchanged: a claim of a taken key reports
// the row and no creation, and leaves the stored facts alone.
func TestClaimReturnsTheExistingRowUnchanged(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	if _, created, err := store.Claim(t.Context(), "held", base); err != nil || !created {
		t.Fatalf("first Claim = %v %v, want created", err, created)
	}
	if err := store.Record(t.Context(), "held", "task-held"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	claim, created, err := store.Claim(t.Context(), "held", base.Add(time.Minute))
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if created {
		t.Error("second claim reported created, want false")
	}
	if claim.TaskID != "task-held" || !claim.CreatedAt.Equal(base) {
		t.Errorf("claim = %+v, want the stored row unchanged", claim)
	}
}

// TestClaimRoundTrip: the row a claim inserts carries the key, an empty
// task id, the running state and the creation time to the nanosecond.
func TestClaimRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	store := openStore(t, path)
	claim, created, err := store.Claim(t.Context(), "fresh", base)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !created {
		t.Fatal("first claim reported created false, want true")
	}
	if claim.TaskID != "" || claim.State != providertask.StateRunning || claim.Code != "" {
		t.Errorf("claim = %+v, want an empty running claim", claim)
	}
	var (
		taskID    string
		state     string
		errorCode string
		createdAt int64
	)
	if err := openFresh(t, path).QueryRow(
		`SELECT task_id, state, error_code, created_at FROM providertask_task WHERE key = 'fresh'`,
	).Scan(&taskID, &state, &errorCode, &createdAt); err != nil {
		t.Fatalf("fresh query: %v", err)
	}
	if taskID != "" || state != "running" || errorCode != "" {
		t.Errorf("row = %q %s %q, want an empty running claim", taskID, state, errorCode)
	}
	if !time.Unix(0, createdAt).Equal(base) {
		t.Errorf("created_at = %v, want %v to the nanosecond", time.Unix(0, createdAt), base)
	}
}

// TestRecordAttachesTheTaskIDOnce: the first record lands, a repeat of the
// recorded id is accepted, a different id is refused, and the row never
// moves.
func TestRecordAttachesTheTaskIDOnce(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	if _, _, err := store.Claim(t.Context(), "attach", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.Record(t.Context(), "attach", "task-a"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := store.Record(t.Context(), "attach", "task-a"); err != nil {
		t.Errorf("repeat of the recorded id: %v, want no error", err)
	}
	if err := store.Record(t.Context(), "attach", "task-b"); err == nil {
		t.Error("record of a different id passed, want an error")
	}
	claim, err := store.Get(t.Context(), "attach")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if claim.TaskID != "task-a" {
		t.Errorf("task id = %q, want task-a: the key must never move", claim.TaskID)
	}
	if err := store.Record(t.Context(), "missing", "task-x"); !errors.Is(err, providertask.ErrUnknownKey) {
		t.Errorf("unknown key: err = %v, want ErrUnknownKey", err)
	}
}

// TestRecordAfterFinishKeepsTheVerdict: a repeat of the recorded id after
// the verdict landed is not an error, and the row keeps its terminal state
// and the provider's code. The fresh connection reads the stored bytes, so
// the store never grades its own work.
func TestRecordAfterFinishKeepsTheVerdict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	store := openStore(t, path)
	if _, _, err := store.Claim(t.Context(), "done", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.Record(t.Context(), "done", "task-done"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := store.Finish(t.Context(), "done", providertask.StateFailed, "unit_limit"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := store.Record(t.Context(), "done", "task-done"); err != nil {
		t.Errorf("repeat after the verdict: %v, want no error", err)
	}
	claim, err := store.Get(t.Context(), "done")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if claim.TaskID != "task-done" || claim.State != providertask.StateFailed || claim.Code != "unit_limit" {
		t.Errorf("claim = %+v, want the failed verdict intact", claim)
	}
	var state, errorCode string
	if err := openFresh(t, path).QueryRow(
		`SELECT state, error_code FROM providertask_task WHERE key = 'done'`,
	).Scan(&state, &errorCode); err != nil {
		t.Fatalf("fresh query: %v", err)
	}
	if state != "failed" || errorCode != "unit_limit" {
		t.Errorf("row = %q %q, want the terminal verdict the finish wrote", state, errorCode)
	}
}

// TestGetUnknownKey: a key with no row matches the sentinel, so a waiter
// can tell an absent claim from a broken database.
func TestGetUnknownKey(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	if _, err := store.Get(t.Context(), "absent"); !errors.Is(err, providertask.ErrUnknownKey) {
		t.Errorf("err = %v, want ErrUnknownKey", err)
	}
}

// TestFinishRecordsTheVerdict: succeeded and failed land with the
// provider's code, a non terminal state is refused, and an unknown key
// matches the sentinel.
func TestFinishRecordsTheVerdict(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	if _, _, err := store.Claim(t.Context(), "win", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, _, err := store.Claim(t.Context(), "lose", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.Record(t.Context(), "win", "task-win"); err != nil {
		t.Fatalf("Record win: %v", err)
	}
	if err := store.Record(t.Context(), "lose", "task-lose"); err != nil {
		t.Fatalf("Record lose: %v", err)
	}
	if err := store.Finish(t.Context(), "win", providertask.StateSucceeded, ""); err != nil {
		t.Errorf("finish succeeded: %v", err)
	}
	if err := store.Finish(t.Context(), "lose", providertask.StateFailed, "unit_limit"); err != nil {
		t.Errorf("finish failed: %v", err)
	}
	if err := store.Finish(t.Context(), "win", providertask.StateRunning, ""); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Errorf("finish running: err = %v, want ErrInvalid", err)
	}
	if err := store.Finish(t.Context(), "absent", providertask.StateSucceeded, ""); !errors.Is(err, providertask.ErrUnknownKey) {
		t.Errorf("finish unknown: err = %v, want ErrUnknownKey", err)
	}
	win, err := store.Get(t.Context(), "win")
	if err != nil {
		t.Fatalf("Get win: %v", err)
	}
	if win.State != providertask.StateSucceeded || win.Code != "" {
		t.Errorf("win = %+v, want succeeded with no code", win)
	}
	lose, err := store.Get(t.Context(), "lose")
	if err != nil {
		t.Fatalf("Get lose: %v", err)
	}
	if lose.State != providertask.StateFailed || lose.Code != "unit_limit" {
		t.Errorf("lose = %+v, want failed with the provider code", lose)
	}
}

// TestReleaseDeletesOnlyAnEmptyClaim: an unresolved claim is removed, a
// key that names a task is kept, and the answer says which happened.
func TestReleaseDeletesOnlyAnEmptyClaim(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	if _, _, err := store.Claim(t.Context(), "empty", base); err != nil {
		t.Fatalf("Claim empty: %v", err)
	}
	if _, _, err := store.Claim(t.Context(), "recorded", base); err != nil {
		t.Fatalf("Claim recorded: %v", err)
	}
	if err := store.Record(t.Context(), "recorded", "task-recorded"); err != nil {
		t.Fatalf("Record: %v", err)
	}

	removed, err := store.Release(t.Context(), "empty")
	if err != nil || !removed {
		t.Errorf("release empty = %v %v, want removed", err, removed)
	}
	if _, err := store.Get(t.Context(), "empty"); !errors.Is(err, providertask.ErrUnknownKey) {
		t.Errorf("get after release: err = %v, want ErrUnknownKey", err)
	}
	removed, err = store.Release(t.Context(), "recorded")
	if err != nil || removed {
		t.Errorf("release recorded = %v %v, want kept", err, removed)
	}
	claim, err := store.Get(t.Context(), "recorded")
	if err != nil {
		t.Fatalf("Get recorded: %v", err)
	}
	if claim.TaskID != "task-recorded" {
		t.Errorf("task id = %q, want task-recorded: a release never erases a task", claim.TaskID)
	}
	removed, err = store.Release(t.Context(), "absent")
	if err != nil || removed {
		t.Errorf("release absent = %v %v, want false with no error", err, removed)
	}
}

// TestFreshConnectionPinsRows: a plain SQLite connection the store never
// holds sees exactly the rows the store wrote, with every column intact.
func TestFreshConnectionPinsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	store := openStore(t, path)
	want := []providertask.Claim{
		{Key: "one", TaskID: "", State: providertask.StateRunning, CreatedAt: base},
		{Key: "two", TaskID: "task-two", State: providertask.StateFailed, Code: "unit_limit", CreatedAt: base.Add(time.Second)},
	}
	for _, w := range want {
		if _, _, err := store.Claim(t.Context(), w.Key, w.CreatedAt); err != nil {
			t.Fatalf("Claim %s: %v", w.Key, err)
		}
		if w.TaskID != "" {
			if err := store.Record(t.Context(), w.Key, w.TaskID); err != nil {
				t.Fatalf("Record %s: %v", w.Key, err)
			}
		}
		if w.State == providertask.StateFailed {
			if err := store.Finish(t.Context(), w.Key, w.State, w.Code); err != nil {
				t.Fatalf("Finish %s: %v", w.Key, err)
			}
		}
	}

	rows, err := openFresh(t, path).Query(
		`SELECT key, task_id, state, error_code, created_at FROM providertask_task ORDER BY created_at`)
	if err != nil {
		t.Fatalf("fresh query: %v", err)
	}
	defer rows.Close()
	var got []providertask.Claim
	for rows.Next() {
		var (
			c         providertask.Claim
			state     string
			createdAt int64
		)
		if err := rows.Scan(&c.Key, &c.TaskID, &state, &c.Code, &createdAt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		c.State = providertask.State(state)
		c.CreatedAt = time.Unix(0, createdAt)
		got = append(got, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("fresh connection read %d rows, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Key != w.Key || got[i].TaskID != w.TaskID || got[i].State != w.State || got[i].Code != w.Code {
			t.Errorf("row %d = %+v, want %+v", i, got[i], w)
		}
		// A decoded time carries the local zone, so compare instants.
		if !got[i].CreatedAt.Equal(w.CreatedAt) {
			t.Errorf("row %d created at %v, want %v", i, got[i].CreatedAt, w.CreatedAt)
		}
	}
}

// TestConcurrentWritesShareOneWriter: claims, records and finishes from
// many goroutines all land, which is what a process full of jobs produces.
func TestConcurrentWritesShareOneWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	store := openStore(t, path)

	const writers = 8
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			key := fmt.Sprintf("writer-%d", w)
			ctx := t.Context()
			if _, _, err := store.Claim(ctx, key, base); err != nil {
				t.Errorf("Claim %s: %v", key, err)
				return
			}
			if err := store.Record(ctx, key, "task-"+key); err != nil {
				t.Errorf("Record %s: %v", key, err)
				return
			}
			if err := store.Finish(ctx, key, providertask.StateSucceeded, ""); err != nil {
				t.Errorf("Finish %s: %v", key, err)
			}
		}(w)
	}
	wg.Wait()

	for w := 0; w < writers; w++ {
		key := fmt.Sprintf("writer-%d", w)
		claim, err := store.Get(t.Context(), key)
		if err != nil {
			t.Fatalf("Get %s: %v", key, err)
		}
		if claim.State != providertask.StateSucceeded || claim.TaskID != "task-"+key {
			t.Errorf("claim %s = %+v, want the finished task", key, claim)
		}
	}
}

// TestStoreSurvivesAKilledProcess is the durability half of the store
// promise, checked without the full run harness: a claim and a record the
// child wrote are still there after the child died with its handles open.
func TestStoreSurvivesAKilledProcess(t *testing.T) {
	if os.Getenv(crashStoreChildEnv) == "" {
		// The child launches from the test below.
		path := filepath.Join(t.TempDir(), "tasks.db")
		runStoreCrashChild(t, path)

		// The WAL is the crash residue the reopen must recover.
		wal, err := os.Stat(path + "-wal")
		if err != nil {
			t.Fatalf("stat the WAL the dead child left: %v", err)
		}
		if wal.Size() == 0 {
			t.Fatalf("the WAL the child left is empty, want the committed frames in it")
		}
		store := openStore(t, path)
		claim, err := store.Get(t.Context(), "survivor")
		if err != nil {
			t.Fatalf("Get after the crash: %v", err)
		}
		if claim.TaskID != "task-survivor" || claim.State != providertask.StateRunning {
			t.Errorf("claim = %+v, want the recorded running task", claim)
		}
		return
	}
	dir := os.Getenv(crashStoreDirEnv)
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: filepath.Join(dir, "tasks.db")})
	if err != nil {
		t.Fatalf("child sqlite.Open: %v", err)
	}
	store, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: db})
	if err != nil {
		t.Fatalf("child sqlitestore.Open: %v", err)
	}
	if _, _, err := store.Claim(t.Context(), "survivor", base); err != nil {
		t.Fatalf("child Claim: %v", err)
	}
	if err := store.Record(t.Context(), "survivor", "task-survivor"); err != nil {
		t.Fatalf("child Record: %v", err)
	}
	// The exit path: this process dies here with its handles open, so no
	// close runs and the WAL keeps the committed frames for the parent's
	// reopen to recover.
	os.Exit(3)
}

// The environment the store crash harness passes to its child.
const (
	crashStoreChildEnv = "KEEL_PROVIDERTASK_STORE_CRASH_CHILD"
	crashStoreDirEnv   = "KEEL_PROVIDERTASK_STORE_CRASH_DIR"
)

// runStoreCrashChild launches the child process that claims and records,
// then dies with its handles open.
func runStoreCrashChild(t *testing.T, path string) {
	t.Helper()
	dir := filepath.Dir(path)
	child := exec.Command(os.Args[0], "-test.run=TestStoreSurvivesAKilledProcess")
	child.Env = append(os.Environ(), crashStoreChildEnv+"=1", crashStoreDirEnv+"="+dir)
	var childLog strings.Builder
	child.Stdout, child.Stderr = &childLog, &childLog
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 3 {
			t.Fatalf("child exit = %v, want status 3. Output:\n%s", err, childLog.String())
		}
	case <-time.After(30 * time.Second):
		_ = child.Process.Kill() // the watchdog fired, so the child only needs reaping
		<-done
		t.Fatalf("child hung. Output:\n%s", childLog.String())
	}
}
