package mediastore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nrynss/keel/id"
)

// base is the instant every retention test measures against. Rows are
// stamped from it directly and the sweeper is given a clock offset from
// it, so an age is arithmetic rather than elapsed test time.
var base = time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

// openTestStoreAt builds a store whose clock is fixed at at, so a test
// can stamp a row's age exactly and the store never drifts away from
// it.
func openTestStoreAt(t *testing.T, at time.Time) *Store {
	t.Helper()
	return openTestStoreWith(t, func(c *Config) {
		c.Now = func() time.Time { return at }
	})
}

// fixedClock returns a clock that always reads at.
func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// newSweeper builds the sweeper or fails the test.
func newSweeper(t *testing.T, s *Store, cfg RetentionConfig) *Sweeper {
	t.Helper()
	w, err := s.NewSweeper(cfg)
	if err != nil {
		t.Fatalf("new sweeper: %v", err)
	}
	return w
}

// newID returns a fresh id, which is the only shape of name the sweep
// considers its own.
func newID(t *testing.T) string {
	t.Helper()
	blobID, err := id.New()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	return blobID
}

// place writes the file and inserts the row, which is the state a
// healthy blob is in. The row's size is taken from the bytes so the two
// cannot disagree.
func place(t *testing.T, s *Store, b Blob, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, b.ID), data, 0o644); err != nil {
		t.Fatalf("write blob %s: %v", b.ID, err)
	}
	b.SizeBytes = int64(len(data))
	if err := s.index.Create(t.Context(), b); err != nil {
		t.Fatalf("create row %s: %v", b.ID, err)
	}
}

// orphan writes a file with no row at all, which is what a crash
// between a delete's two halves leaves behind, and backdates it so its
// age is known.
func orphan(t *testing.T, s *Store, data []byte, modTime time.Time) string {
	t.Helper()
	blobID := newID(t)
	path := filepath.Join(s.dir, blobID)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write orphan: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("backdate orphan: %v", err)
	}
	return blobID
}

// placeGroup inserts count blobs of size bytes each, all named by
// groupID and all stamped at createdAt. One group's age is the age of
// its earliest blob, so a shared timestamp is how a test sets it.
func placeGroup(t *testing.T, s *Store, groupID string, createdAt time.Time, count, size int) []string {
	t.Helper()
	ids := make([]string, 0, count)
	for range count {
		blobID := newID(t)
		place(t, s, Blob{
			ID:          blobID,
			Group:       groupID,
			ContentType: "image/png",
			CreatedAt:   createdAt,
		}, blob(size))
		ids = append(ids, blobID)
	}
	return ids
}

// fileExists reports whether the blob file is still on disk.
func fileExists(t *testing.T, s *Store, blobID string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(s.dir, blobID))
	return err == nil
}

// rowExists reports whether the index still holds the row.
func rowExists(t *testing.T, s *Store, blobID string) bool {
	t.Helper()
	_, err := s.index.Get(t.Context(), blobID)
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrNotFound):
		return false
	default:
		t.Fatalf("rowExists: %v", err)
		return false
	}
}

// requireAbsent fails unless both the row and the file are gone, which
// is the only honest form of "this blob was removed".
func requireAbsent(t *testing.T, s *Store, blobID, what string) {
	t.Helper()
	if rowExists(t, s, blobID) {
		t.Fatalf("%s: row for %s survived", what, blobID)
	}
	if fileExists(t, s, blobID) {
		t.Fatalf("%s: file for %s survived", what, blobID)
	}
}

// requirePresent fails unless both the row and the file are still
// there.
func requirePresent(t *testing.T, s *Store, blobID, what string) {
	t.Helper()
	if !rowExists(t, s, blobID) {
		t.Fatalf("%s: row for %s went missing", what, blobID)
	}
	if !fileExists(t, s, blobID) {
		t.Fatalf("%s: file for %s went missing", what, blobID)
	}
}

