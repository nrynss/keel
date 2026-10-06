package sqlitestore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nrynss/keel/cache"
	"github.com/nrynss/keel/cache/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// base is the instant the tests stamp entries with. It carries a non-zero
// nanosecond part, so a store that lost precision would be caught.
var base = time.Date(2026, 1, 1, 8, 0, 0, 123456789, time.UTC)

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

// openStoreAt applies the schema at path and returns the store on the clock
// the caller passes.
func openStoreAt(t *testing.T, path string, now func() time.Time) *sqlitestore.Store {
	t.Helper()
	store, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: openDB(t, path), Now: now})
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

// sampleEntry is one stored outcome with bytes a text column would mangle.
func sampleEntry() cache.Entry {
	return cache.Entry{
		Payload:     []byte{'j', 0x00, 's', 0xff, 'n'},
		ContentType: "application/json",
		ChargeRef:   "charge-7",
		BlobID:      "blob-0000000000000000000000000000aa11",
		CreatedAt:   base,
	}
}

// sampleRefusal is one stored refusal.
func sampleRefusal() cache.Entry {
	return cache.Entry{
		Payload:     []byte("no face in the input"),
		ContentType: "text/plain",
		Refusal:     true,
		CreatedAt:   base,
	}
}

// fakeClock is the injected clock the tests advance by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock(at time.Time) *fakeClock { return &fakeClock{now: at} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestOpenRejectsNilDB: a store with no database refuses to open rather than
// panicking on the first write.
func TestOpenRejectsNilDB(t *testing.T) {
	if _, err := sqlitestore.Open(t.Context(), sqlitestore.Config{}); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Errorf("err = %v, want errors.Is(.., ErrInvalid)", err)
	}
}

// TestOpenAppliesMigrationsOnce: a second open on the same file changes
// nothing in the ledger, and the row the first store wrote stays readable.
func TestOpenAppliesMigrationsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	first, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: openDB(t, path)})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	want := sampleEntry()
	if err := first.Put(t.Context(), "key-once", want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	second := openStoreAt(t, path, nil)
	got, err := second.Get(t.Context(), "key-once")
	if err != nil {
		t.Fatalf("Get through the second store: %v", err)
	}
	if string(got.Payload) != string(want.Payload) || got.ChargeRef != want.ChargeRef {
		t.Errorf("Get = %+v, want the row the first store wrote", got)
	}

	var applied int
	if err := openFresh(t, path).QueryRow("SELECT COUNT(*) FROM cache_schema_migrations").Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != 1 {
		t.Errorf("migration ledger holds %d rows, want 1", applied)
	}
}

// TestPutGetRoundTrip: every stored field comes back, including payload
// bytes a text column would mangle, the refusal mark, and a stamp to the
// nanosecond.
func TestPutGetRoundTrip(t *testing.T) {
	clk := newClock(base)
	store := openStoreAt(t, filepath.Join(t.TempDir(), "cache.db"), clk.Now)

	want := sampleEntry()
	if err := store.Put(t.Context(), "key-round", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(t.Context(), "key-round")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Payload) != string(want.Payload) {
		t.Errorf("payload %x, want %x", got.Payload, want.Payload)
	}
	if got.ContentType != want.ContentType || got.ChargeRef != want.ChargeRef || got.BlobID != want.BlobID {
		t.Errorf("metadata %+v, want %+v", got, want)
	}
	if got.Refusal {
		t.Error("a success entry reads back as a refusal")
	}
	if !got.CreatedAt.Equal(base) {
		t.Errorf("CreatedAt %v, want %v", got.CreatedAt, base)
	}

	refusal := sampleRefusal()
	if err := store.Put(t.Context(), "key-refusal", refusal); err != nil {
		t.Fatalf("Put refusal: %v", err)
	}
	got, err = store.Get(t.Context(), "key-refusal")
	if err != nil {
		t.Fatalf("Get refusal: %v", err)
	}
	if !got.Refusal {
		t.Error("a refusal entry reads back as a success")
	}
	if string(got.Payload) != string(refusal.Payload) {
		t.Errorf("refusal message %q, want %q", got.Payload, refusal.Payload)
	}
}

