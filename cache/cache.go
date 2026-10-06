// Package cache reuses the outcome of a paid generation, so the same
// request pays once.
//
// A Cache stores entries under keys the caller builds. A key covers the
// provider, the model, the parameters and the hashes of the inputs. Key
// hashes any caller struct as canonical JSON, so such a key is one struct
// away. The digest photo.Normalize returns on Result.SHA256 is the input
// hash of an image key, so the same pixels hash to one key whatever
// metadata each upload carried.
//
// GetOrMake returns a stored entry, or runs the caller's maker once per key
// across concurrent callers. The callers that arrive while the maker runs
// wait for it and receive the outcome it produced, so a double click pays
// once. The flight is the in-process primitive, and it holds no lock across
// processes.
//
// A failed make is never cached. A make failure the caller's classifier
// marks permanent for the key is stored as a refusal for a short time and
// expires back to a miss. The package never decides permanence itself.
//
// Entries expire by age. An expired entry behaves exactly like a miss and
// goes through the same single flight, so callers who arrive together after
// expiry still trigger one make.
//
// An entry's payload is bytes plus a content type, so a structured result
// is as natural as a media one. A JSON document stores its own bytes. A
// media payload is too big to inline, so its maker persists the bytes into
// a mediastore group and stores a small JSON document that names the blob.
// The maker must return bytes that outlive the call, because a provider
// result URL expires and the entry is read back long after the make.
//
// Eviction rides the mediastore group that holds the blobs. An entry
// carries the blob id its payload names. The entry index in
// cache/sqlitestore carries a sweep that deletes expired rows and rows
// whose blob a retention sweep has dropped. Run it beside the mediastore
// sweeper, with Present wired to that index.
//
// Multiple cache levels are multiple Cache instances, each with its own
// Config and its own key structs. There is no multi-level API. Two levels
// that share one entry index need distinct key structs or distinct
// namespaces, so their rows never collide.
//
// Config carries everything the package would otherwise read from the
// world. Nil means a sensible default, and the zero Config is usable.
package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/singleflight"
)

// ErrNotFound is returned by a Store when a key has no row. GetOrMake
// treats it as a miss. A store fault never matches it, so a read the store
// cannot vouch for is never mistaken for a miss that is worth a paid make.
var ErrNotFound = errors.New("cache: no entry for key")

// ErrInvalidConfig is returned by New for a Config it cannot honour, such
// as a negative age.
var ErrInvalidConfig = errors.New("cache: invalid config")

// ErrRefused reports a hit on a cached refusal. The message the maker
// returned travels with it. Sentinel identity does not survive storage, so
// a caller matches this sentinel and reads the message, and never a
// sentinel the maker returned.
var ErrRefused = errors.New("cache: refused")

// DefaultMaxAge is the entry age a Config means when MaxAge is zero. A
// week suits a verdict whose sources change slowly, and a generation whose
// inputs repeat.
const DefaultMaxAge = 7 * 24 * time.Hour

// DefaultRefusalTTL is how long a cached refusal lives when Config leaves
// RefusalTTL at zero. An hour absorbs a retry storm, and it asks again for
// the same input soon enough to notice a fix.
const DefaultRefusalTTL = time.Hour

// Result is the outcome of one make. It is what GetOrMake returns and what
// an entry stores.
type Result struct {
	// Payload is the bytes of the outcome. Every caller receives its own
	// copy, so a caller may keep or hand off the bytes without racing
	// another.
	Payload []byte

	// ContentType is the type Payload reads as, for example
	// application/json for a document or for the pointer document that
	// names a blob. Set it always, because a hit must know how to read
	// the bytes back.
	ContentType string

	// ChargeRef names the charge that produced the outcome, as the
	// opaque string the caller's own spend record uses. A report joins
	// the savings of every hit against it. Empty means the maker
	// recorded none.
	ChargeRef string

	// BlobID names the mediastore blob that holds the durable copy of
	// the payload, when there is one. Empty means the payload is its
	// own bytes. The entry index sweep matches it against the blobs a
	// retention sweep has kept.
	BlobID string
}

// Entry is one record in a Store. GetOrMake builds it, and the store keeps
// it.
type Entry struct {
	// Payload is the stored bytes. A refusal entry stores the message
	// of the error it caches.
	Payload []byte

	// ContentType is the type Payload reads as. A refusal entry reads
	// as text/plain.
	ContentType string

	// ChargeRef is the charge reference the maker recorded on success.
	// A refusal entry carries none.
	ChargeRef string

	// BlobID names the blob the payload points at, when there is one.
	BlobID string

	// Refusal marks an entry that caches a permanent make failure. The
	// payload then carries the error message.
	Refusal bool

	// CreatedAt is when the entry was stored. Expiry is judged against
	// it.
	CreatedAt time.Time
}

