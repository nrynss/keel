package sqlitestore_test

import (
	"bytes"
	"context"
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

	"github.com/nrynss/keel/outbox"
	"github.com/nrynss/keel/outbox/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// base is the instant the tests stamp entries with. It carries a non-zero
// nanosecond part, so a store that lost precision would be caught.
var base = time.Date(2025, 4, 1, 9, 30, 0, 123456789, time.UTC)

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

// entry builds one entry the tests store, with a distinct id per name.
func entry(name string, payload []byte) outbox.Entry {
	return outbox.Entry{
		ID:       fmt.Sprintf("id-%s-0123456789abcdef", name),
		Payload:  payload,
		Failures: 0,
		AddedAt:  base,
	}
}

// TestOpenRejectsNilDB: a store with no database refuses to open rather than
// panicking on the first write.
func TestOpenRejectsNilDB(t *testing.T) {
	if _, err := sqlitestore.Open(t.Context(), sqlitestore.Config{}); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Errorf("err = %v, want errors.Is(.., ErrInvalid)", err)
	}
}

// TestOpenAppliesMigrationsOnce: a second open on the same file changes
// nothing in the ledger, and data the first store wrote stays readable.
func TestOpenAppliesMigrationsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.db")
	first, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: openDB(t, path)})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	want := entry("first", []byte("payload-a"))
	if err := first.Add(t.Context(), want); err != nil {
		t.Fatalf("Add: %v", err)
	}

	second := openStore(t, path)
	got, err := second.Pending(t.Context(), 1, -1)
	if err != nil {
		t.Fatalf("Pending through the second store: %v", err)
	}
	if len(got) != 1 || got[0].ID != want.ID {
		t.Fatalf("Pending = %+v, want the row the first store wrote", got)
	}

	var applied int
	if err := openFresh(t, path).QueryRow("SELECT COUNT(*) FROM outbox_schema_migrations").Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != 1 {
		t.Errorf("migration ledger holds %d rows, want 1", applied)
	}
}

// TestAddPendingRoundTrip: every stored field comes back, including payload
// bytes a text column would mangle and a timestamp to the nanosecond.
func TestAddPendingRoundTrip(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "outbox.db"))

	stamped := entry("stamped", []byte{'b', 0x00, 'i', 0xff, 'n'})
	stamped.Failures = 2
	stamped.AddedAt = base.Add(3 * time.Second)
	if err := store.Add(t.Context(), stamped); err != nil {
		t.Fatalf("Add: %v", err)
	}
	empty := entry("empty", nil)
	if err := store.Add(t.Context(), empty); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := store.Pending(t.Context(), 3, -1)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Pending = %d entries, want 2", len(got))
	}
	if got[0].ID != stamped.ID || !bytes.Equal(got[0].Payload, stamped.Payload) {
		t.Errorf("first entry = %s %x, want %s %x", got[0].ID, got[0].Payload, stamped.ID, stamped.Payload)
	}
	if got[0].Failures != 2 {
		t.Errorf("Failures = %d, want 2", got[0].Failures)
	}
	if !got[0].AddedAt.Equal(stamped.AddedAt) {
		t.Errorf("AddedAt = %v, want %v", got[0].AddedAt, stamped.AddedAt)
	}
	if got[1].ID != empty.ID || len(got[1].Payload) != 0 {
		t.Errorf("second entry = %s %x, want an empty payload round tripped", got[1].ID, got[1].Payload)
	}
}

// TestPendingKeepsInsertionOrderOnAFrozenClock: the replay order is the
// insertion order, not the clock order, so one clock reading across every
// entry changes nothing.
func TestPendingKeepsInsertionOrderOnAFrozenClock(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "outbox.db"))

	var want []string
	for _, name := range []string{"one", "two", "three", "four"} {
		e := entry(name, []byte(name))
		if err := store.Add(t.Context(), e); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
		want = append(want, e.ID)
	}

	got, err := store.Pending(t.Context(), 1, -1)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Pending = %d entries, want %d", len(got), len(want))
	}
	for i, entryID := range want {
		if got[i].ID != entryID {
			t.Errorf("Pending[%d] = %s, want %s", i, got[i].ID, entryID)
		}
	}
}