// TestPutStampsZeroCreatedAt: a row with no stored time takes the store
// clock, so a direct caller cannot store an entry time forgot.
func TestPutStampsZeroCreatedAt(t *testing.T) {
	clk := newClock(base)
	store := openStoreAt(t, filepath.Join(t.TempDir(), "cache.db"), clk.Now)

	entry := cache.Entry{Payload: []byte("doc"), ContentType: "application/json"}
	if err := store.Put(t.Context(), "key-stamped", entry); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(t.Context(), "key-stamped")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.CreatedAt.Equal(base) {
		t.Errorf("CreatedAt %v, want the store clock %v", got.CreatedAt, base)
	}
}

// TestGetMissingKeyWrapsErrNotFound: an absent key matches the one sentinel
// the cache package reads as a miss.
func TestGetMissingKeyWrapsErrNotFound(t *testing.T) {
	store := openStoreAt(t, filepath.Join(t.TempDir(), "cache.db"), nil)
	if _, err := store.Get(t.Context(), "key-absent"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("err = %v, want errors.Is(.., cache.ErrNotFound)", err)
	}
}

// TestPutReplacesRow: a refreshed entry takes the place of the one it
// replaces, key for key.
func TestPutReplacesRow(t *testing.T) {
	store := openStoreAt(t, filepath.Join(t.TempDir(), "cache.db"), nil)

	first := cache.Entry{Payload: []byte("older"), ContentType: "application/json", CreatedAt: base}
	if err := store.Put(t.Context(), "key-replace", first); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	second := cache.Entry{Payload: []byte("newer"), ContentType: "application/json", ChargeRef: "charge-8", CreatedAt: base.Add(time.Minute)}
	if err := store.Put(t.Context(), "key-replace", second); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	got, err := store.Get(t.Context(), "key-replace")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Payload) != "newer" || got.ChargeRef != "charge-8" || !got.CreatedAt.Equal(base.Add(time.Minute)) {
		t.Errorf("Get = %+v, want the second entry", got)
	}
}

// TestEntriesSurviveCloseAndReopen: the entry a paid make stored survives a
// close, and the reopened store serves it.
func TestEntriesSurviveCloseAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")

	firstDB := openDB(t, path)
	firstStore, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: firstDB})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	want := sampleEntry()
	if err := firstStore.Put(t.Context(), "key-reopen", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := firstDB.Close(); err != nil {
		t.Fatalf("close the first handle: %v", err)
	}

	secondStore := openStoreAt(t, path, nil)
	got, err := secondStore.Get(t.Context(), "key-reopen")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if string(got.Payload) != string(want.Payload) || got.ChargeRef != want.ChargeRef || !got.CreatedAt.Equal(base) {
		t.Errorf("Get = %+v, want the entry the first handle stored", got)
	}
}

// TestCacheSurvivesReopenThroughGetOrMake: the whole flow pays once across a
// restart. The maker runs on the first open, and the reopened cache serves
// the stored entry without a make.
func TestCacheSurvivesReopenThroughGetOrMake(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")

	type verdictKey struct {
		Level    string
		Title    string
		Checksum string
	}
	key, err := cache.Key(verdictKey{Level: "verdict", Title: "A policy", Checksum: "cc44"})
	if err != nil {
		t.Fatalf("Key: %v", err)
	}

	makes := 0
	verdict := func(ctx context.Context) (cache.Result, error) {
		makes++
		return cache.Result{Payload: []byte(`{"verdict":"clean"}`), ContentType: "application/json", ChargeRef: "charge-9"}, nil
	}

	firstCache, err := cache.New(cache.Config{Store: openStoreAt(t, path, nil)})
	if err != nil {
		t.Fatalf("first cache.New: %v", err)
	}
	first, err := firstCache.GetOrMake(t.Context(), key, verdict)
	if err != nil {
		t.Fatalf("first GetOrMake: %v", err)
	}

	secondCache, err := cache.New(cache.Config{Store: openStoreAt(t, path, nil)})
	if err != nil {
		t.Fatalf("second cache.New: %v", err)
	}
	second, err := secondCache.GetOrMake(t.Context(), key, verdict)
	if err != nil {
		t.Fatalf("second GetOrMake: %v", err)
	}
	if makes != 1 {
		t.Errorf("maker ran %d times, want 1 across the reopen", makes)
	}
	if string(second.Payload) != string(first.Payload) || second.ChargeRef != first.ChargeRef {
		t.Errorf("hit = %+v, want the stored %+v", second, first)
	}
}

