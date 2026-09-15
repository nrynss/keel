package mediastore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nrynss/keel/id"
)

// The retention sweep removes three kinds of leftover.
//
// Blobs accumulate on a box with less free disk than it has patience,
// and two of the three kinds are unreachable by any delete the product
// performs:
//
//   - An unplaced row. A blob is persisted before anything claims it,
//     so an interrupted flow leaves a row no group owns and no delete
//     path reaches. Sweep A removes them by age.
//   - An unreferenced file. Delete removes the row first, so a crash
//     between the two leaves a file with no row. Sweep B removes them
//     by file age.
//   - A whole group. Groups are what a caller evicts, because half a
//     group is worse than none of it. Sweep C evicts whole groups,
//     oldest first, until the directory fits the byte budget.
//
// A row whose file has already vanished is not reached, because the
// pass is driven by the blob directory. That costs a row of storage and
// no disk.
//
// # A veto protects an id another lifetime owner holds
//
// RetentionConfig.Retain is asked about every candidate id before
// anything is deleted, and a consumer wires it to its own tracking set.
// Even with a misconfigured age, this sweep cannot remove a row or a
// file that owner still holds, including an id the owner reserved
// before its blob existed. A short-lived voice sample outlives neither
// its own expiry nor this veto, and the veto is the stronger of the
// two.

// defaultMaxBytes is the default byte budget for the blob directory. A
// few hundred groups fit in six GiB, which still leaves most of a small
// disk free.
const defaultMaxBytes int64 = 6 << 30

// defaultUnplacedAge is how long a blob with no group may live before
// the sweep removes it. A clip is worthless the moment its request is
// answered, so two hours is generous by a wide margin while still
// leaving an in-flight persist far outside the window.
const defaultUnplacedAge = 2 * time.Hour

// defaultOrphanFileAge is how long a file with no row may live. It is
// longer than defaultUnplacedAge on purpose, because a blob is written
// before its row, so "no row" is briefly the normal state of a healthy
// persist. The age is what tells a crash leftover apart from a write in
// progress.
const defaultOrphanFileAge = 6 * time.Hour

// defaultMinGroupAge is the youngest a group may be and still be
// evicted for the byte budget. It keeps a group whose work is still
// running, or whose reader is still on the page, out of the candidate
// set entirely.
const defaultMinGroupAge = 24 * time.Hour

// defaultMinGroups is how many of the newest groups the byte budget
// never evicts, whatever the arithmetic says. Emptying the directory to
// satisfy a budget is a worse outcome than being over it.
const defaultMinGroups = 3

// defaultSweepInterval is how often Start's loop sweeps.
const defaultSweepInterval = 10 * time.Minute

// Unbounded is the MaxBytes value that turns group eviction off and
// leaves the other two sweeps running. It is a named constant rather
// than the zero value, because zero has to mean "not configured", and a
// field that silently means "no disk cap" is how a disk fills up.
const Unbounded int64 = -1

// ErrInvalidRetention is returned by NewSweeper for a configuration it
// cannot honour: a negative age, a negative budget, or a negative group
// floor.
var ErrInvalidRetention = errors.New("mediastore: invalid retention config")

// RetentionConfig configures NewSweeper. The zero value is usable and
// means every default above.
type RetentionConfig struct {
	// UnplacedAge is how old a blob with no group must be before the
	// sweep removes it. Zero means defaultUnplacedAge.
	UnplacedAge time.Duration
	// OrphanFileAge is how old a file with no row must be before the
	// sweep removes it. Zero means defaultOrphanFileAge.
	OrphanFileAge time.Duration
	// MaxBytes is the byte budget for the blob directory. Zero means
	// defaultMaxBytes, and a negative value is ErrInvalidRetention. Set
	// it to Unbounded to sweep orphans only and never evict a group.
	MaxBytes int64
	// MinGroupAge is the youngest a group may be and still be evicted
	// for the byte budget. Zero means defaultMinGroupAge.
	MinGroupAge time.Duration
	// MinGroups is how many of the newest groups are never evicted.
	// Zero means defaultMinGroups.
	MinGroups int
	// Protected lists group ids the byte budget must never evict, such
	// as the prewarmed groups a first visitor lands on.
	Protected []string
	// Retain reports whether an id belongs to another lifetime owner
	// and must be left alone. Nil retains nothing. See the package
	// comment above for why a consumer wires this to its own tracking
	// set.
	Retain func(id string) bool
	// Interval is how often Start's loop sweeps. Zero means
	// defaultSweepInterval.
	Interval time.Duration
	// Now supplies the clock the sweep judges ages against. Nil means
	// time.Now.
	Now func() time.Time
}