// TestPendingLimitBoundsTheRead: the limit rides in the SQL, so a bounded
// read returns the oldest matching rows and costs nothing like the backlog.
func TestPendingLimitBoundsTheRead(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "outbox.db"))

	var want []string
	for _, name := range []string{"one", "two", "three", "four"} {
		e := entry(name, []byte(name))
		if err := store.Add(t.Context(), e); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
		want = append(want, e.ID)
	}

	got, err := store.Pending(t.Context(), 1, 2)
	if err != nil {
		t.Fatalf("Pending with limit 2: %v", err)
	}
	if len(got) != 2 || got[0].ID != want[0] || got[1].ID != want[1] {
		t.Fatalf("Pending(1, 2) = %+v, want the two oldest entries", got)
	}
	none, err := store.Pending(t.Context(), 1, 0)
	if err != nil {
		t.Fatalf("Pending with limit 0: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("Pending(1, 0) = %+v, want nothing", none)
	}
	all, err := store.Pending(t.Context(), 1, -1)
	if err != nil {
		t.Fatalf("Pending with a negative limit: %v", err)
	}
	if len(all) != len(want) {
		t.Fatalf("Pending(1, -1) = %d entries, want every matching one", len(all))
	}
}

// TestPendingAndExhaustedSplitOnTheThreshold: the two reads partition the
// owed entries at the failure count the caller names.
func TestPendingAndExhaustedSplitOnTheThreshold(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "outbox.db"))
	fresh := entry("fresh", []byte("a"))
	once := entry("once", []byte("b"))
	if err := store.Add(t.Context(), fresh); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := store.Add(t.Context(), once); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := store.RecordFailures(t.Context(), []string{once.ID}); err != nil {
		t.Fatalf("RecordFailures: %v", err)
	}

	live, err := store.Pending(t.Context(), 1, -1)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(live) != 1 || live[0].ID != fresh.ID {
		t.Errorf("Pending(1) = %+v, want only the untouched entry", live)
	}
	spent, err := store.Exhausted(t.Context(), 1)
	if err != nil {
		t.Fatalf("Exhausted: %v", err)
	}
	if len(spent) != 1 || spent[0].ID != once.ID || spent[0].Failures != 1 {
		t.Errorf("Exhausted(1) = %+v, want the entry with one failure", spent)
	}

	all, err := store.Pending(t.Context(), 2, -1)
	if err != nil {
		t.Fatalf("Pending(2): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("Pending(2) = %d entries, want both, because neither reached the cap", len(all))
	}
}

// TestDeliveredRemovesWholeBatch: the batch retires whole, an id with no row
// is not an error, and a retired entry leaves the pending read.
func TestDeliveredRemovesWholeBatch(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "outbox.db"))
	var ids []string
	for _, name := range []string{"one", "two", "three"} {
		e := entry(name, []byte(name))
		if err := store.Add(t.Context(), e); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
		ids = append(ids, e.ID)
	}

	if err := store.Delivered(t.Context(), []string{ids[0], ids[2], "id-never-stored"}); err != nil {
		t.Fatalf("Delivered: %v", err)
	}
	got, err := store.Pending(t.Context(), 1, -1)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(got) != 1 || got[0].ID != ids[1] {
		t.Fatalf("Pending = %+v, want only the middle entry", got)
	}

	if err := store.Delivered(t.Context(), []string{ids[0]}); err != nil {
		t.Errorf("Delivered again on a retired id: %v, want no error", err)
	}
}

