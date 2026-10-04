package mediastore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/nrynss/keel/id"
)

// ErrSnapshot is returned for a snapshot this package cannot read or
// write: an empty selection, a duplicate id, an unsupported manifest
// version, a missing blob file, a size that disagrees with the bytes,
// or a visibility the manifest does not define. Hash mismatches and id
// collisions have their own sentinels.
var ErrSnapshot = errors.New("mediastore: invalid snapshot")

// ErrHashMismatch is returned by Restore when a blob's bytes do not
// match the sha256 recorded in the manifest. Restore writes nothing
// when the mismatch is found before any blob is stored, and it removes
// blobs it already stored when the mismatch is found mid-way.
var ErrHashMismatch = errors.New("mediastore: snapshot hash mismatch")

// ManifestVersion is the manifest version Snapshot writes and the only
// version Restore accepts.
const ManifestVersion = 1

// manifestFile is the name of the manifest inside a snapshot directory.
const manifestFile = "manifest.json"

// Manifest is the JSON document Snapshot writes next to the blob files.
// Version is ManifestVersion. Blobs is in capture order.
type Manifest struct {
	Version int            `json:"version"`
	Blobs   []ManifestBlob `json:"blobs"`
}

// ManifestBlob is one captured blob. The bytes live in a sibling file
// named ID. SHA256 is the lowercase hex digest of those bytes. An empty
// Visibility is private, the same zero value Put uses.
type ManifestBlob struct {
	ID          string     `json:"id"`
	Owner       string     `json:"owner"`
	Group       string     `json:"group"`
	ContentType string     `json:"content_type"`
	Visibility  Visibility `json:"visibility"`
	SizeBytes   int64      `json:"size_bytes"`
	SHA256      string     `json:"sha256"`
	// CreatedAt is when the blob was first accepted. Restore keeps it in
	// the manifest and stamps the new row with the store clock instead.
	CreatedAt time.Time `json:"created_at"`
}

// Selection names the blobs Snapshot captures.
//
// A non-empty IDs list captures those blobs in that order and ignores
// Owner. When IDs is empty, Owner captures every blob whose Owner field
// equals Owner. The comparison is exact. It is not a prefix match and it
// does not walk a directory. Both empty is refused, and so is a
// selection that resolves to no blobs.
type Selection struct {
	IDs   []string
	Owner string
}

// Snapshot writes a manifest and one file per blob into dir. dir is
// created when it is absent. Each blob file is named by the blob id.
// The manifest is written last, so a directory without manifest.json is
// not a snapshot Restore will accept.
//
// The capture is the rows and files as they stand while each blob is
// read. Blob bytes are immutable once stored, so a row and its file
// describe one artifact. A file that disappears mid-capture fails the
// snapshot. The capture is written in a sibling directory and swapped
// into dir only after the manifest is in place, so a failed refresh
// leaves a previous snapshot untouched. An id that is not in the store
// fails the snapshot the same way.
func (s *Store) Snapshot(ctx context.Context, dir string, sel Selection) error {
	if s == nil {
		return fmt.Errorf("mediastore: snapshot: %w: nil store", ErrSnapshot)
	}
	if dir == "" {
		return fmt.Errorf("mediastore: snapshot: %w: directory must not be empty", ErrSnapshot)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	blobs, err := s.selectBlobs(ctx, sel)
	if err != nil {
		return err
	}
	parent := filepath.Dir(dir)
	if parent == "" {
		parent = "."
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("mediastore: snapshot: %w", err)
	}
	tmp, err := os.MkdirTemp(parent, filepath.Base(dir)+".partial-")
	if err != nil {
		return fmt.Errorf("mediastore: snapshot: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(tmp)
		}
	}()
	if err := s.writeSnapshot(ctx, tmp, blobs); err != nil {
		return err
	}
	if err := swapSnapshot(dir, tmp); err != nil {
		return fmt.Errorf("mediastore: snapshot: %w", err)
	}
	committed = true
	return nil
}

// writeSnapshot fills an empty directory with blob files and a manifest.
// On failure it removes the files this call created. The directory itself
// is the caller's to discard.
func (s *Store) writeSnapshot(ctx context.Context, dir string, blobs []Blob) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("mediastore: snapshot: %w", err)
	}
	defer root.Close()

	manifest := Manifest{Version: ManifestVersion, Blobs: make([]ManifestBlob, 0, len(blobs))}
	var written []string
	fail := func(err error) error {
		s.removeSnapshotFiles(root, written)
		return err
	}
	for _, b := range blobs {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		entry, err := s.captureBlob(root, b)
		if err != nil {
			return fail(err)
		}
		written = append(written, b.ID)
		manifest.Blobs = append(manifest.Blobs, entry)
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fail(fmt.Errorf("mediastore: snapshot: %w", err))
	}
	raw = append(raw, '\n')
	if err := writeRootFile(root, manifestFile+".partial", raw); err != nil {
		return fail(fmt.Errorf("mediastore: snapshot: %w", err))
	}
	if err := root.Rename(manifestFile+".partial", manifestFile); err != nil {
		s.removeSnapshotFiles(root, []string{manifestFile + ".partial"})
		return fail(fmt.Errorf("mediastore: snapshot: %w", err))
	}
	return nil
}

