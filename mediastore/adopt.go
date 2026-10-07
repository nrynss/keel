package mediastore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nrynss/keel/id"
)

// ErrSizeMismatch is returned by Adopt when the bytes in the backend do
// not add up to the size the caller expected. The bytes stay in the
// backend and no row is written, so the blob stays unreachable.
var ErrSizeMismatch = errors.New("mediastore: size mismatch")

// ErrDigestMismatch is returned by Adopt when the bytes in the backend
// do not hash to the digest the caller expected. The bytes stay in the
// backend and no row is written, so the blob stays unreachable.
var ErrDigestMismatch = errors.New("mediastore: digest mismatch")

// Adoption names what the bytes under an id must match before Adopt
// records them. The caller learns the size and the digest from the
// client that sent the bytes and treats both as claims until Adopt has
// checked them against the stored object itself.
type Adoption struct {
	// SizeBytes is the size the stored bytes must have.
	SizeBytes int64
	// SHA256 is the digest the stored bytes must hash to. It is
	// accepted as 64 hex characters in either case, because clients
	// print digests both ways.
	SHA256 string
}

// Adopt records a row for bytes that already sit in the backend under
// blobID, after the bytes have verified against want. It is the
// completion half of a direct upload: a client that sent its bytes
// straight to the backend holds the id already, and this call is what
// first makes the bytes reachable.
//
// The call streams the stored object once, counting its bytes and
// hashing them as they pass. A size or a digest that disagrees with
// want refuses with an error matching ErrSizeMismatch or
// ErrDigestMismatch, and an id the backend holds no bytes for refuses
// with one matching ErrNotFound. The metadata row is written only after
// the checks pass, which keeps the bytes-before-row order for bytes
// this call never wrote itself: the backend acknowledged the client's
// write before the caller ever asked for completion, and that
// acknowledgement is the durability the row rests on, the same promise
// a backend's own Write makes.
//
// The content type, owner, group and visibility come from p and follow
// the rules of Persist. The store clock stamps the row unless
// p.CreatedAt is set.
//
// An id the index already holds refuses with an error matching
// ErrAlreadyExists before the bytes are read. When the row write fails
// for any other reason the bytes stay where they are, because another
// completion may have won the race for the id, so removing them could
// never be safe. Unreachable bytes age out under the orphan sweep by
// the backend's own modification time.
func (s *Store) Adopt(ctx context.Context, blobID string, want Adoption, p Put) error {
	if !id.Valid(blobID) {
		return fmt.Errorf("mediastore: adopt %q: %w: malformed id", blobID, ErrNotFound)
	}
	if want.SizeBytes < 0 {
		return fmt.Errorf("mediastore: adopt %s: %w: negative size", blobID, ErrInvalid)
	}
	digest := strings.ToLower(want.SHA256)
	if !validDigest(digest) {
		return fmt.Errorf("mediastore: adopt %s: %w: SHA256 must be 64 hex characters", blobID, ErrInvalid)
	}
	ct, ok := s.normalizeContentType(p.ContentType)
	if !ok {
		return fmt.Errorf("mediastore: adopt: %w: %q", ErrInvalidContentType, p.ContentType)
	}
	p.ContentType = ct
	if _, err := s.index.Get(ctx, blobID); err == nil {
		return fmt.Errorf("mediastore: adopt %s: %w", blobID, ErrAlreadyExists)
	} else if !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("mediastore: adopt: %w", err)
	}

	size, digestActual, err := s.measure(ctx, blobID)
	if err != nil {
		return err
	}
	if size != want.SizeBytes {
		return fmt.Errorf("mediastore: adopt %s: %w: object holds %d bytes, %d expected", blobID, ErrSizeMismatch, size, want.SizeBytes)
	}
	if digestActual != digest {
		return fmt.Errorf("mediastore: adopt %s: %w: object hashes to %s, %s expected", blobID, ErrDigestMismatch, digestActual, digest)
	}

	b := Blob{
		ID:          blobID,
		Owner:       p.Owner,
		Group:       p.Group,
		ContentType: p.ContentType,
		SizeBytes:   size,
		Visibility:  p.Visibility,
		CreatedAt:   createdAt(p, s.now()),
	}
	if err := s.index.Create(ctx, b); err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			return fmt.Errorf("mediastore: adopt %s: %w", blobID, err)
		}
		return fmt.Errorf("mediastore: adopt: %w", err)
	}
	return nil
}

// measure reads the stored object once and returns its size and its
// SHA-256 digest in lowercase hex. An id the backend holds no bytes for
// is classified as ErrNotFound, so a completion that arrives before the
// bytes can never record a row.
func (s *Store) measure(ctx context.Context, blobID string) (int64, string, error) {
	r, err := s.backend.Open(ctx, blobID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, "", notFound(blobID, fmt.Errorf("adopt: %w", err))
		}
		return 0, "", fmt.Errorf("mediastore: adopt: %w", err)
	}
	defer r.Close()
	h := sha256.New()
	size, err := io.Copy(h, r)
	if err != nil {
		return 0, "", fmt.Errorf("mediastore: adopt %s: %w", blobID, err)
	}
	return size, hex.EncodeToString(h.Sum(nil)), nil
}

// validDigest reports whether s is a SHA-256 digest as 64 hex
// characters.
func validDigest(s string) bool {
	if len(s) != 2*sha256.Size {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