// TestRecordFailuresBumpsOncePerCall: each call adds one to the named
// entries and touches no others.
func TestRecordFailuresBumpsOncePerCall(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "outbox.db"))
	counted := entry("counted", []byte("a"))
	quiet := entry("quiet", []byte("b"))
	if err := store.Add(t.Context(), counted); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := store.Add(t.Context(), quiet); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for want := 1; want <= 3; want++ {
		if err := store.RecordFailures(t.Context(), []string{counted.ID}); err != nil {
			t.Fatalf("RecordFailures: %v", err)
		}
		got, err := store.Exhausted(t.Context(), want)
		if err != nil {
			t.Fatalf("Exhausted(%d): %v", want, err)
		}
		if len(got) != 1 || got[0].ID != counted.ID || got[0].Failures != want {
			t.Fatalf("Exhausted(%d) = %+v, want the counted entry at %d failures", want, got, want)
		}
	}
	untouched, err := store.Pending(t.Context(), 1, -1)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(untouched) != 1 || untouched[0].ID != quiet.ID || untouched[0].Failures != 0 {
		t.Errorf("Pending(1) = %+v, want the quiet entry untouched at zero", untouched)
	}
}

// The environment the parent passes to the child it launches.
const (
	crashChildEnv = "KEEL_OUTBOX_CRASH_CHILD"
	crashDirEnv   = "KEEL_OUTBOX_CRASH_DIR"
	crashRunName  = "TestCrashHelperProcess"
)

// nopSink is the sink the crash child opens with, because the child adds
// entries and dies before any pass could deliver them.
type nopSink struct{}

// Deliver accepts the batch and does nothing with it.
func (nopSink) Deliver(ctx context.Context, batch []outbox.Entry) error { return nil }

// TestCrashHelperProcess is the child half of the crash harness. A plain go
// test run skips it, because only the parent sets the child variable.
func TestCrashHelperProcess(t *testing.T) {
	if os.Getenv(crashChildEnv) == "" {
		t.Skip("child of the outbox crash harness")
	}
	dir := os.Getenv(crashDirEnv)
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "outbox.db")})
	if err != nil {
		t.Fatalf("child sqlite.Open: %v", err)
	}
	store, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: db})
	if err != nil {
		t.Fatalf("child sqlitestore.Open: %v", err)
	}
	box, err := outbox.Open(ctx, outbox.Config{Store: store, Sink: nopSink{}})
	if err != nil {
		t.Fatalf("child outbox.Open: %v", err)
	}

	var ids []string
	for _, payload := range []string{"alpha", "beta", "gamma"} {
		e, err := box.Add(ctx, []byte(payload))
		if err != nil {
			t.Fatalf("child Add %s: %v", payload, err)
		}
		ids = append(ids, e.ID)
	}
	// The ids leave through a file, so the parent compares them against a
	// record the dead process wrote outside the database it is re-read
	// from.
	if err := os.WriteFile(filepath.Join(dir, "ids.txt"), []byte(strings.Join(ids, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("child record ids: %v", err)
	}
	// The exit path: this process dies here with its handles open, so no
	// close runs, no checkpoint runs, and the WAL keeps the committed
	// frames for the parent's reopen to recover.
	os.Exit(3)
}

// TestEntriesSurviveAKilledProcess kills a process right after it added
// entries and reads them back in this one. The child never closes its
// handles, so the committed frames are still in the WAL, and the reopen
// recovers them the way a real crash recovery runs. A polite close would
// have checkpointed the WAL away, which is a weaker end state.
func TestEntriesSurviveAKilledProcess(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "outbox.db")
	idsPath := filepath.Join(dir, "ids.txt")

	child := exec.Command(os.Args[0], "-test.run="+crashRunName)
	child.Env = append(os.Environ(), crashChildEnv+"=1", crashDirEnv+"="+dir)
	var childLog strings.Builder
	child.Stdout, child.Stderr = &childLog, &childLog
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	// The child exits by itself, so the wait below reaps it with the wanted
	// exit status. The kill is the watchdog, because a hung child would
	// otherwise hang the suite.
	waitCh := make(chan error, 1)
	go func() { waitCh <- child.Wait() }()
	select {
	case err := <-waitCh:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 3 {
			t.Fatalf("child exit = %v, want status 3. Output:\n%s", err, childLog.String())
		}
	case <-time.After(30 * time.Second):
		_ = child.Process.Kill() // the watchdog fired, so the child only needs reaping
		<-waitCh
		t.Fatalf("child hung. Output:\n%s", childLog.String())
	}

	// The WAL is the crash residue: a process that closed politely would
	// have checkpointed it away, and a journal that never leaves a WAL
	// behind cannot hold this package's durability promise.
	wal, err := os.Stat(dbPath + "-wal")
	if err != nil {
		t.Fatalf("stat the WAL the dead child left: %v", err)
	}
	if wal.Size() == 0 {
		t.Fatalf("the WAL the child left is empty, want the committed frames in it")
	}

	data, err := os.ReadFile(idsPath)
	if err != nil {
		t.Fatalf("read the ids the child recorded: %v", err)
	}
	want := strings.Split(strings.TrimSpace(string(data)), "\n")

	reopened := openStore(t, dbPath)
	got, err := reopened.Pending(t.Context(), 1, -1)
	if err != nil {
		t.Fatalf("Pending after the crash: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Pending = %d entries, want the %d the dead process stored", len(got), len(want))
	}
	for i, entryID := range want {
		if got[i].ID != entryID {
			t.Errorf("entry %d ID = %s, want the stable %s", i, got[i].ID, entryID)
		}
		if got[i].Failures != 0 {
			t.Errorf("entry %d Failures = %d, want 0, because the crash spent no attempt", i, got[i].Failures)
		}
		if wantPayload := []byte([]string{"alpha", "beta", "gamma"}[i]); !bytes.Equal(got[i].Payload, wantPayload) {
			t.Errorf("entry %d payload = %q, want %q", i, got[i].Payload, wantPayload)
		}
	}
}