// TestSweepExpiresEntriesAndRefusals: each class goes at its own age, and an
// entry exactly at its age is old enough to go.
func TestSweepExpiresEntriesAndRefusals(t *testing.T) {
	clk := newClock(base)
	store := openStoreAt(t, filepath.Join(t.TempDir(), "cache.db"), clk.Now)
	ctx := t.Context()

	if err := store.Put(ctx, "key-expire", sampleEntry()); err != nil {
		t.Fatalf("Put entry: %v", err)
	}
	if err := store.Put(ctx, "key-refuse", sampleRefusal()); err != nil {
		t.Fatalf("Put refusal: %v", err)
	}

	// MaxAge one hour and RefusalTTL half an hour, the way a caller
	// passes the fields its cache Config resolved.
	cfg := sqlitestore.SweepConfig{MaxAge: time.Hour, RefusalTTL: 30 * time.Minute}

	clk.Advance(29 * time.Minute)
	got, err := store.Sweep(ctx, cfg)
	if err != nil {
		t.Fatalf("Sweep inside both lifetimes: %v", err)
	}
	if got.ExpiredDeleted != 0 || got.RefusalsDeleted != 0 || got.OrphansDeleted != 0 {
		t.Errorf("inside both lifetimes the sweep deleted %+v, want nothing", got)
	}

	clk.Advance(time.Minute) // the refusal reaches its half hour first
	got, err = store.Sweep(ctx, cfg)
	if err != nil {
		t.Fatalf("Sweep at the refusal TTL: %v", err)
	}
	if got.RefusalsDeleted != 1 || got.ExpiredDeleted != 0 {
		t.Errorf("at the refusal TTL the sweep deleted %+v, want the refusal only", got)
	}
	if _, err := store.Get(ctx, "key-refuse"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("the swept refusal is still readable: %v", err)
	}
	if _, err := store.Get(ctx, "key-expire"); err != nil {
		t.Errorf("the entry went before its age: %v", err)
	}

	clk.Advance(30 * time.Minute) // the entry reaches its hour
	got, err = store.Sweep(ctx, cfg)
	if err != nil {
		t.Fatalf("Sweep at the entry age: %v", err)
	}
	if got.ExpiredDeleted != 1 || got.RefusalsDeleted != 0 {
		t.Errorf("at the entry age the sweep deleted %+v, want the entry only", got)
	}
	if _, err := store.Get(ctx, "key-expire"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("the swept entry is still readable: %v", err)
	}
}

// TestSweepRemovesOrphans: a row whose blob no longer resolves leaves the
// index, and a row whose blob is still there stays.
func TestSweepRemovesOrphans(t *testing.T) {
	store := openStoreAt(t, filepath.Join(t.TempDir(), "cache.db"), nil)
	ctx := t.Context()

	dropped := sampleEntry()
	if err := store.Put(ctx, "key-dropped", dropped); err != nil {
		t.Fatalf("Put dropped: %v", err)
	}
	kept := sampleEntry()
	kept.BlobID = "blob-0000000000000000000000000000bb22"
	if err := store.Put(ctx, "key-kept", kept); err != nil {
		t.Fatalf("Put kept: %v", err)
	}

	present := map[string]bool{
		dropped.BlobID: false, // the retention sweep already dropped this one
		kept.BlobID:    true,
	}
	cfg := sqlitestore.SweepConfig{
		Present: func(ctx context.Context, blobID string) (bool, error) {
			ok, seen := present[blobID]
			if !seen {
				return false, errors.New("blob id the test never heard of")
			}
			return ok, nil
		},
	}
	got, err := store.Sweep(ctx, cfg)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got.OrphansDeleted != 1 || got.ExpiredDeleted != 0 || got.RefusalsDeleted != 0 {
		t.Errorf("the sweep deleted %+v, want the one orphan", got)
	}
	if _, err := store.Get(ctx, "key-dropped"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("the orphaned entry is still readable: %v", err)
	}
	if _, err := store.Get(ctx, "key-kept"); err != nil {
		t.Errorf("the entry whose blob is kept went: %v", err)
	}
}

