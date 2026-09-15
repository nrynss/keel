package upload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/nrynss/keel/mediastore"
)

// sha256HexSize is the length of a SHA-256 in hex.
const sha256HexSize = 2 * sha256.Size

// upload is the index entry for one upload in flight. Every field is
// guarded by Handler.mu.
type upload struct {
	id          string
	owner       string
	group       string
	contentType string
	visibility  mediastore.Visibility
	chunkSize   int64
	// sizeBytes and chunkCount describe the size the client declared, and
	// are meaningful only when sizeKnown is true.
	sizeBytes   int64
	sizeKnown   bool
	chunkCount  int
	storedBytes int64
	// highest is the largest chunk index stored, or -1 when none is. An
	// undeclared upload spans one past it, so span stays constant time.
	highest   int64
	expiresAt time.Time
	chunks    map[int64]chunk
}

// chunk records one stored chunk.
type chunk struct {
	sizeBytes int64
	sha256    string
}

// span reports how many indices an upload expects: the declared chunk count
// when the client declared a size, and otherwise every index up to and
// including the highest one stored.
func (u *upload) span() int {
	if u.sizeKnown {
		return u.chunkCount
	}
	return int(u.highest) + 1
}

// missing lists, in ascending order, the indices an upload needs before it
// can be completed: the gaps below span, which is the whole upload when
// nothing is stored yet.
func (u *upload) missing() []int {
	span := u.span()
	if len(u.chunks) == span {
		// Every index below span is stored, so no gap exists and the scan
		// below would walk the whole span to find nothing.
		return []int{}
	}
	out := make([]int, 0, span-len(u.chunks))
	for index := 0; index < span; index++ {
		if _, ok := u.chunks[int64(index)]; !ok {
			out = append(out, index)
		}
	}
	return out
}

// received lists, in ascending order, the indices an upload has stored.
func (u *upload) received() []int {
	out := make([]int, 0, len(u.chunks))
	for index := range u.chunks {
		out = append(out, int(index))
	}
	sort.Ints(out)
	return out
}

// chunkLength reports the length of the chunk at index in an upload of
// chunkCount chunks of chunkSize bytes totalling sizeBytes bytes.
func chunkLength(sizeBytes, chunkSize int64, chunkCount int, index int64) int64 {
	if index < int64(chunkCount)-1 {
		return chunkSize
	}
	return sizeBytes - int64(chunkCount-1)*chunkSize
}

// snapshot renders the client-visible state of an upload. The caller must
// hold Handler.mu.
func (u *upload) snapshot() stateResponse {
	out := stateResponse{
		ID:          u.id,
		Owner:       u.owner,
		ContentType: u.contentType,
		Visibility:  visibilityName(u.visibility),
		ChunkSize:   u.chunkSize,
		StoredBytes: u.storedBytes,
		Received:    u.received(),
		Missing:     u.missing(),
		ExpiresAt:   u.expiresAt.UTC().Format(time.RFC3339),
	}
	if u.sizeKnown {
		size := u.sizeBytes
		count := u.chunkCount
		out.SizeBytes = &size
		out.ChunkCount = &count
	}
	return out
}

// liveLocked reports the index entry for id, and false when the upload is
// unknown or has expired. An expired entry stays in the index for Sweep to
// remove, so its chunk files are removed with it. The caller must hold
// Handler.mu.
func (h *Handler) liveLocked(id string) (*upload, bool) {
	up, ok := h.uploads[id]
	if !ok || !h.now().Before(up.expiresAt) {
		return nil, false
	}
	return up, true
}

// maxIndex reports the largest chunk index an upload may use.
func (h *Handler) maxIndex(up *upload) int64 {
	if up.sizeKnown {
		return int64(up.chunkCount) - 1
	}
	return int64(h.maxChunk) - 1
}

// ownerBytesLocked reports the bytes an owner has staged across the uploads
// in the index. The caller must hold Handler.mu.
func (h *Handler) ownerBytesLocked(owner string) int64 {
	var total int64
	for _, up := range h.uploads {
		if up.owner == owner {
			total += up.storedBytes
		}
	}
	return total
}

// chunkName is the staging path of the chunk at index, relative to the
// staging root.
func chunkName(id string, index int64) string {
	return id + "/" + strconv.FormatInt(index, 10) + ".chunk"
}