// TestSweepDeletesAgedUnplacedAndKeepsPlaced separates the two kinds of
// row: one no group owns, which nothing else will ever collect, and one
// a group owns, which the group's own lifecycle collects.
func TestSweepDeletesAgedUnplacedAndKeepsPlaced(t *testing.T) {
	s := openTestStoreAt(t, base)
	unplaced := newID(t)
	place(t, s, Blob{ID: unplaced, ContentType: "audio/mpeg", CreatedAt: base}, blob(120))
	placed := placeGroup(t, s, "g1", base, 1, 80)[0]

	w := newSweeper(t, s, RetentionConfig{Now: fixedClock(base.Add(defaultUnplacedAge + time.Minute))})
	result, err := w.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if result.UnplacedDeleted != 1 || result.UnplacedBytes != 120 {
		t.Fatalf("unplaced = %d, %d bytes, want 1, 120", result.UnplacedDeleted, result.UnplacedBytes)
	}
	if result.OrphanFilesDeleted != 0 || result.GroupsEvicted != 0 || result.Retained != 0 {
		t.Fatalf("unexpected other work: %+v", result)
	}
	if result.BytesBefore != 200 || result.BytesAfter != 80 {
		t.Fatalf("bytes before/after = %d/%d, want 200/80", result.BytesBefore, result.BytesAfter)
	}
	requireAbsent(t, s, unplaced, "aged unplaced")
	requirePresent(t, s, placed, "placed")
}

// TestSweepKeepsYoungUnplacedRows pins the age gate: an unplaced row
// inside the window is a persist still in flight, not a leftover.
func TestSweepKeepsYoungUnplacedRows(t *testing.T) {
	s := openTestStoreAt(t, base)
	blobID := newID(t)
	place(t, s, Blob{ID: blobID, ContentType: "audio/mpeg", CreatedAt: base}, blob(50))

	w := newSweeper(t, s, RetentionConfig{Now: fixedClock(base.Add(time.Minute))})
	result, err := w.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if result.UnplacedDeleted != 0 {
		t.Fatalf("unplaced deleted = %d, want 0", result.UnplacedDeleted)
	}
	requirePresent(t, s, blobID, "young unplaced")
}

// TestSweepUnplacedAgeBoundary walks both sides of the default window so
// the comparison cannot be off by a factor of the window or inverted.
func TestSweepUnplacedAgeBoundary(t *testing.T) {
	cases := []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"just placed", time.Minute, true},
		{"one minute short", defaultUnplacedAge - time.Minute, true},
		{"one minute over", defaultUnplacedAge + time.Minute, false},
		{"three days old", 72 * time.Hour, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStoreAt(t, base)
			blobID := newID(t)
			place(t, s, Blob{ID: blobID, ContentType: "audio/mpeg", CreatedAt: base}, blob(10))

			w := newSweeper(t, s, RetentionConfig{Now: fixedClock(base.Add(tc.age))})
			if _, err := w.Sweep(t.Context()); err != nil {
				t.Fatalf("sweep: %v", err)
			}

			if got := rowExists(t, s, blobID); got != tc.want {
				t.Fatalf("row survived = %v, want %v at age %v", got, tc.want, tc.age)
			}
		})
	}
}

// TestSweepRetainVetoIsAbsolute: a consumer that still holds an id, such
// as the owner of a short-lived sample, outranks even a wildly
// misconfigured age.
func TestSweepRetainVetoIsAbsolute(t *testing.T) {
	s := openTestStoreAt(t, base)
	blobID := newID(t)
	place(t, s, Blob{ID: blobID, ContentType: "audio/mpeg", CreatedAt: base}, blob(30))

	w := newSweeper(t, s, RetentionConfig{
		Now:    fixedClock(base.Add(72 * time.Hour)),
		Retain: func(gotID string) bool { return gotID == blobID },
	})
	result, err := w.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if result.Retained != 1 || result.UnplacedDeleted != 0 {
		t.Fatalf("retained = %d, unplaced = %d, want 1, 0", result.Retained, result.UnplacedDeleted)
	}
	requirePresent(t, s, blobID, "retained unplaced")
}

