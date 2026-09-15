// Package upload accepts a file as a series of resumable chunks and lands
// the assembled bytes in a media store.
//
// An upload is opened, filled, inspected and finished over HTTP:
//
//	POST {base}                  open an upload, answering with its id
//	PUT  {base}/{id}/chunks/{n}  store chunk n
//	GET  {base}/{id}             report which chunks are stored
//	POST {base}/{id}/complete    assemble, check and store the file
//
// A PUT carries the chunk's SHA-256 in the X-Chunk-SHA256 header and its
// bytes in the body. Chunks may arrive in any order, an index may be sent
// again with the digest it already has, and the same index sent with a
// different digest is refused with the chunk_mismatch code. A chunk is
// written to its staging file only once its digest matches, so a staged
// chunk is always the bytes its digest names. The per-upload and per-owner
// byte limits are enforced while a chunk arrives rather than after the file
// is assembled, so an upload cannot fill the disk and then be rejected.
//
// Completing an upload streams the chunks in order, checks the whole-file
// digest the client declares, and stores the bytes through Config.Store
// under the upload's own id. An upload that stops receiving chunks expires
// after Config.UploadTTL and its files are removed.
//
// Every failure is written in the shared error envelope, so a client
// branches on the envelope's code rather than on prose.
//
// The zero Handler is not usable: New builds one from a Config and opens
// its staging directory. The package reads no environment.
package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	keelid "github.com/nrynss/keel/id"
	"github.com/nrynss/keel/mediastore"
)

// Defaults applied when the matching Config field is zero.
const (
	defaultBasePath      = "/uploads"
	defaultChunkSize     = 8 << 20
	defaultMaxUpload     = int64(4) << 30
	defaultMaxOwner      = int64(16) << 30
	defaultMaxChunks     = 65536
	defaultUploadTTL     = 24 * time.Hour
	defaultSweepInterval = time.Hour
)

// maxNameBytes bounds the owner, group and content type names an upload may
// carry.
const maxNameBytes = 256

// errConfig reports a Config this package cannot serve from.
var errConfig = errors.New("upload: invalid config")

// BlobStore receives the assembled bytes of a finished upload. A
// mediastore.Store satisfies it.
type BlobStore interface {
	// PersistWithID stores src under blobID with the given metadata,
	// leaving a blob already stored under that id untouched.
	PersistWithID(ctx context.Context, blobID string, src io.Reader, p mediastore.Put) error
}

// Config configures New.
type Config struct {
	// Dir is the directory chunk files are staged in. New creates it when
	// it is absent and opens it as the root every staged path is resolved
	// through. Required.
	Dir string
	// Store receives the bytes of a finished upload. Required.
	Store BlobStore
	// BasePath is the URL prefix the handler serves under. Empty means
	// "/uploads". The prefix is matched as a whole path segment, so it
	// never doubles as the prefix of an unrelated route.
	BasePath string
	// MaxUploadBytes caps the size of one upload. Zero means 4 GiB.
	MaxUploadBytes int64
	// MaxOwnerBytes caps the bytes one owner may hold across the uploads
	// this handler holds at once, counting the chunks staged so far. The
	// cap is per handler, not per store, so two handlers over one store
	// each admit the full budget. Zero means 16 GiB.
	//
	// A consumer that needs one owner capped across a whole store must
	// serve that store from a single handler.
	MaxOwnerBytes int64
	// MaxChunks caps how many chunks one upload may hold, which bounds the
	// per-chunk bookkeeping a single upload can make the handler keep.
	// Zero means 65536.
	MaxChunks int
	// UploadTTL is how long an upload may sit without a chunk arriving
	// before the sweep removes it. Zero means 24 hours.
	UploadTTL time.Duration
	// SweepInterval is how often Start's loop sweeps. Zero means one hour.
	SweepInterval time.Duration
	// Now is the clock, injected for tests. Nil means time.Now.
	Now func() time.Time
	// Log receives one line per server fault and one per staging file the
	// sweep cannot remove. Nil discards.
	Log *slog.Logger
}