// Maker runs the paid generation GetOrMake caches. It must return bytes
// that outlive the call. A provider result URL expires, so a maker that
// receives one persists the bytes it names, for example into a mediastore
// group, before it returns. A maker returns a Result or an error, never
// both.
type Maker func(ctx context.Context) (Result, error)

// Classifier reports whether a make failure is permanent for the key that
// produced it. A permanent failure is cached as a refusal for the refusal
// lifetime. The package never decides permanence itself, and a nil
// Classifier caches no refusal.
type Classifier func(err error) bool

// Store is the entry index. cache/sqlitestore implements it over SQLite,
// and a test can supply its own. An implementation must be safe for
// concurrent use, must return an error matching ErrNotFound for a key with
// no row, and must never return ErrNotFound for a fault.
type Store interface {
	// Get returns the entry stored under key, or an error matching
	// ErrNotFound.
	Get(ctx context.Context, key string) (Entry, error)

	// Put stores entry under key, replacing any row already there. A
	// Put that returns without error is durable.
	Put(ctx context.Context, key string, entry Entry) error
}

// Config configures New. The zero value is usable and means the defaults
// this package names.
type Config struct {
	// Store is the entry index. Nil keeps every entry in flight only,
	// so results are shared between callers that arrive together and
	// forgotten the moment the make settles.
	Store Store

	// MaxAge is how old an entry may be before it reads as a miss.
	// Zero means DefaultMaxAge. A negative value is refused.
	MaxAge time.Duration

	// RefusalTTL is how long a cached refusal is served before it
	// reads as a miss again. Zero means DefaultRefusalTTL. A negative
	// value is refused.
	RefusalTTL time.Duration

	// Classifier marks the make failures that are permanent for their
	// key. Nil caches no refusal.
	Classifier Classifier

	// Now is the clock expiry is judged against and the stamp an entry
	// carries. Nil means time.Now.
	Now func() time.Time

	// Log receives one line per entry the store refused to keep. Nil
	// discards.
	Log *slog.Logger
}

// Cache reuses the outcome of paid generations under caller-built keys.
// Create it with New, because the zero value has no clock. A Cache is safe
// for concurrent use.
type Cache struct {
	store      Store
	maxAge     time.Duration
	refusalTTL time.Duration
	classify   Classifier
	now        func() time.Time
	log        *slog.Logger

	// flights is the in-process single flight. One maker runs per key
	// at a time, and every caller that arrives while it runs receives
	// its outcome.
	flights singleflight.Group
}