// staged is a chunk file written to disk but not yet part of an upload.
type staged struct {
	name      string
	sizeBytes int64
	sha256    string
}

// stageChunk streams body into a temporary file beside the chunk it will
// become, hashing as it goes. limit is the largest body worth keeping: the
// read stops one byte past it, so the caller can tell a body that fits from
// one that does not without reading the rest of it. A file that fits is
// left in place for the caller to keep or discard.
func (h *Handler) stageChunk(id string, index int64, body io.Reader, limit int64) (staged, error) {
	token, err := h.newID()
	if err != nil {
		return staged{}, err
	}
	name := chunkName(id, index) + "." + token + ".part"
	f, err := h.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return staged{}, err
	}
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, digest), io.LimitReader(body, limit+1))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		h.discard(name)
		return staged{}, err
	}
	return staged{name: name, sizeBytes: n, sha256: hex.EncodeToString(digest.Sum(nil))}, nil
}

// discard removes a staging file, logging a failure it cannot act on.
func (h *Handler) discard(name string) {
	if err := h.root.Remove(name); err != nil {
		h.log.Error("upload: staging file not removed", "err", err.Error())
	}
}

// digestUpload streams the count chunks of an upload into one digest in
// index order, returning the digest and the assembled size.
func (h *Handler) digestUpload(id string, count int) (string, int64, error) {
	digest := sha256.New()
	var total int64
	for index := 0; index < count; index++ {
		f, err := h.root.Open(chunkName(id, int64(index)))
		if err != nil {
			return "", 0, err
		}
		n, err := io.Copy(digest, f)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", 0, err
		}
		total += n
	}
	return hex.EncodeToString(digest.Sum(nil)), total, nil
}

// persist streams the count chunks of an upload into the store under the
// upload's own id. A failure leaves the staged chunks in place, so the
// caller can put the upload back for a retry.
func (h *Handler) persist(ctx context.Context, up *upload, count int) error {
	files := make([]*os.File, 0, count)
	defer func() {
		for _, f := range files {
			if err := f.Close(); err != nil {
				h.log.Error("upload: chunk file not closed", "err", err.Error())
			}
		}
	}()
	readers := make([]io.Reader, 0, count)
	for index := 0; index < count; index++ {
		f, err := h.root.Open(chunkName(up.id, int64(index)))
		if err != nil {
			return err
		}
		files = append(files, f)
		readers = append(readers, f)
	}
	return h.store.PersistWithID(ctx, up.id, io.MultiReader(readers...), mediastore.Put{
		ContentType: up.contentType,
		Owner:       up.owner,
		Group:       up.group,
		Visibility:  up.visibility,
	})
}

// restore returns an upload to the index after a completion that did not
// land, so the client can retry against the chunks still on disk. An upload
// that aged out meanwhile is left out for Sweep to remove.
func (h *Handler) restore(up *upload) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.now().Before(up.expiresAt) {
		h.uploads[up.id] = up
	}
}

// Sweep removes the uploads that have outlived Config.UploadTTL, along with
// any staging directory left behind by a previous process, and reports how
// many it removed. Start calls it on a ticker; a caller that left Start
// uncalled calls it directly.
func (h *Handler) Sweep() (int, error) {
	now := h.now()

	h.mu.Lock()
	expired := make([]string, 0)
	for id, up := range h.uploads {
		if !now.Before(up.expiresAt) {
			expired = append(expired, id)
			delete(h.uploads, id)
		}
	}
	h.mu.Unlock()

	removed := 0
	var errs []error
	for _, id := range expired {
		if err := h.root.RemoveAll(id); err != nil {
			errs = append(errs, fmt.Errorf("upload: remove expired upload %s: %w", id, err))
			continue
		}
		removed++
	}

	entries, err := fs.ReadDir(h.root.FS(), ".")
	if err != nil {
		errs = append(errs, fmt.Errorf("upload: read staging directory: %w", err))
		return removed, errors.Join(errs...)
	}
	h.mu.Lock()
	stale := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, live := h.uploads[entry.Name()]; live {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < h.ttl {
			continue
		}
		stale = append(stale, entry.Name())
	}
	h.mu.Unlock()

	for _, name := range stale {
		if err := h.root.RemoveAll(name); err != nil {
			errs = append(errs, fmt.Errorf("upload: remove stale upload %s: %w", name, err))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}