// SweepResult is what one pass did. It is returned for logging, and so
// a test can assert on the specific class of thing that went rather
// than on a total several causes could produce.
type SweepResult struct {
	// UnplacedDeleted and UnplacedBytes count sweep A: aged-out rows
	// with no group, row and file.
	UnplacedDeleted int
	UnplacedBytes   int64
	// OrphanFilesDeleted and OrphanFileBytes count sweep B: files with
	// no row at all.
	OrphanFilesDeleted int
	OrphanFileBytes    int64
	// GroupsEvicted and GroupBytes count sweep C: whole groups dropped
	// to get back under the byte budget.
	GroupsEvicted int
	GroupBytes    int64
	// Retained counts candidates another owner vetoed through Retain. A
	// non-zero value here is two mechanisms staying out of each other's
	// way, not a failure.
	Retained int
	// BytesBefore and BytesAfter are the blob directory's measured size
	// at the start and end of the pass.
	BytesBefore int64
	BytesAfter  int64
}

// Sweeper enforces the retention policy over one Store. Create it with
// NewSweeper, because the zero value has no store. Sweep is safe to
// call concurrently with the store's own traffic.
type Sweeper struct {
	store         *Store
	unplacedAge   time.Duration
	orphanFileAge time.Duration
	maxBytes      int64
	minGroupAge   time.Duration
	minGroups     int
	protected     map[string]bool
	retain        func(id string) bool
	interval      time.Duration
	now           func() time.Time

	startOnce sync.Once
	closeOnce sync.Once
	started   atomic.Bool
	stop      chan struct{}
	done      chan struct{}
}