// swapSnapshot moves tmp onto dir. dir is renamed aside first when it
// already exists, and moved back if the swap fails, so a reader never
// sees a truncated fixture and a failed call does not replace a good one.
func swapSnapshot(dir, tmp string) error {
	_, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return os.Rename(tmp, dir)
	}
	if err != nil {
		return err
	}
	backup, err := os.MkdirTemp(filepath.Dir(dir), filepath.Base(dir)+".previous-")
	if err != nil {
		return err
	}
	if err := os.Remove(backup); err != nil {
		return err
	}
	if err := os.Rename(dir, backup); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		if rbErr := os.Rename(backup, dir); rbErr != nil {
			return fmt.Errorf("%w (restoring the previous snapshot: %v)", err, rbErr)
		}
		return err
	}
	// The new snapshot is already at dir. A leftover backup is not a
	// failed capture, and reporting one would tell the caller the old
	// fixture was kept.
	os.RemoveAll(backup)
	return nil
}

// selectBlobs resolves sel to metadata rows. IDs, when set, are read in
// order. Otherwise every blob with the named owner is returned in the
// index's list order, which is creation time then id.
func (s *Store) selectBlobs(ctx context.Context, sel Selection) ([]Blob, error) {
	if len(sel.IDs) > 0 {
		seen := make(map[string]bool, len(sel.IDs))
		out := make([]Blob, 0, len(sel.IDs))
		for _, blobID := range sel.IDs {
			if seen[blobID] {
				return nil, fmt.Errorf("mediastore: snapshot: %w: duplicate id %s", ErrSnapshot, blobID)
			}
			seen[blobID] = true
			b, err := s.find(ctx, blobID)
			if err != nil {
				return nil, fmt.Errorf("mediastore: snapshot: %w", err)
			}
			out = append(out, b)
		}
		return out, nil
	}
	if sel.Owner == "" {
		return nil, fmt.Errorf("mediastore: snapshot: %w: selection must name ids or an owner", ErrSnapshot)
	}
	all, err := s.index.Blobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("mediastore: snapshot: %w", err)
	}
	var out []Blob
	for _, b := range all {
		if b.Owner == sel.Owner {
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("mediastore: snapshot: %w: selection matches no blobs", ErrSnapshot)
	}
	return out, nil
}

// captureBlob copies one blob into the snapshot directory and returns its
// manifest entry. The digest is of the bytes just written. The bytes come
// from openBlob, so a storage backend replaces that read without this
// function learning where they live. The snapshot directory stays local.
func (s *Store) captureBlob(root *os.Root, b Blob) (ManifestBlob, error) {
	src, err := s.openBlob(b.ID)
	if err != nil {
		return ManifestBlob{}, fmt.Errorf("mediastore: snapshot %s: %w", b.ID, err)
	}
	defer src.Close()
	dst, err := root.OpenFile(b.ID, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return ManifestBlob{}, fmt.Errorf("mediastore: snapshot %s: %w", b.ID, err)
	}
	n, sum, copyErr := digestCopy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil {
		root.Remove(b.ID)
		return ManifestBlob{}, fmt.Errorf("mediastore: snapshot %s: %w", b.ID, copyErr)
	}
	if closeErr != nil {
		root.Remove(b.ID)
		return ManifestBlob{}, fmt.Errorf("mediastore: snapshot %s: %w", b.ID, closeErr)
	}
	if n != b.SizeBytes {
		root.Remove(b.ID)
		return ManifestBlob{}, fmt.Errorf("mediastore: snapshot %s: %w: size %d disagrees with %d stored bytes", b.ID, ErrSnapshot, b.SizeBytes, n)
	}
	return ManifestBlob{
		ID:          b.ID,
		Owner:       b.Owner,
		Group:       b.Group,
		ContentType: b.ContentType,
		Visibility:  b.Visibility,
		SizeBytes:   n,
		SHA256:      sum,
		CreatedAt:   b.CreatedAt.UTC(),
	}, nil
}