// TestSweepReportsAPresentFault: a presence check that fails ends the pass,
// because a row it could not judge is a row the sweep cannot vouch for.
func TestSweepReportsAPresentFault(t *testing.T) {
	store := openStoreAt(t, filepath.Join(t.TempDir(), "cache.db"), nil)
	blast := errors.New("index is closed")
	cfg := sqlitestore.SweepConfig{
		Present: func(ctx context.Context, blobID string) (bool, error) { return false, blast },
	}
	if err := store.Put(t.Context(), "key-fault", sampleEntry()); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := store.Sweep(t.Context(), cfg); !errors.Is(err, blast) {
		t.Errorf("err = %v, want the present fault", err)
	}
}

// TestSweepSkipsClassesWithZeroAges: a zero age skips its class, so a caller
// that passes its cache Config sweeps exactly what that Config expires.
func TestSweepSkipsClassesWithZeroAges(t *testing.T) {
	clk := newClock(base)
	store := openStoreAt(t, filepath.Join(t.TempDir(), "cache.db"), clk.Now)
	ctx := t.Context()

	if err := store.Put(ctx, "key-old", sampleEntry()); err != nil {
		t.Fatalf("Put entry: %v", err)
	}
	if err := store.Put(ctx, "key-old-refusal", sampleRefusal()); err != nil {
		t.Fatalf("Put refusal: %v", err)
	}
	clk.Advance(48 * time.Hour)

	got, err := store.Sweep(ctx, sqlitestore.SweepConfig{})
	if err != nil {
		t.Fatalf("Sweep with no ages: %v", err)
	}
	if got.ExpiredDeleted != 0 || got.RefusalsDeleted != 0 || got.OrphansDeleted != 0 {
		t.Errorf("a sweep with no ages deleted %+v, want nothing", got)
	}
	if _, err := store.Get(ctx, "key-old"); err != nil {
		t.Errorf("the entry went without a MaxAge: %v", err)
	}
	if _, err := store.Get(ctx, "key-old-refusal"); err != nil {
		t.Errorf("the refusal went without a RefusalTTL: %v", err)
	}
}

// TestSweepRefusesNegativeAges: a config the sweep cannot honour refuses
// before any row is touched.
func TestSweepRefusesNegativeAges(t *testing.T) {
	store := openStoreAt(t, filepath.Join(t.TempDir(), "cache.db"), nil)
	if _, err := store.Sweep(t.Context(), sqlitestore.SweepConfig{MaxAge: -time.Minute}); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Errorf("MaxAge -time.Minute: err = %v, want errors.Is(.., ErrInvalid)", err)
	}
	if _, err := store.Sweep(t.Context(), sqlitestore.SweepConfig{RefusalTTL: -time.Minute}); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Errorf("RefusalTTL -time.Minute: err = %v, want errors.Is(.., ErrInvalid)", err)
	}
}