// NewSweeper returns the retention sweeper for s.
func (s *Store) NewSweeper(cfg RetentionConfig) (*Sweeper, error) {
	if cfg.UnplacedAge < 0 || cfg.OrphanFileAge < 0 || cfg.MinGroupAge < 0 || cfg.Interval < 0 {
		return nil, fmt.Errorf("mediastore: new sweeper: %w: durations must not be negative", ErrInvalidRetention)
	}
	if cfg.MinGroups < 0 {
		return nil, fmt.Errorf("mediastore: new sweeper: %w: MinGroups must not be negative", ErrInvalidRetention)
	}
	if cfg.MaxBytes < 0 && cfg.MaxBytes != Unbounded {
		return nil, fmt.Errorf("mediastore: new sweeper: %w: MaxBytes must not be negative", ErrInvalidRetention)
	}
	w := &Sweeper{
		store:         s,
		unplacedAge:   orDuration(cfg.UnplacedAge, defaultUnplacedAge),
		orphanFileAge: orDuration(cfg.OrphanFileAge, defaultOrphanFileAge),
		maxBytes:      cfg.MaxBytes,
		minGroupAge:   orDuration(cfg.MinGroupAge, defaultMinGroupAge),
		minGroups:     cfg.MinGroups,
		protected:     make(map[string]bool, len(cfg.Protected)),
		retain:        cfg.Retain,
		interval:      orDuration(cfg.Interval, defaultSweepInterval),
		now:           cfg.Now,
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
	if cfg.MaxBytes == 0 {
		w.maxBytes = defaultMaxBytes
	}
	if cfg.MinGroups == 0 {
		w.minGroups = defaultMinGroups
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.retain == nil {
		w.retain = func(string) bool { return false }
	}
	for _, group := range cfg.Protected {
		w.protected[group] = true
	}
	return w, nil
}

// orDuration is the "zero means the default" resolution the config doc
// promises for every duration field.
func orDuration(v, fallback time.Duration) time.Duration {
	if v == 0 {
		return fallback
	}
	return v
}

// Start runs Sweep on a ticker until Close. It sweeps once immediately,
// so a process that has been down long enough to accumulate orphans
// does not wait a full interval to clean them. A second Start is a
// no-op.
func (w *Sweeper) Start() {
	w.startOnce.Do(func() {
		w.started.Store(true)
		go w.loop()
	})
}

// loop is Start's goroutine. Its only exit is Close.
func (w *Sweeper) loop() {
	defer close(w.done)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		// A failed sweep is retried on the next tick: nothing about a
		// retention pass is worth stopping the server for, and the
		// blobs it did not reach are still there to reach.
		if _, err := w.Sweep(context.Background()); err != nil {
			w.store.log.Error("mediastore: retention sweep failed", "err", err.Error())
		}
		select {
		case <-ticker.C:
		case <-w.stop:
			return
		}
	}
}

// Close stops Start's loop and waits for it to exit. It is safe on a
// Sweeper that was never started, because there is no goroutine to wait
// for, and safe to call more than once.
func (w *Sweeper) Close() {
	w.closeOnce.Do(func() {
		close(w.stop)
	})
	if w.started.Load() {
		<-w.done
	}
}

// blobEntry is one file in the blob directory, classified against its
// metadata row.
type blobEntry struct {
	id      string
	size    int64
	modTime time.Time
	group   string
	created time.Time
	// orphan is true when the file has no metadata row at all.
	orphan bool
}

// Sweep runs one retention pass: aged-out unplaced rows, then
// unreferenced files, then whole groups when the directory is still
// over budget, oldest first.
//
// It returns what it did. A failure to remove one file is logged and
// the pass continues, because a sweep that stops at the first stubborn
// file leaves the rest of the disk uncollected, which is the failure it
// exists to prevent. A failure to read the directory or the index is
// returned, because nothing after it can be trusted.
func (w *Sweeper) Sweep(ctx context.Context) (SweepResult, error) {
	now := w.now()
	blobs, err := w.scan(ctx)
	if err != nil {
		return SweepResult{}, err
	}

	var result SweepResult
	live := make([]blobEntry, 0, len(blobs))
	for _, b := range blobs {
		result.BytesBefore += b.size
		if w.retain(b.id) {
			// Another lifetime owner still holds this id, and the veto
			// is absolute.
			result.Retained++
			live = append(live, b)
			continue
		}
		switch {
		case b.orphan:
			if now.Sub(b.modTime) < w.orphanFileAge {
				live = append(live, b)
				continue
			}
			if err := w.store.root.Remove(b.id); err != nil && !errors.Is(err, fs.ErrNotExist) {
				w.store.log.Error("mediastore: unreferenced blob not removed", "id", b.id, "err", err.Error())
				live = append(live, b)
				continue
			}
			result.OrphanFilesDeleted++
			result.OrphanFileBytes += b.size
		case b.group == "":
			if now.Sub(b.created) < w.unplacedAge {
				live = append(live, b)
				continue
			}
			if err := w.store.Delete(ctx, b.id); err != nil && !errors.Is(err, ErrNotFound) {
				w.store.log.Error("mediastore: unplaced blob not deleted", "id", b.id, "err", err.Error())
				live = append(live, b)
				continue
			}
			result.UnplacedDeleted++
			result.UnplacedBytes += b.size
		default:
			live = append(live, b)
		}
	}

	evicted, bytes, err := w.enforceBudget(ctx, now, live)
	if err != nil {
		return result, err
	}
	result.GroupsEvicted = evicted
	result.GroupBytes = bytes
	result.BytesAfter = result.BytesBefore - result.UnplacedBytes - result.OrphanFileBytes - result.GroupBytes
	return result, nil
}

// scan lists the blob directory and classifies every file against its
// row. A file whose name is not a blob id is ignored entirely, because
// this package named every blob it wrote and anything else belongs to
// somebody the sweep has no business deleting.
//
// The lookups run one at a time in directory order. Each is a point
// read on the index, and the whole pass runs on a sweep ticker rather
// than on a request, so holding the index for the length of a directory
// listing buys nothing.
func (w *Sweeper) scan(ctx context.Context) ([]blobEntry, error) {
	entries, err := os.ReadDir(w.store.dir)
	if err != nil {
		return nil, fmt.Errorf("mediastore: sweep: read blob dir: %w", err)
	}
	blobs := make([]blobEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !id.Valid(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // removed under us, so the next pass will not see it
			}
			return nil, fmt.Errorf("mediastore: sweep: stat %s: %w", entry.Name(), err)
		}
		b := blobEntry{id: entry.Name(), size: info.Size(), modTime: info.ModTime()}
		m, err := w.store.find(ctx, b.id)
		switch {
		case errors.Is(err, ErrNotFound):
			b.orphan = true
		case err != nil:
			// A failed lookup means this file was classified as
			// nothing, so the failure reaches the caller rather than
			// reading as a clean pass.
			return nil, fmt.Errorf("mediastore: sweep: %w", err)
		default:
			b.group, b.created = m.Group, m.CreatedAt
		}
		blobs = append(blobs, b)
	}
	return blobs, nil
}