// openBlob returns a reader for the stored bytes of id. Snapshot is the
// caller. A storage backend replaces this function. The snapshot
// directory, a manifest plus files, is a local fixture and is not read
// through here.
func (s *Store) openBlob(id string) (io.ReadCloser, error) {
	return s.root.Open(id)
}

// Restore recreates every blob in the snapshot at dir. Each blob keeps
// the id, owner, group, content type and visibility the manifest records,
// so a URL that named the blob still names it. The manifest's creation
// time is not copied onto the row. The restored row is stamped with the
// store clock, the same clock Persist uses, so a fixture older than
// UnplacedAge is not deleted by the first default sweep.
//
// Restore refuses the whole snapshot, and writes nothing, when the
// manifest version is not ManifestVersion, a blob file is missing, a
// size or digest disagrees, or any id is already stored. A failure
// after the first blob has been stored removes the blobs this call
// created and leaves anything that was already there untouched. Bytes
// land only through PersistWithID.
//
// Visibility is not a retention pin. A restored row is swept on the same
// rules as a row Persist wrote. Protected and Retain are what keep a row
// a sweep would otherwise remove.
func (s *Store) Restore(ctx context.Context, dir string) error {
	if s == nil {
		return fmt.Errorf("mediastore: restore: %w: nil store", ErrSnapshot)
	}
	if dir == "" {
		return fmt.Errorf("mediastore: restore: %w: directory must not be empty", ErrSnapshot)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("mediastore: restore: %w", err)
	}
	defer root.Close()
	manifest, err := readManifest(root)
	if err != nil {
		return err
	}
	if err := s.preflight(ctx, root, manifest); err != nil {
		return err
	}
	var created []string
	for _, b := range manifest.Blobs {
		if err := ctx.Err(); err != nil {
			s.rollback(ctx, created)
			return err
		}
		if err := s.restoreBlob(ctx, root, b); err != nil {
			s.rollback(ctx, created)
			return err
		}
		created = append(created, b.ID)
	}
	return nil
}

// preflight checks the manifest and the files without writing a blob.
// An id that is already stored refuses the snapshot before any write.
func (s *Store) preflight(ctx context.Context, root *os.Root, manifest Manifest) error {
	seen := make(map[string]bool, len(manifest.Blobs))
	for _, b := range manifest.Blobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !id.Valid(b.ID) {
			return fmt.Errorf("mediastore: restore: %w: malformed id %q", ErrSnapshot, b.ID)
		}
		if seen[b.ID] {
			return fmt.Errorf("mediastore: restore: %w: duplicate id %s", ErrSnapshot, b.ID)
		}
		seen[b.ID] = true
		if _, err := parseVisibility(b.Visibility); err != nil {
			return fmt.Errorf("mediastore: restore %s: %w", b.ID, err)
		}
		if b.ContentType == "" {
			return fmt.Errorf("mediastore: restore %s: %w: content type must not be empty", b.ID, ErrSnapshot)
		}
		if b.SizeBytes < 0 {
			return fmt.Errorf("mediastore: restore %s: %w: negative size", b.ID, ErrSnapshot)
		}
		n, sum, err := hashRootFile(root, b.ID)
		if err != nil {
			return fmt.Errorf("mediastore: restore %s: %w", b.ID, err)
		}
		if n != b.SizeBytes {
			return fmt.Errorf("mediastore: restore %s: %w: size %d disagrees with %d stored bytes", b.ID, ErrSnapshot, b.SizeBytes, n)
		}
		if sum != b.SHA256 {
			return fmt.Errorf("mediastore: restore %s: %w", b.ID, ErrHashMismatch)
		}
		_, err = s.index.Get(ctx, b.ID)
		if err == nil {
			return fmt.Errorf("mediastore: restore %s: %w", b.ID, ErrAlreadyExists)
		}
		if !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("mediastore: restore %s: %w", b.ID, err)
		}
	}
	return nil
}