// TestSweepRetainVetoCoversAReservedIDWithNoRow: an owner that reserved
// an id before its bytes existed has a file and no row, and the veto
// has to reach that shape too.
func TestSweepRetainVetoCoversAReservedIDWithNoRow(t *testing.T) {
	s := openTestStoreAt(t, base)
	blobID := orphan(t, s, blob(40), base.Add(-2*defaultOrphanFileAge))

	w := newSweeper(t, s, RetentionConfig{
		Now:    fixedClock(base),
		Retain: func(gotID string) bool { return gotID == blobID },
	})
	result, err := w.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if result.Retained != 1 || result.OrphanFilesDeleted != 0 {
		t.Fatalf("retained = %d, orphans = %d, want 1, 0", result.Retained, result.OrphanFilesDeleted)
	}
	if !fileExists(t, s, blobID) {
		t.Fatal("reserved file was removed")
	}
}

// TestSweepRemovesAgedUnreferencedFiles: a file no row names is only
// collectable once it is older than the write-in-progress window, so the
// aged one goes and the fresh one stays.
func TestSweepRemovesAgedUnreferencedFiles(t *testing.T) {
	s := openTestStoreAt(t, base)
	aged := orphan(t, s, blob(70), base.Add(-defaultOrphanFileAge-time.Hour))
	fresh := orphan(t, s, blob(90), base)

	w := newSweeper(t, s, RetentionConfig{Now: fixedClock(base)})
	result, err := w.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if result.OrphanFilesDeleted != 1 || result.OrphanFileBytes != 70 {
		t.Fatalf("orphans = %d, %d bytes, want 1, 70", result.OrphanFilesDeleted, result.OrphanFileBytes)
	}
	if fileExists(t, s, aged) {
		t.Fatal("aged orphan survived")
	}
	if !fileExists(t, s, fresh) {
		t.Fatal("fresh orphan was removed")
	}
}