// enforceBudget evicts whole groups, oldest first, until the surviving
// blobs fit the byte budget. A group is the unit because half a group
// is worse than a group that is gone, so one removal takes every row
// and every file the group held.
//
// Three things are never evicted, whatever the arithmetic says: a group
// in Protected, a group younger than MinGroupAge, whose work may still
// be running, and the newest MinGroups groups, because an empty
// directory is worse than an over-budget one.
func (w *Sweeper) enforceBudget(ctx context.Context, now time.Time, live []blobEntry) (int, int64, error) {
	if w.maxBytes < 0 {
		return 0, 0, nil
	}
	var total int64
	byGroup := make(map[string]int64)
	for _, b := range live {
		total += b.size
		if b.group != "" {
			byGroup[b.group] += b.size
		}
	}
	if total <= w.maxBytes {
		return 0, 0, nil
	}

	groups, err := w.store.index.Groups(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("mediastore: sweep: list groups: %w", err)
	}
	// The newest MinGroups groups are kept, so the candidate list is
	// the sorted one with its tail trimmed.
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].CreatedAt.Before(groups[j].CreatedAt) })
	candidates := groups
	if len(candidates) > w.minGroups {
		candidates = candidates[:len(candidates)-w.minGroups]
	} else {
		candidates = nil
	}

	evicted, freed := 0, int64(0)
	for _, group := range candidates {
		if total <= w.maxBytes {
			break
		}
		if w.protected[group.ID] || now.Sub(group.CreatedAt) < w.minGroupAge {
			continue
		}
		removed, err := w.evictGroup(ctx, group)
		if err != nil {
			return evicted, freed, err
		}
		// byGroup is what this pass measured on disk, and the rows' own
		// sizes are the fallback for a blob written between the
		// directory scan and the eviction.
		size := byGroup[group.ID]
		if removed > size {
			size = removed
		}
		total -= size
		freed += size
		evicted++
	}
	return evicted, freed, nil
}

// evictGroup deletes one group and every file its rows named. The group
// arrives with its blobs already read, because the delete takes the
// rows and nothing could name the files afterwards.
func (w *Sweeper) evictGroup(ctx context.Context, group Group) (int64, error) {
	err := w.store.index.DeleteGroup(ctx, group.ID)
	if errors.Is(err, ErrNotFound) {
		return 0, nil // already gone, so there is nothing to charge to the budget
	}
	if err != nil {
		return 0, fmt.Errorf("mediastore: sweep: delete group %s: %w", group.ID, err)
	}
	var freed int64
	for _, b := range group.Blobs {
		if w.retain(b.ID) {
			// The veto is absolute, so the file stays and its owner
			// removes it.
			continue
		}
		if err := w.store.root.Remove(b.ID); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				w.store.log.Error("mediastore: evicted blob not removed", "id", b.ID, "group", group.ID, "err", err.Error())
			}
			continue
		}
		freed += b.SizeBytes
	}
	w.store.log.Info("mediastore: evicted group for retention budget", "group", group.ID, "blobs", len(group.Blobs), "bytes", freed)
	return freed, nil
}