// TestTwoNamespacesOwnTwoTables: two cache levels share one database file as
// two isolated tables, which is the promise Config.Namespace makes. Each
// namespace owns its ledger, its table and its sweeps.
func TestTwoNamespacesOwnTwoTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	shared := openDB(t, path)
	ctx := t.Context()

	defaults, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: shared})
	if err != nil {
		t.Fatalf("open the default namespace: %v", err)
	}
	verdicts, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: shared, Namespace: "verdicts"})
	if err != nil {
		t.Fatalf("open a second namespace on the same file: %v", err)
	}

	first := cache.Entry{Payload: []byte("first"), ContentType: "application/json", CreatedAt: base}
	if err := defaults.Put(ctx, "key-level", first); err != nil {
		t.Fatalf("Put through the default namespace: %v", err)
	}
	second := cache.Entry{Payload: []byte("second"), ContentType: "application/json", ChargeRef: "charge-11", CreatedAt: base.Add(time.Minute)}
	if err := verdicts.Put(ctx, "key-level", second); err != nil {
		t.Fatalf("Put through the second namespace: %v", err)
	}

	gotFirst, err := defaults.Get(ctx, "key-level")
	if err != nil {
		t.Fatalf("Get through the default namespace: %v", err)
	}
	if string(gotFirst.Payload) != "first" {
		t.Errorf("default namespace payload %q, want first", gotFirst.Payload)
	}
	gotSecond, err := verdicts.Get(ctx, "key-level")
	if err != nil {
		t.Fatalf("Get through the second namespace: %v", err)
	}
	if string(gotSecond.Payload) != "second" || gotSecond.ChargeRef != "charge-11" {
		t.Errorf("second namespace Get = %+v, want its own row", gotSecond)
	}

	// Each namespace owns its ledger and its table.
	fresh := openFresh(t, path)
	var applied int
	if err := fresh.QueryRow("SELECT COUNT(*) FROM cache_schema_migrations").Scan(&applied); err != nil {
		t.Fatalf("count the default ledger rows: %v", err)
	}
	if applied != 1 {
		t.Errorf("default ledger holds %d rows, want 1", applied)
	}
	if err := fresh.QueryRow("SELECT COUNT(*) FROM verdicts_schema_migrations").Scan(&applied); err != nil {
		t.Fatalf("count the second ledger rows: %v", err)
	}
	if applied != 1 {
		t.Errorf("second ledger holds %d rows, want 1", applied)
	}
	var payload string
	if err := fresh.QueryRow("SELECT payload FROM cache_entry WHERE key = ?", "key-level").Scan(&payload); err != nil {
		t.Fatalf("read the default table: %v", err)
	}
	if payload != "first" {
		t.Errorf("default table payload %q, want first", payload)
	}
	if err := fresh.QueryRow("SELECT payload FROM verdicts_entry WHERE key = ?", "key-level").Scan(&payload); err != nil {
		t.Fatalf("read the second table: %v", err)
	}
	if payload != "second" {
		t.Errorf("second table payload %q, want second", payload)
	}

	// A sweep in one namespace leaves the other alone. The second store
	// runs on the real clock, so its old row is past any age.
	result, err := verdicts.Sweep(ctx, sqlitestore.SweepConfig{MaxAge: time.Hour, RefusalTTL: time.Hour})
	if err != nil {
		t.Fatalf("Sweep through the second namespace: %v", err)
	}
	if result.ExpiredDeleted != 1 {
		t.Errorf("the second namespace sweep deleted %d rows, want its own 1", result.ExpiredDeleted)
	}
	if _, err := defaults.Get(ctx, "key-level"); err != nil {
		t.Errorf("the second namespace sweep touched the default table: %v", err)
	}
}