// restoreBlob stores one manifest entry. The reader checks the digest
// again as the bytes are copied, so a file that changes after preflight
// does not land.
func (s *Store) restoreBlob(ctx context.Context, root *os.Root, b ManifestBlob) error {
	src, err := root.Open(b.ID)
	if err != nil {
		return fmt.Errorf("mediastore: restore %s: %w", b.ID, err)
	}
	defer src.Close()
	vis, err := parseVisibility(b.Visibility)
	if err != nil {
		return fmt.Errorf("mediastore: restore %s: %w", b.ID, err)
	}
	checked := &checkingReader{
		r:       src,
		h:       sha256.New(),
		wantN:   b.SizeBytes,
		wantSum: b.SHA256,
		id:      b.ID,
	}
	err = s.PersistWithID(ctx, b.ID, checked, Put{
		ContentType: b.ContentType,
		Owner:       b.Owner,
		Group:       b.Group,
		Visibility:  vis,
	})
	if err != nil {
		return fmt.Errorf("mediastore: restore %s: %w", b.ID, err)
	}
	return nil
}

// rollback removes blobs this restore created. Cancellation of the
// caller's context does not stop the cleanup.
func (s *Store) rollback(ctx context.Context, ids []string) {
	ctx = context.WithoutCancel(ctx)
	for i := len(ids) - 1; i >= 0; i-- {
		if err := s.Delete(ctx, ids[i]); err != nil {
			s.log.Error("mediastore: restore rollback failed", "id", ids[i], "err", err.Error())
		}
	}
}

// readManifest loads and checks the snapshot manifest.
func readManifest(root *os.Root) (Manifest, error) {
	f, err := root.Open(manifestFile)
	if err != nil {
		return Manifest{}, fmt.Errorf("mediastore: restore: %w", err)
	}
	defer f.Close()
	var manifest Manifest
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("mediastore: restore: %w: %w", ErrSnapshot, err)
	}
	if manifest.Version != ManifestVersion {
		return Manifest{}, fmt.Errorf("mediastore: restore: %w: version %d", ErrSnapshot, manifest.Version)
	}
	return manifest, nil
}

// parseVisibility accepts the two visibilities the store writes. The
// word private is accepted as the zero value. Anything else is refused,
// so an unknown value cannot widen who may read the blob.
func parseVisibility(v Visibility) (Visibility, error) {
	switch string(v) {
	case "", "private":
		return Private, nil
	case string(Public):
		return Public, nil
	default:
		return "", fmt.Errorf("%w: visibility %q", ErrSnapshot, v)
	}
}

// removeSnapshotFiles deletes names this call created. Cleanup is best
// effort. A remove error is logged and then ignored, because the error
// that failed the snapshot is already being returned. The snapshot
// directory is the caller's fixture, not the store.
func (s *Store) removeSnapshotFiles(root *os.Root, names []string) {
	names = append(append([]string{}, names...), manifestFile+".partial")
	for _, name := range names {
		err := root.Remove(name)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Error("mediastore: snapshot cleanup failed", "name", name, "err", err.Error())
		}
	}
}

// writeRootFile replaces name with raw.
func writeRootFile(root *os.Root, name string, raw []byte) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := f.Write(raw)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// hashRootFile returns the size and lowercase sha256 of name.
func hashRootFile(root *os.Root, name string) (int64, string, error) {
	f, err := root.Open(name)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	return digestCopy(io.Discard, f)
}

// digestCopy copies src to dst and returns the size and lowercase sha256.
func digestCopy(dst io.Writer, src io.Reader) (int64, string, error) {
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), src)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// checkingReader hashes r and returns ErrHashMismatch at EOF when the
// size or digest disagrees. Persist then removes the file it created.
type checkingReader struct {
	r       io.Reader
	h       hash.Hash
	n       int64
	wantN   int64
	wantSum string
	id      string
}

func (c *checkingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.h.Write(p[:n])
		c.n += int64(n)
	}
	if err == io.EOF {
		sum := hex.EncodeToString(c.h.Sum(nil))
		if c.n != c.wantN || sum != c.wantSum {
			return n, fmt.Errorf("mediastore: restore %s: %w", c.id, ErrHashMismatch)
		}
	}
	return n, err
}