// TestSweepIgnoresFilesItDidNotName: this package wrote every file it
// owns and named each one with an id, so anything else in the directory
// belongs to somebody else.
func TestSweepIgnoresFilesItDidNotName(t *testing.T) {
	s := openTestStoreAt(t, base)
	foreign := map[string]string{
		"notes.txt":                        "not a blob",
		"0123456789ABCDEF0123456789ABCDEF": "uppercase is not an id",
		"0123456789abcdef0123456789abcde":  "short is not an id",
	}
	for name, content := range foreign {
		path := filepath.Join(s.dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if err := os.Chtimes(path, base.Add(-100*time.Hour), base.Add(-100*time.Hour)); err != nil {
			t.Fatalf("backdate %s: %v", name, err)
		}
	}
	sub := filepath.Join(s.dir, "keepme")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	w := newSweeper(t, s, RetentionConfig{Now: fixedClock(base), MaxBytes: Unbounded})
	result, err := w.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if result != (SweepResult{}) {
		t.Fatalf("sweep touched foreign entries: %+v", result)
	}
	for name := range foreign {
		if _, err := os.Stat(filepath.Join(s.dir, name)); err != nil {
			t.Fatalf("%s was removed: %v", name, err)
		}
	}
	if _, err := os.Stat(sub); err != nil {
		t.Fatalf("subdirectory was removed: %v", err)
	}
}

// TestSweepEvictsOldestGroupsOverBudget: the byte budget takes whole
// groups, oldest first, and stops the moment the survivors fit.
func TestSweepEvictsOldestGroupsOverBudget(t *testing.T) {
	s := openTestStoreAt(t, base)
	var groups [][]string
	for i := range 4 {
		groupID := "g" + string(rune('0'+i))
		groups = append(groups, placeGroup(t, s, groupID, base.Add(-time.Duration(4-i)*time.Hour), 1, 100))
	}

	w := newSweeper(t, s, RetentionConfig{
		Now:         fixedClock(base),
		MaxBytes:    350,
		MinGroupAge: time.Minute,
	})
	result, err := w.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if result.GroupsEvicted != 1 || result.GroupBytes != 100 {
		t.Fatalf("evicted = %d, %d bytes, want 1, 100", result.GroupsEvicted, result.GroupBytes)
	}
	requireAbsent(t, s, groups[0][0], "oldest group")
	for i := 1; i < 4; i++ {
		requirePresent(t, s, groups[i][0], "surviving group")
	}
}

// TestSweepBudgetProtectsPinnedGroups: a pinned group is skipped and the
// budget moves on to the next oldest, so pinning one group never forces
// the sweep to stop collecting.
func TestSweepBudgetProtectsPinnedGroups(t *testing.T) {
	s := openTestStoreAt(t, base)
	var groups [][]string
	for i := range 5 {
		groupID := "g" + string(rune('0'+i))
		groups = append(groups, placeGroup(t, s, groupID, base.Add(-time.Duration(5-i)*time.Hour), 1, 100))
	}

	w := newSweeper(t, s, RetentionConfig{
		Now:         fixedClock(base),
		MaxBytes:    250,
		MinGroups:   1,
		MinGroupAge: time.Minute,
		Protected:   []string{"g0"},
	})
	result, err := w.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if result.GroupsEvicted != 3 {
		t.Fatalf("evicted = %d, want 3", result.GroupsEvicted)
	}
	requirePresent(t, s, groups[0][0], "pinned group")
	requirePresent(t, s, groups[4][0], "newest group")
	for i := 1; i <= 3; i++ {
		requireAbsent(t, s, groups[i][0], "evicted group")
	}
}

// TestSweepBudgetKeepsTheNewestGroups: the floor is what stops a budget
// from emptying the directory, so the newest groups are not candidates
// even when the arithmetic still says the pass is over budget.
func TestSweepBudgetKeepsTheNewestGroups(t *testing.T) {
	s := openTestStoreAt(t, base)
	var groups [][]string
	for i := range 5 {
		groupID := "g" + string(rune('0'+i))
		groups = append(groups, placeGroup(t, s, groupID, base.Add(-time.Duration(5-i)*time.Hour), 1, 100))
	}

	w := newSweeper(t, s, RetentionConfig{
		Now:         fixedClock(base),
		MaxBytes:    10,
		MinGroups:   4,
		MinGroupAge: time.Minute,
	})
	result, err := w.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if result.GroupsEvicted != 1 {
		t.Fatalf("evicted = %d, want 1", result.GroupsEvicted)
	}
	requireAbsent(t, s, groups[0][0], "oldest group")
	for i := 1; i < 5; i++ {
		requirePresent(t, s, groups[i][0], "floored group")
	}
}

// TestSweepBudgetSparesYoungGroups: a group younger than the floor may
// still have work running against it, so it is not an eviction
// candidate even when the budget is still unmet. Both groups are young,
// so only the age rule can spare them.
func TestSweepBudgetSparesYoungGroups(t *testing.T) {
	s := openTestStoreAt(t, base)
	older := placeGroup(t, s, "older", base.Add(-2*time.Minute), 1, 100)[0]
	newer := placeGroup(t, s, "newer", base.Add(-time.Minute), 1, 100)[0]

	w := newSweeper(t, s, RetentionConfig{
		Now:         fixedClock(base),
		MaxBytes:    10,
		MinGroups:   1,
		MinGroupAge: time.Hour,
	})
	result, err := w.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if result.GroupsEvicted != 0 {
		t.Fatalf("evicted = %d, want 0", result.GroupsEvicted)
	}
	requirePresent(t, s, older, "older young group")
	requirePresent(t, s, newer, "newer young group")
}

// TestSweepUnboundedNeverEvictsAGroup: the budget can be turned off
// without turning off the other two sweeps.
func TestSweepUnboundedNeverEvictsAGroup(t *testing.T) {
	s := openTestStoreAt(t, base)
	blobID := placeGroup(t, s, "g1", base.Add(-100*time.Hour), 1, 100)[0]

	w := newSweeper(t, s, RetentionConfig{
		Now:         fixedClock(base),
		MaxBytes:    Unbounded,
		MinGroupAge: time.Minute,
	})
	result, err := w.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if result.GroupsEvicted != 0 {
		t.Fatalf("evicted = %d, want 0", result.GroupsEvicted)
	}
	requirePresent(t, s, blobID, "group under an unbounded budget")
}

// TestSweepUnderBudgetEvictsNothing: nothing goes while the directory
// fits.
func TestSweepUnderBudgetEvictsNothing(t *testing.T) {
	s := openTestStoreAt(t, base)
	blobID := placeGroup(t, s, "g1", base.Add(-100*time.Hour), 2, 100)[0]

	w := newSweeper(t, s, RetentionConfig{
		Now:         fixedClock(base),
		MaxBytes:    defaultMaxBytes,
		MinGroupAge: time.Minute,
	})
	result, err := w.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if result.GroupsEvicted != 0 {
		t.Fatalf("evicted = %d, want 0", result.GroupsEvicted)
	}
	requirePresent(t, s, blobID, "group under budget")
}

// TestSweepIsIdempotent: a second pass over a swept directory has
// nothing left to find.
func TestSweepIsIdempotent(t *testing.T) {
	s := openTestStoreAt(t, base)
	blobID := newID(t)
	place(t, s, Blob{ID: blobID, ContentType: "audio/mpeg", CreatedAt: base}, blob(60))
	orphan(t, s, blob(20), base.Add(-2*defaultOrphanFileAge))

	w := newSweeper(t, s, RetentionConfig{Now: fixedClock(base.Add(24 * time.Hour)), MaxBytes: Unbounded})
	first, err := w.Sweep(t.Context())
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if first.UnplacedDeleted != 1 || first.OrphanFilesDeleted != 1 {
		t.Fatalf("first pass did %+v, want one of each", first)
	}

	second, err := w.Sweep(t.Context())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if second != (SweepResult{}) {
		t.Fatalf("second pass did work: %+v", second)
	}
}

// TestSweepOnAMissingDirectoryIsAnError: the pass is driven by the blob
// directory, so losing it means nothing after it can be trusted.
func TestSweepOnAMissingDirectoryIsAnError(t *testing.T) {
	s := openTestStoreAt(t, base)
	w := newSweeper(t, s, RetentionConfig{Now: fixedClock(base)})
	if err := os.RemoveAll(s.dir); err != nil {
		t.Fatalf("remove dir: %v", err)
	}

	_, err := w.Sweep(t.Context())

	if err == nil {
		t.Fatal("sweep over a missing directory returned no error")
	}
	if !strings.Contains(err.Error(), "read blob dir") {
		t.Fatalf("error = %v, want the directory read named", err)
	}
}

// TestSweepOnAFailingIndexIsAnError: a failed lookup classifies a file
// as nothing, so the failure has to reach the caller rather than read as
// a clean pass.
func TestSweepOnAFailingIndexIsAnError(t *testing.T) {
	s := openTestStoreAt(t, base)
	blobID := newID(t)
	place(t, s, Blob{ID: blobID, ContentType: "audio/mpeg", CreatedAt: base}, blob(10))

	w := newSweeper(t, s, RetentionConfig{Now: fixedClock(base.Add(24 * time.Hour))})
	s.index.(*memIndex).shut()

	if _, err := w.Sweep(t.Context()); err == nil {
		t.Fatal("sweep with a broken index returned no error")
	}
	if !fileExists(t, s, blobID) {
		t.Fatal("sweep removed a file it had not classified")
	}
}

// TestNewSweeperDefaults pins the "zero value means default" promise for
// every field, because a silently zero budget or age is how a disk fills
// or a healthy blob disappears.
func TestNewSweeperDefaults(t *testing.T) {
	s := openTestStoreAt(t, base)
	w := newSweeper(t, s, RetentionConfig{})

	if w.unplacedAge != defaultUnplacedAge {
		t.Fatalf("unplacedAge = %v, want %v", w.unplacedAge, defaultUnplacedAge)
	}
	if w.orphanFileAge != defaultOrphanFileAge {
		t.Fatalf("orphanFileAge = %v, want %v", w.orphanFileAge, defaultOrphanFileAge)
	}
	if w.maxBytes != defaultMaxBytes {
		t.Fatalf("maxBytes = %d, want %d", w.maxBytes, defaultMaxBytes)
	}
	if w.minGroupAge != defaultMinGroupAge {
		t.Fatalf("minGroupAge = %v, want %v", w.minGroupAge, defaultMinGroupAge)
	}
	if w.minGroups != defaultMinGroups {
		t.Fatalf("minGroups = %d, want %d", w.minGroups, defaultMinGroups)
	}
	if w.interval != defaultSweepInterval {
		t.Fatalf("interval = %v, want %v", w.interval, defaultSweepInterval)
	}
	if w.retain == nil || w.now == nil {
		t.Fatal("retain and now must never be nil")
	}
	if w.retain("anything") {
		t.Fatal("the default veto must retain nothing")
	}
}

// TestNewSweeperRejectsInvalidConfig refuses the configurations that
// would make a pass either destructive or useless.
func TestNewSweeperRejectsInvalidConfig(t *testing.T) {
	s := openTestStoreAt(t, base)
	cases := []struct {
		name string
		cfg  RetentionConfig
	}{
		{"negative unplaced age", RetentionConfig{UnplacedAge: -time.Second}},
		{"negative orphan age", RetentionConfig{OrphanFileAge: -time.Second}},
		{"negative group age", RetentionConfig{MinGroupAge: -time.Second}},
		{"negative interval", RetentionConfig{Interval: -time.Second}},
		{"negative group floor", RetentionConfig{MinGroups: -1}},
		{"negative budget", RetentionConfig{MaxBytes: -2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.NewSweeper(tc.cfg); !errors.Is(err, ErrInvalidRetention) {
				t.Fatalf("err = %v, want ErrInvalidRetention", err)
			}
		})
	}
}

// TestSweeperStartSweepsImmediatelyAndClosesCleanly: a process that has
// been down long enough to accumulate leftovers must not wait a full
// interval to collect them, and Close must return.
func TestSweeperStartSweepsImmediatelyAndClosesCleanly(t *testing.T) {
	s := openTestStoreAt(t, base)
	blobID := newID(t)
	place(t, s, Blob{ID: blobID, ContentType: "audio/mpeg", CreatedAt: base}, blob(10))

	w := newSweeper(t, s, RetentionConfig{
		Now:      fixedClock(base.Add(24 * time.Hour)),
		Interval: time.Hour,
	})
	w.Start()

	deadline := time.Now().Add(5 * time.Second)
	for fileExists(t, s, blobID) {
		if time.Now().After(deadline) {
			t.Fatal("start did not sweep immediately")
		}
		time.Sleep(5 * time.Millisecond)
	}

	w.Close()
	w.Close() // a second Close must not block either
	if rowExists(t, s, blobID) {
		t.Fatal("row survived the immediate sweep")
	}
}

// TestSweeperCloseWithoutStartDoesNotBlock: there is no goroutine to
// wait for, so a caller that never started the loop still returns.
func TestSweeperCloseWithoutStartDoesNotBlock(t *testing.T) {
	s := openTestStoreAt(t, base)
	w := newSweeper(t, s, RetentionConfig{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Close()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on a sweeper that was never started")
	}
}