// TestNamespaceCaseIsCanonical: a namespace reads as lowercase, because
// SQLite folds table names that differ only in ASCII case into one. A case
// twin of an open namespace is that namespace, never a silent second one,
// and the ledger and table carry the lowercase names.
func TestNamespaceCaseIsCanonical(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	ctx := t.Context()

	// A fresh file opened with a mixed-case namespace owns the lowercase
	// ledger and table.
	mixed, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: openDB(t, path), Namespace: "Cache"})
	if err != nil {
		t.Fatalf("open with a mixed-case namespace: %v", err)
	}
	fresh := openFresh(t, path)
	var tables int
	if err := fresh.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'cache_entry'`,
	).Scan(&tables); err != nil {
		t.Fatalf("count cache_entry: %v", err)
	}
	if tables != 1 {
		t.Errorf("sqlite_master holds %d cache_entry tables, want 1", tables)
	}
	if err := fresh.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'Cache_entry'`,
	).Scan(&tables); err != nil {
		t.Fatalf("count Cache_entry: %v", err)
	}
	if tables != 0 {
		t.Errorf("sqlite_master holds %d Cache_entry tables, want 0, because the name reads as lowercase", tables)
	}

	// The case twin of an open namespace is that namespace. The row one
	// store writes, the other reads, and no second pair of tables
	// appears.
	lower, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: openDB(t, path)})
	if err != nil {
		t.Fatalf("open the lowercase twin: %v", err)
	}
	entry := cache.Entry{Payload: []byte("twin"), ContentType: "application/json", CreatedAt: base}
	if err := lower.Put(ctx, "key-case", entry); err != nil {
		t.Fatalf("Put through the lowercase store: %v", err)
	}
	got, err := mixed.Get(ctx, "key-case")
	if err != nil {
		t.Fatalf("Get through the mixed-case store: %v", err)
	}
	if string(got.Payload) != "twin" {
		t.Errorf("the case twin read %q, want the row its twin wrote", got.Payload)
	}
	if err := fresh.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name LIKE '%cache%'`,
	).Scan(&tables); err != nil {
		t.Fatalf("count the cache tables: %v", err)
	}
	if tables != 2 {
		t.Errorf("sqlite_master holds %d tables named like cache, want one entry table and one ledger", tables)
	}
}

// TestOrphanSweepSparesAFreshEntry: an entry a concurrent make stores while
// the sweep holds its snapshot survives the pass, and the next caller is
// served instead of paying again.
func TestOrphanSweepSparesAFreshEntry(t *testing.T) {
	clk := newClock(base)
	store := openStoreAt(t, filepath.Join(t.TempDir(), "cache.db"), clk.Now)
	ctx := t.Context()

	key := "key-race"
	// The candidate the sweep snapshots names a blob the retention sweep
	// has already dropped.
	stale := cache.Entry{
		Payload:     []byte("stale"),
		ContentType: "application/json",
		BlobID:      "blob-old",
		CreatedAt:   base,
	}
	if err := store.Put(ctx, key, stale); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	c, err := cache.New(cache.Config{Store: store, MaxAge: time.Hour, Now: clk.Now})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	var makes atomic.Int64
	maker := func(ctx context.Context) (cache.Result, error) {
		makes.Add(1)
		return cache.Result{Payload: []byte("fresh"), ContentType: "application/json", ChargeRef: "charge-12", BlobID: "blob-fresh"}, nil
	}

	// Present runs while the pass holds its snapshot. The make it runs
	// stores a fresh row for the same key before the pass reaches its
	// delete, which is exactly the window the pin covers.
	present := func(ctx context.Context, blobID string) (bool, error) {
		if blobID != "blob-old" {
			return false, fmt.Errorf("present saw unexpected blob %s", blobID)
		}
		clk.Advance(2 * time.Hour) // past MaxAge, so the make runs
		if _, err := c.GetOrMake(ctx, key, maker); err != nil {
			return false, fmt.Errorf("the concurrent make failed: %w", err)
		}
		return false, nil // the old blob is gone
	}

	type swept struct {
		result sqlitestore.SweepResult
		err    error
	}
	done := make(chan swept, 1)
	go func() {
		result, err := store.Sweep(ctx, sqlitestore.SweepConfig{Present: present})
		done <- swept{result: result, err: err}
	}()
	out := <-done
	if out.err != nil {
		t.Fatalf("Sweep: %v", out.err)
	}
	if out.result.OrphansDeleted != 0 {
		t.Errorf("the sweep deleted %d rows, want 0, because the snapshotted row was already replaced", out.result.OrphansDeleted)
	}

	// The fresh entry survived the pass.
	got, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("the fresh entry did not survive the sweep: %v", err)
	}
	if string(got.Payload) != "fresh" || got.BlobID != "blob-fresh" || got.ChargeRef != "charge-12" {
		t.Errorf("Get = %+v, want the fresh entry", got)
	}

	// The next caller is served, not charged again.
	again, err := c.GetOrMake(ctx, key, maker)
	if err != nil {
		t.Fatalf("GetOrMake after the sweep: %v", err)
	}
	if string(again.Payload) != "fresh" {
		t.Errorf("payload %q, want the stored fresh entry", again.Payload)
	}
	if makes.Load() != 1 {
		t.Errorf("maker ran %d times, want 1, so the sweep cost no second make", makes.Load())
	}
}

// TestRowsAreReadableThroughAFreshConnection: the row an ordinary Put wrote
// is exactly what an independent SQL reader sees, column for column.
func TestRowsAreReadableThroughAFreshConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	store := openStoreAt(t, path, nil)

	want := sampleEntry()
	if err := store.Put(t.Context(), "key-raw", want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var (
		payload     []byte
		contentType string
		chargeRef   string
		blobID      string
		refusal     int
		createdAt   int64
	)
	err := openFresh(t, path).QueryRow(
		`SELECT payload, content_type, charge_ref, blob_id, refusal, created_at
		 FROM cache_entry WHERE key = ?`, "key-raw",
	).Scan(&payload, &contentType, &chargeRef, &blobID, &refusal, &createdAt)
	if err != nil {
		t.Fatalf("fresh read: %v", err)
	}
	if string(payload) != string(want.Payload) {
		t.Errorf("payload %x, want %x", payload, want.Payload)
	}
	if contentType != want.ContentType || chargeRef != want.ChargeRef || blobID != want.BlobID {
		t.Errorf("columns %q %q %q, want %q %q %q", contentType, chargeRef, blobID, want.ContentType, want.ChargeRef, want.BlobID)
	}
	if refusal != 0 {
		t.Errorf("refusal flag %d, want 0", refusal)
	}
	if createdAt != want.CreatedAt.UnixNano() {
		t.Errorf("created_at %d, want %d", createdAt, want.CreatedAt.UnixNano())
	}
}