// TestFreshConnectionPinsRows: a plain SQLite connection the store never
// holds sees exactly the rows Add wrote, in insertion order.
func TestFreshConnectionPinsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.db")
	store := openStore(t, path)

	var want []outbox.Entry
	for i, name := range []string{"one", "two", "three"} {
		e := entry(name, []byte(name))
		e.Failures = i
		e.AddedAt = base.Add(time.Duration(i) * time.Second)
		if err := store.Add(t.Context(), e); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
		want = append(want, e)
	}

	rows, err := openFresh(t, path).Query(
		`SELECT id, payload, failures, added_at FROM outbox_entry ORDER BY seq`)
	if err != nil {
		t.Fatalf("fresh query: %v", err)
	}
	defer rows.Close()
	var got []outbox.Entry
	for rows.Next() {
		var (
			entryID  string
			payload  []byte
			failures int
			addedAt  int64
		)
		if err := rows.Scan(&entryID, &payload, &failures, &addedAt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, outbox.Entry{ID: entryID, Payload: payload, Failures: failures, AddedAt: time.Unix(0, addedAt)})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("fresh connection read %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID || !bytes.Equal(got[i].Payload, want[i].Payload) {
			t.Errorf("row %d = %s %x, want %s %x", i, got[i].ID, got[i].Payload, want[i].ID, want[i].Payload)
		}
		if got[i].Failures != want[i].Failures {
			t.Errorf("row %d failures = %d, want %d", i, got[i].Failures, want[i].Failures)
		}
		if !got[i].AddedAt.Equal(want[i].AddedAt) {
			t.Errorf("row %d added_at = %v, want %v", i, got[i].AddedAt, want[i].AddedAt)
		}
	}
}

// TestConcurrentAdds: one write connection takes concurrent adds without a
// lost entry, which is what a running loop and its callers produce.
func TestConcurrentAdds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.db")
	store := openStore(t, path)

	const writers = 8
	const perWriter = 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				e := entry(fmt.Sprintf("w%d-e%d", w, i), []byte("payload"))
				if err := store.Add(t.Context(), e); err != nil {
					t.Errorf("Add: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	var n int
	if err := openFresh(t, path).QueryRow(`SELECT COUNT(*) FROM outbox_entry`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != writers*perWriter {
		t.Errorf("stored rows = %d, want %d", n, writers*perWriter)
	}
}
