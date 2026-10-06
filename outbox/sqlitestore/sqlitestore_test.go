package sqlitestore_test

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
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
	got, err := second.Pending(t.Context(), 1)
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

	got, err := store.Pending(t.Context(), 3)
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

	got, err := store.Pending(t.Context(), 1)
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

	live, err := store.Pending(t.Context(), 1)
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

	all, err := store.Pending(t.Context(), 2)
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
	got, err := store.Pending(t.Context(), 1)
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
	untouched, err := store.Pending(t.Context(), 1)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(untouched) != 1 || untouched[0].ID != quiet.ID || untouched[0].Failures != 0 {
		t.Errorf("Pending(1) = %+v, want the quiet entry untouched at zero", untouched)
	}
}

// TestEntriesSurviveReopen: a store closed the way a dying process leaves
// one keeps every entry, in order, for the next open.
func TestEntriesSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.db")
	db := openDB(t, path)
	first, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: db})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}

	var want []outbox.Entry
	for _, name := range []string{"one", "two", "three"} {
		e := entry(name, []byte("payload-of-"+name))
		if err := first.Add(t.Context(), e); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
		want = append(want, e)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the first process: %v", err)
	}

	reopened := openStore(t, path)
	got, err := reopened.Pending(t.Context(), 1)
	if err != nil {
		t.Fatalf("Pending after reopen: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Pending = %d entries, want the %d the first process stored", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID || !bytes.Equal(got[i].Payload, want[i].Payload) || !got[i].AddedAt.Equal(want[i].AddedAt) {
			t.Errorf("entry %d = %s %x %v, want %s %x %v", i,
				got[i].ID, got[i].Payload, got[i].AddedAt,
				want[i].ID, want[i].Payload, want[i].AddedAt)
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