// Handler serves the chunked upload protocol. Build one with New.
type Handler struct {
	root     *os.Root
	store    BlobStore
	base     string
	maxUp    int64
	maxOwner int64
	maxChunk int
	ttl      time.Duration
	interval time.Duration
	now      func() time.Time
	log      *slog.Logger

	// newID mints an upload id. It is a field so a test can make ids
	// repeatable.
	newID func() (string, error)

	mu      sync.Mutex
	uploads map[string]*upload

	startOnce sync.Once
	closeOnce sync.Once
	started   atomic.Bool
	closeErr  error
	stop      chan struct{}
	done      chan struct{}
}

// New opens the staging directory and returns a handler ready to serve. A
// zero field takes its documented default; a value out of range is refused
// rather than clamped.
func New(cfg Config) (*Handler, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("%w: Dir is required", errConfig)
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("%w: Store is required", errConfig)
	}
	if cfg.MaxUploadBytes < 0 || cfg.MaxOwnerBytes < 0 || cfg.MaxChunks < 0 {
		return nil, fmt.Errorf("%w: byte and chunk limits are not negative", errConfig)
	}
	if cfg.UploadTTL < 0 || cfg.SweepInterval < 0 {
		return nil, fmt.Errorf("%w: durations are not negative", errConfig)
	}
	base := strings.TrimSuffix(cfg.BasePath, "/")
	if base == "" {
		base = defaultBasePath
	}
	if !strings.HasPrefix(base, "/") {
		return nil, fmt.Errorf("%w: BasePath must begin with a slash", errConfig)
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("upload: create staging directory: %w", err)
	}
	root, err := os.OpenRoot(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("upload: open staging directory: %w", err)
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Handler{
		root:     root,
		store:    cfg.Store,
		base:     base,
		maxUp:    orInt64(cfg.MaxUploadBytes, defaultMaxUpload),
		maxOwner: orInt64(cfg.MaxOwnerBytes, defaultMaxOwner),
		maxChunk: orInt(cfg.MaxChunks, defaultMaxChunks),
		ttl:      orDuration(cfg.UploadTTL, defaultUploadTTL),
		interval: orDuration(cfg.SweepInterval, defaultSweepInterval),
		now:      orClock(cfg.Now),
		log:      log,
		newID:    keelid.New,
		uploads:  make(map[string]*upload),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}, nil
}

// Start begins sweeping expired uploads every Config.SweepInterval until
// Close. A caller that would rather drive expiry itself leaves Start
// uncalled and calls Sweep instead.
func (h *Handler) Start() {
	h.startOnce.Do(func() {
		h.started.Store(true)
		go h.loop()
	})
}

// loop sweeps until stop is closed, then closes done.
func (h *Handler) loop() {
	defer close(h.done)
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		if removed, err := h.Sweep(); err != nil {
			h.log.Error("upload: sweep failed", "removed", removed, "err", err.Error())
		}
		select {
		case <-ticker.C:
		case <-h.stop:
			return
		}
	}
}

// Close stops Start's loop, waits for it to exit, and closes the staging
// directory. It is safe on a handler that was never started, and a second
// call is a no-op that reports the first call's error.
func (h *Handler) Close() error {
	h.closeOnce.Do(func() {
		close(h.stop)
		h.closeErr = h.root.Close()
	})
	if h.started.Load() {
		<-h.done
	}
	return h.closeErr
}

// orInt64 resolves a zero limit to its default.
func orInt64(v, fallback int64) int64 {
	if v == 0 {
		return fallback
	}
	return v
}

// orInt resolves a zero limit to its default.
func orInt(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

// orDuration resolves a zero duration to its default.
func orDuration(v, fallback time.Duration) time.Duration {
	if v == 0 {
		return fallback
	}
	return v
}

// orClock resolves a nil clock to time.Now.
func orClock(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}