// New resolves cfg and returns the Cache. A negative age is refused. Every
// other zero resolves to the default this package names for it.
func New(cfg Config) (*Cache, error) {
	if cfg.MaxAge < 0 {
		return nil, fmt.Errorf("cache: new: %w: MaxAge must not be negative", ErrInvalidConfig)
	}
	if cfg.RefusalTTL < 0 {
		return nil, fmt.Errorf("cache: new: %w: RefusalTTL must not be negative", ErrInvalidConfig)
	}
	maxAge := cfg.MaxAge
	if maxAge == 0 {
		maxAge = DefaultMaxAge
	}
	refusalTTL := cfg.RefusalTTL
	if refusalTTL == 0 {
		refusalTTL = DefaultRefusalTTL
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Cache{
		store:      cfg.Store,
		maxAge:     maxAge,
		refusalTTL: refusalTTL,
		classify:   cfg.Classifier,
		now:        now,
		log:        log,
	}, nil
}

// Key hashes v as canonical JSON and returns the lowercase hex SHA-256 of
// the encoding. The encoding is what encoding/json writes for a struct:
// fields in declaration order, map keys sorted, exported fields only. The
// same value therefore hashes to the same key in every process and on
// every run, and map iteration order never reaches the hash.
//
// A key struct carries the provider, the model, the parameters and the
// input hashes, so one request hashes to one key. A struct that names no
// version of its own meaning keys on its content forever. Give a second
// cache level a key struct of its own, so two meanings never share bytes.
func Key(v any) (string, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("cache: key: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// GetOrMake returns the entry stored under key, or runs maker once and
// stores its outcome. Callers that arrive while the maker runs receive the
// outcome it produced, whatever caller started it, so two callers who
// arrive together trigger one make. Expiry is judged inside that flight, so
// callers who arrive together after expiry still trigger one make.
//
// A maker that succeeds is stored under key with the charge reference it
// reports. A maker that fails is never stored, unless the Config classifier
// marks the failure permanent for this key, in which case the refusal is
// served for the refusal lifetime. The caller that started the make always
// receives the maker's own error.
//
// A store read that fails for a reason other than a missing entry is
// returned, and no make runs, because a read the store cannot vouch for is
// not a miss. A store write that fails after a successful make is logged
// and dropped, because the caller has already paid for the result and must
// receive it.
//
// The first caller's context bounds the maker, so a caller that joins a
// running make shares its fate. Pass a context that outlives the request
// when the make must survive it.
func (c *Cache) GetOrMake(ctx context.Context, key string, maker Maker) (Result, error) {
	if maker == nil {
		return Result{}, errors.New("cache: get or make: maker is nil")
	}
	if key == "" {
		return Result{}, errors.New("cache: get or make: key is empty")
	}
	outcome, err, _ := c.flights.Do(key, func() (any, error) {
		return c.load(ctx, key, maker)
	})
	res, ok := outcome.(Result)
	if !ok {
		// The flight stores only what load returns, so this guard
		// reports a programming error instead of trusting the value.
		return Result{}, fmt.Errorf("cache: get or make %s: flight returned %T", key, outcome)
	}
	// The flight hands every joined caller the same slice, so each
	// caller receives a copy it owns.
	res.Payload = bytes.Clone(res.Payload)
	return res, err
}

// load is the one make per key the flight runs. It serves a live entry, and
// otherwise runs the maker and stores the outcome.
func (c *Cache) load(ctx context.Context, key string, maker Maker) (Result, error) {
	entry, found, err := c.lookup(ctx, key)
	if err != nil {
		return Result{}, err
	}
	if found {
		age := c.now().Sub(entry.CreatedAt)
		switch {
		case entry.Refusal && age < c.refusalTTL:
			return Result{}, fmt.Errorf("%w: %s", ErrRefused, entry.Payload)
		case !entry.Refusal && age < c.maxAge:
			return Result{
				Payload:     entry.Payload,
				ContentType: entry.ContentType,
				ChargeRef:   entry.ChargeRef,
				BlobID:      entry.BlobID,
			}, nil
		}
	}
	res, err := maker(ctx)
	if err != nil {
		if c.classify != nil && c.classify(err) {
			c.storeRefusal(ctx, key, err)
		}
		return Result{}, err
	}
	c.storeResult(ctx, key, res)
	return res, nil
}

// lookup reads the entry under key. A missing entry is found false with a
// nil error, and a store fault is returned, so no make runs on a read the
// store cannot vouch for.
func (c *Cache) lookup(ctx context.Context, key string) (Entry, bool, error) {
	if c.store == nil {
		return Entry{}, false, nil
	}
	entry, err := c.store.Get(ctx, key)
	if err == nil {
		return entry, true, nil
	}
	if errors.Is(err, ErrNotFound) {
		return Entry{}, false, nil
	}
	return Entry{}, false, fmt.Errorf("cache: get or make %s: %w", key, err)
}

// storeResult keeps a successful outcome under key. The write is best
// effort, because the maker has already run and the caller must receive the
// result it paid for even when the row does not land.
func (c *Cache) storeResult(ctx context.Context, key string, res Result) {
	if c.store == nil {
		return
	}
	entry := Entry{
		Payload:     res.Payload,
		ContentType: res.ContentType,
		ChargeRef:   res.ChargeRef,
		BlobID:      res.BlobID,
		CreatedAt:   c.now().UTC(),
	}
	if err := c.store.Put(ctx, key, entry); err != nil {
		c.log.Error("cache: entry not stored", "key", key, "err", err.Error())
	}
}

// storeRefusal keeps a failure the classifier marked permanent. The same
// best effort applies, and the caller still receives the maker's own error.
func (c *Cache) storeRefusal(ctx context.Context, key string, makeErr error) {
	if c.store == nil {
		return
	}
	entry := Entry{
		Payload:     []byte(makeErr.Error()),
		ContentType: "text/plain",
		Refusal:     true,
		CreatedAt:   c.now().UTC(),
	}
	if err := c.store.Put(ctx, key, entry); err != nil {
		c.log.Error("cache: refusal not stored", "key", key, "err", err.Error())
		return
	}
	c.log.Info("cache: refusal cached", "key", key)
}
