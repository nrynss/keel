package s3

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nrynss/keel/mediastore"
)

// digestHex hashes data the way a client declares its digest.
func digestHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// sendPut sends body through a presigned URL with the headers the
// issuer returned, plus one header the test overrides when it wants to
// impersonate a client that lies about its shape.
func sendPut(client *http.Client, p Presigned, body []byte, override string, value string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPut, p.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for name, value := range p.Headers {
		req.Header.Set(name, value)
	}
	if override != "" {
		req.Header.Set(override, value)
	}
	return client.Do(req)
}

// statusOf reads a response down to its status, closing the body.
func statusOf(resp *http.Response, err error) (int, error) {
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

// TestPresignedPutRoundTripThroughCompletion pins the whole single PUT
// flow. The issuer returns a URL bound to the declared size, type and
// digest, the bucket accepts exactly those bytes, and completion
// verifies the stored object and makes it servable.
func TestPresignedPutRoundTripThroughCompletion(t *testing.T) {
	f, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	index := newMemIndex()
	s := openStore(t, b, index)
	data := deterministic(4096)
	blobID := strings.Repeat("8", 32)

	signed, err := b.PresignPut(t.Context(), PresignPutInput{
		BlobID:      blobID,
		ContentType: "video/mp4",
		SizeBytes:   int64(len(data)),
		SHA256:      digestHex(data),
	})
	if err != nil {
		t.Fatalf("presign put: %v", err)
	}
	if got := signed.Headers["Content-Type"]; got != "video/mp4" {
		t.Fatalf("content type header = %q, want the declared type", got)
	}
	if got := signed.Headers["Content-Length"]; got != strconv.Itoa(len(data)) {
		t.Fatalf("content length header = %q, want the declared size", got)
	}
	if strings.Contains(signed.URL, testSecret) {
		t.Fatal("the presigned URL spells the secret")
	}
	if !signed.Expires.After(base) {
		t.Fatalf("expires = %s, want a moment past the signing stamp", signed.Expires)
	}
	stamp, err := time.Parse("20060102T150405Z", queryValue(signed.URL, "X-Amz-Date"))
	if err != nil {
		t.Fatalf("parse the signing stamp: %v", err)
	}
	if want := stamp.Add(15 * time.Minute); !signed.Expires.Equal(want) {
		t.Fatalf("expires = %s, want the URL's own %s", signed.Expires, want)
	}

	// Nothing is reachable before the client puts or completion runs.
	if _, _, err := s.Open(t.Context(), blobID); !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("open before completion = %v, want ErrNotFound", err)
	}
	code, err := statusOf(sendPut(client, signed, data, "", ""))
	if err != nil {
		t.Fatalf("client put: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("client put = %d, want 200", code)
	}
	if !f.holds(blobID) {
		t.Fatal("the bucket took nothing under the id")
	}

	// Completion verifies against the stored object, then records.
	if err := s.Adopt(t.Context(), blobID, mediastore.Adoption{
		SizeBytes: int64(len(data)),
		SHA256:    digestHex(data),
	}, mediastore.Put{ContentType: "video/mp4", Visibility: mediastore.Public}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	r, row, err := s.Open(t.Context(), blobID)
	if err != nil {
		t.Fatalf("open after completion: %v", err)
	}
	got, readErr := io.ReadAll(r)
	r.Close()
	if readErr != nil {
		t.Fatalf("read after completion: %v", readErr)
	}
	if !bytes.Equal(got, data) || row.SizeBytes != int64(len(data)) {
		t.Fatalf("served %d bytes with size %d, want the %d uploaded", len(got), row.SizeBytes, len(data))
	}
}

// TestPresignedPutRefusesWrongSizeOrType pins the bucket-side
// enforcement. The signature binds the declared length and type, so a
// client that sends either a different body or a different type is
// refused before a byte is stored.
func TestPresignedPutRefusesWrongSizeOrType(t *testing.T) {
	f, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	index := newMemIndex()
	s := openStore(t, b, index)
	data := deterministic(1024)
	other := deterministic(512)
	blobID := strings.Repeat("9", 32)

	signed, err := b.PresignPut(t.Context(), PresignPutInput{
		BlobID:      blobID,
		ContentType: "image/png",
		SizeBytes:   int64(len(data)),
		SHA256:      digestHex(data),
	})
	if err != nil {
		t.Fatalf("presign put: %v", err)
	}

	code, err := statusOf(sendPut(client, signed, other, "", ""))
	if err != nil {
		t.Fatalf("put of the wrong size: %v", err)
	}
	if code != http.StatusForbidden {
		t.Fatalf("put of the wrong size = %d, want 403", code)
	}
	code, err = statusOf(sendPut(client, signed, data, "Content-Type", "audio/mpeg"))
	if err != nil {
		t.Fatalf("put of the wrong type: %v", err)
	}
	if code != http.StatusForbidden {
		t.Fatalf("put of the wrong type = %d, want 403", code)
	}
	if f.holds(blobID) {
		t.Fatal("the bucket stored bytes a constrained URL did not declare")
	}

	// Completion on an id whose bytes never arrived refuses with the
	// absence the store classifies on.
	if err := s.Adopt(t.Context(), blobID, mediastore.Adoption{
		SizeBytes: int64(len(data)),
		SHA256:    digestHex(data),
	}, mediastore.Put{ContentType: "image/png"}); !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("adopt after refused puts = %v, want ErrNotFound", err)
	}
	rows, err := index.Blobs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("%d rows landed for refused uploads, want none", len(rows))
	}
}

// TestPresignedPutPinnedDigestIsEnforcedAtTheBucket pins the optional
// checksum. When the issuer signs the digest, a body of any other
// shape is refused by the bucket itself, exactly the way a service
// that honours checksums refuses one.
func TestPresignedPutPinnedDigestIsEnforcedAtTheBucket(t *testing.T) {
	f, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	data := deterministic(512)
	other := bytes.Repeat([]byte{0x42}, 512)
	blobID := strings.Repeat("4", 32)

	signed, err := b.PresignPut(t.Context(), PresignPutInput{
		BlobID:      blobID,
		ContentType: "image/png",
		SizeBytes:   int64(len(data)),
		SHA256:      digestHex(data),
	})
	if err != nil {
		t.Fatalf("presign put: %v", err)
	}
	if _, ok := signed.Headers["x-amz-checksum-sha256"]; !ok {
		t.Fatal("the pinned digest is missing from the headers the client must send")
	}

	// Bytes of the declared shape pass and the object stands.
	code, err := statusOf(sendPut(client, signed, data, "", ""))
	if err != nil {
		t.Fatalf("put of the declared bytes: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("put of the declared bytes = %d, want 200", code)
	}
	if got := f.bodyOf(blobID); !bytes.Equal(got, data) {
		t.Fatalf("the bucket holds %d bytes, want the %d declared", len(got), len(data))
	}

	// Bytes of any other shape, sent with every signed header intact,
	// are refused as a bad digest before they are stored.
	second := strings.Repeat("2", 32)
	signed, err = b.PresignPut(t.Context(), PresignPutInput{
		BlobID:      second,
		ContentType: "image/png",
		SizeBytes:   int64(len(other)),
		SHA256:      digestHex(other),
	})
	if err != nil {
		t.Fatalf("presign put: %v", err)
	}
	code, err = statusOf(sendPut(client, signed, data, "", ""))
	if err != nil {
		t.Fatalf("put of bytes against a foreign digest: %v", err)
	}
	if code != http.StatusBadRequest {
		t.Fatalf("put against a foreign digest = %d, want 400", code)
	}
	if f.holds(second) {
		t.Fatal("the bucket stored bytes that do not hash to the pinned digest")
	}
}

// TestCompletedPutURLCannotSubstituteBytes pins the closure the pinned
// digest gives a completed upload. The URL lives until its expiry, but
// the checksum it signs leaves it nothing to substitute: a same-size
// body of any other shape is refused at the bucket, and the bytes the
// store serves keep matching the row completion wrote.
func TestCompletedPutURLCannotSubstituteBytes(t *testing.T) {
	f, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	s := openStore(t, b, newMemIndex())
	data := deterministic(1024)
	other := bytes.Repeat([]byte{0x42}, len(data))
	blobID := strings.Repeat("f", 32)

	signed, err := b.PresignPut(t.Context(), PresignPutInput{
		BlobID:      blobID,
		ContentType: "image/png",
		SizeBytes:   int64(len(data)),
		SHA256:      digestHex(data),
	})
	if err != nil {
		t.Fatalf("presign put: %v", err)
	}
	code, err := statusOf(sendPut(client, signed, data, "", ""))
	if err != nil || code != http.StatusOK {
		t.Fatalf("client put = %d, %v, want 200", code, err)
	}
	if err := s.Adopt(t.Context(), blobID, mediastore.Adoption{
		SizeBytes: int64(len(data)),
		SHA256:    digestHex(data),
	}, mediastore.Put{ContentType: "image/png", Visibility: mediastore.Public}); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	// A same-size substitute is refused before a byte is stored.
	code, err = statusOf(sendPut(client, signed, other, "", ""))
	if err != nil {
		t.Fatalf("substitute put: %v", err)
	}
	if code != http.StatusBadRequest {
		t.Fatalf("substitute put = %d, want 400", code)
	}

	// The verified bytes themselves may land again, and change nothing.
	code, err = statusOf(sendPut(client, signed, data, "", ""))
	if err != nil || code != http.StatusOK {
		t.Fatalf("repeat put = %d, %v, want 200", code, err)
	}
	if got := f.bodyOf(blobID); !bytes.Equal(got, data) {
		t.Fatalf("the bucket holds %d bytes, want the %d completion verified", len(got), len(data))
	}
	r, row, err := s.Open(t.Context(), blobID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, readErr := io.ReadAll(r)
	r.Close()
	if readErr != nil {
		t.Fatalf("read: %v", readErr)
	}
	if !bytes.Equal(got, data) || row.SizeBytes != int64(len(data)) {
		t.Fatalf("the store serves bytes completion never verified")
	}
}

// TestFailedCompletionLeavesNothingReachable pins the verification at
// completion. A completion whose size or digest disagrees with the
// stored object refuses, and no row lands either way, so the store
// serves nothing.
func TestFailedCompletionLeavesNothingReachable(t *testing.T) {
	_, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	index := newMemIndex()
	s := openStore(t, b, index)
	data := deterministic(2048)
	blobID := strings.Repeat("6", 32)

	signed, err := b.PresignPut(t.Context(), PresignPutInput{
		BlobID:      blobID,
		ContentType: "image/png",
		SizeBytes:   int64(len(data)),
		SHA256:      digestHex(data),
	})
	if err != nil {
		t.Fatalf("presign put: %v", err)
	}
	code, err := statusOf(sendPut(client, signed, data, "", ""))
	if err != nil || code != http.StatusOK {
		t.Fatalf("client put = %d, %v, want 200", code, err)
	}

	want := mediastore.Adoption{SizeBytes: int64(len(data)), SHA256: digestHex(data)}
	if err := s.Adopt(t.Context(), blobID, mediastore.Adoption{SizeBytes: want.SizeBytes + 1, SHA256: want.SHA256}, mediastore.Put{ContentType: "image/png"}); !errors.Is(err, mediastore.ErrSizeMismatch) {
		t.Fatalf("adopt with a wrong size = %v, want ErrSizeMismatch", err)
	}
	if err := s.Adopt(t.Context(), blobID, mediastore.Adoption{SizeBytes: want.SizeBytes, SHA256: strings.Repeat("0", 64)}, mediastore.Put{ContentType: "image/png"}); !errors.Is(err, mediastore.ErrDigestMismatch) {
		t.Fatalf("adopt with a wrong digest = %v, want ErrDigestMismatch", err)
	}
	rows, err := index.Blobs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("%d rows landed for failed completions, want none", len(rows))
	}
	if _, _, err := s.Open(t.Context(), blobID); !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("open after failed completions = %v, want ErrNotFound", err)
	}
}

// TestPresignedPutValidatesInput pins the issuer's refusals.
func TestPresignedPutValidatesInput(t *testing.T) {
	_, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	good := strings.Repeat("a", 32)

	if _, err := b.PresignPut(t.Context(), PresignPutInput{BlobID: "../escape", ContentType: "image/png", SizeBytes: 1}); !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("issuer on a malformed id = %v, want ErrNotFound", err)
	}
	if _, err := b.PresignPut(t.Context(), PresignPutInput{BlobID: good, SizeBytes: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("issuer with no content type = %v, want ErrInvalid", err)
	}
	if _, err := b.PresignPut(t.Context(), PresignPutInput{BlobID: good, ContentType: "image/png", SizeBytes: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("issuer with a negative size = %v, want ErrInvalid", err)
	}
	if _, err := b.PresignPut(t.Context(), PresignPutInput{BlobID: good, ContentType: "image/png", SizeBytes: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("issuer with no digest = %v, want ErrInvalid", err)
	}
	if _, err := b.PresignPut(t.Context(), PresignPutInput{BlobID: good, ContentType: "image/png", SizeBytes: 1, SHA256: "zz"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("issuer with a malformed digest = %v, want ErrInvalid", err)
	}
}

// TestMultipartHappyPath pins the multipart flow. The session opens in
// the bucket, every part presigns with its own length bound, completion
// assembles from the service's own listing, and the assembled object
// verifies and records like any other blob. The first part carries the
// dialect's minimum, and the last carries what is left under it.
func TestMultipartHappyPath(t *testing.T) {
	f, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	index := newMemIndex()
	s := openStore(t, b, index)
	blobID := strings.Repeat("3", 32)
	part1 := deterministic(MinPartSize)
	part2 := deterministic(128 << 10)
	whole := append(append([]byte{}, part1...), part2...)

	uploadID, err := b.StartUpload(t.Context(), blobID, "video/mp4")
	if err != nil {
		t.Fatalf("start upload: %v", err)
	}
	first, err := b.PresignPart(t.Context(), blobID, uploadID, 1, int64(len(part1)))
	if err != nil {
		t.Fatalf("presign part 1: %v", err)
	}
	second, err := b.PresignPart(t.Context(), blobID, uploadID, 2, int64(len(part2)))
	if err != nil {
		t.Fatalf("presign part 2: %v", err)
	}
	for i, part := range []struct {
		signed Presigned
		body   []byte
	}{
		{first, part1},
		{second, part2},
	} {
		code, err := statusOf(sendPut(client, part.signed, part.body, "", ""))
		if err != nil {
			t.Fatalf("put part %d: %v", i+1, err)
		}
		if code != http.StatusOK {
			t.Fatalf("put part %d = %d, want 200", i+1, code)
		}
	}

	// Completing before every part has arrived refuses and assembles
	// nothing.
	if err := b.CompleteUpload(t.Context(), blobID, uploadID, 3); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("complete with a part still missing = %v, want ErrIncomplete", err)
	}
	if f.holds(blobID) {
		t.Fatal("an incomplete upload assembled an object")
	}

	if err := b.CompleteUpload(t.Context(), blobID, uploadID, 2); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !f.holds(blobID) {
		t.Fatal("the assembled object is missing")
	}
	if f.uploadHeld(uploadID) {
		t.Fatal("the service still holds the upload after the assembly")
	}
	if err := s.Adopt(t.Context(), blobID, mediastore.Adoption{
		SizeBytes: int64(len(whole)),
		SHA256:    digestHex(whole),
	}, mediastore.Put{ContentType: "video/mp4"}); err != nil {
		t.Fatalf("adopt the assembly: %v", err)
	}
	r, _, err := s.Open(t.Context(), blobID)
	if err != nil {
		t.Fatalf("open the assembled blob: %v", err)
	}
	got, readErr := io.ReadAll(r)
	r.Close()
	if readErr != nil {
		t.Fatalf("read the assembled blob: %v", readErr)
	}
	if !bytes.Equal(got, whole) {
		t.Fatalf("assembled blob = %d bytes, want the %d the parts carry", len(got), len(whole))
	}
	if rows, err := index.Blobs(t.Context()); err != nil || len(rows) != 1 {
		t.Fatalf("rows = %d, %v, want one recorded blob", len(rows), err)
	}
}

// TestMultipartPartPinsItsLength pins the per-part bound. A part sent
// with a body of any other length is refused at the bucket, the same
// way a whole PUT is.
func TestMultipartPartPinsItsLength(t *testing.T) {
	_, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	blobID := strings.Repeat("7", 32)
	part := deterministic(64 << 10)

	uploadID, err := b.StartUpload(t.Context(), blobID, "video/mp4")
	if err != nil {
		t.Fatalf("start upload: %v", err)
	}
	signed, err := b.PresignPart(t.Context(), blobID, uploadID, 1, int64(len(part)))
	if err != nil {
		t.Fatalf("presign part: %v", err)
	}
	code, err := statusOf(sendPut(client, signed, deterministic(32), "", ""))
	if err != nil {
		t.Fatalf("put part of the wrong size: %v", err)
	}
	if code != http.StatusForbidden {
		t.Fatalf("put part of the wrong size = %d, want 403", code)
	}
}

// TestAssemblyRefusesPartsUnderTheFloor pins the dialect's minimum part
// size. A plan whose parts before the last sit under MinPartSize uploads
// cleanly but assembles nothing, and the service answers with the
// refusal the dialect spells for an undersized plan. The upload and its
// parts survive for a plan that respects the floor.
func TestAssemblyRefusesPartsUnderTheFloor(t *testing.T) {
	f, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	blobID := strings.Repeat("1", 32)
	small := deterministic(64 << 10)

	uploadID, err := b.StartUpload(t.Context(), blobID, "video/mp4")
	if err != nil {
		t.Fatalf("start upload: %v", err)
	}
	for n := int64(1); n <= 2; n++ {
		signed, err := b.PresignPart(t.Context(), blobID, uploadID, n, int64(len(small)))
		if err != nil {
			t.Fatalf("presign part %d: %v", n, err)
		}
		code, err := statusOf(sendPut(client, signed, small, "", ""))
		if err != nil || code != http.StatusOK {
			t.Fatalf("put part %d = %d, %v, want 200", n, code, err)
		}
	}

	err = b.CompleteUpload(t.Context(), blobID, uploadID, 2)
	var coded interface{ ErrorCode() string }
	if !errors.As(err, &coded) || coded.ErrorCode() != "EntityTooSmall" {
		t.Fatalf("complete under the floor = %v, want the dialect's too-small refusal", err)
	}
	if f.holds(blobID) {
		t.Fatal("an undersized plan assembled an object")
	}
	if !f.uploadHeld(uploadID) {
		t.Fatal("the refused assembly discarded the upload")
	}
}

// TestAbortRemovesPartsAndReadsTwiceAsDone pins the abort. The parts
// the upload held are discarded, the upload is gone, and a second
// abort reads as already done, which is what lets a sweep run on rows
// a completion left behind.
func TestAbortRemovesPartsAndReadsTwiceAsDone(t *testing.T) {
	f, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	blobID := strings.Repeat("b", 32)
	part := deterministic(32 << 10)

	uploadID, err := b.StartUpload(t.Context(), blobID, "video/mp4")
	if err != nil {
		t.Fatalf("start upload: %v", err)
	}
	signed, err := b.PresignPart(t.Context(), blobID, uploadID, 1, int64(len(part)))
	if err != nil {
		t.Fatalf("presign part: %v", err)
	}
	code, err := statusOf(sendPut(client, signed, part, "", ""))
	if err != nil || code != http.StatusOK {
		t.Fatalf("put part = %d, %v, want 200", code, err)
	}
	if got := f.partsOf(uploadID); len(got) != 1 {
		t.Fatalf("the upload holds %d parts, want 1", len(got))
	}

	if err := b.AbortUpload(t.Context(), blobID, uploadID); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if f.uploadHeld(uploadID) {
		t.Fatal("the upload survived the abort")
	}
	if got := f.partsOf(uploadID); len(got) != 0 {
		t.Fatalf("%d parts survived the abort", len(got))
	}
	if err := b.AbortUpload(t.Context(), blobID, uploadID); err != nil {
		t.Fatalf("second abort = %v, want a no-op", err)
	}
}

// TestCompleteTwiceMatchesNoSuchUpload pins what a completion that
// already ran through looks like, so a retried route knows to go and
// record the object instead.
func TestCompleteTwiceMatchesNoSuchUpload(t *testing.T) {
	_, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	blobID := strings.Repeat("c", 32)
	part := deterministic(64 << 10)

	uploadID, err := b.StartUpload(t.Context(), blobID, "video/mp4")
	if err != nil {
		t.Fatalf("start upload: %v", err)
	}
	signed, err := b.PresignPart(t.Context(), blobID, uploadID, 1, int64(len(part)))
	if err != nil {
		t.Fatalf("presign part: %v", err)
	}
	code, err := statusOf(sendPut(client, signed, part, "", ""))
	if err != nil || code != http.StatusOK {
		t.Fatalf("put part = %d, %v, want 200", code, err)
	}
	if err := b.CompleteUpload(t.Context(), blobID, uploadID, 1); err != nil {
		t.Fatalf("first complete: %v", err)
	}
	if err := b.CompleteUpload(t.Context(), blobID, uploadID, 1); !errors.Is(err, ErrNoSuchUpload) {
		t.Fatalf("second complete = %v, want ErrNoSuchUpload", err)
	}
}

// TestDirectUploadInputBounds pins the dialect bounds on parts and
// completions.
func TestDirectUploadInputBounds(t *testing.T) {
	_, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	blobID := strings.Repeat("d", 32)

	if _, err := b.StartUpload(t.Context(), "../escape", "video/mp4"); !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("start upload of a malformed id = %v, want ErrNotFound", err)
	}
	if _, err := b.StartUpload(t.Context(), blobID, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("start upload with no content type = %v, want ErrInvalid", err)
	}
	if _, err := b.PresignPart(t.Context(), blobID, "u1", 0, 8); !errors.Is(err, ErrInvalid) {
		t.Fatalf("presign of part zero = %v, want ErrInvalid", err)
	}
	if _, err := b.PresignPart(t.Context(), blobID, "u1", MaxParts+1, 8); !errors.Is(err, ErrInvalid) {
		t.Fatalf("presign past the part bound = %v, want ErrInvalid", err)
	}
	if _, err := b.PresignPart(t.Context(), blobID, "u1", 1, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("presign of an empty part = %v, want ErrInvalid", err)
	}
	if err := b.CompleteUpload(t.Context(), blobID, "u1", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("complete with no parts = %v, want ErrInvalid", err)
	}
	if err := b.CompleteUpload(t.Context(), blobID, "u1", MaxParts+1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("complete past the part bound = %v, want ErrInvalid", err)
	}
}

// TestDirectUploadFaultsNeverSpellTheCredential pins the credential
// rule over the new calls. Presigning works against a dead service,
// and every fault the bucket calls produce names no credential.
func TestDirectUploadFaultsNeverSpellTheCredential(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	client := srv.Client()
	endpoint := srv.URL
	srv.Close()
	client.CloseIdleConnections()
	b, err := Open(t.Context(), Config{
		Endpoint:  endpoint,
		Bucket:    testBucket,
		Region:    testRegion,
		AccessKey: testAccess,
		SecretKey: testSecret,
		PathStyle: true,
		Client:    client,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	blobID := strings.Repeat("e", 32)

	signed, err := b.PresignPut(t.Context(), PresignPutInput{
		BlobID:      blobID,
		ContentType: "image/png",
		SizeBytes:   8,
		SHA256:      digestHex(deterministic(8)),
	})
	if err != nil {
		t.Fatalf("presign over a dead endpoint = %v, want a local success", err)
	}
	// The access key id rides the URL in the credential parameter the
	// signing dialect places there, so the secret is the only
	// credential that must never appear.
	if strings.Contains(signed.URL, testSecret) {
		t.Fatal("the presigned URL spells the secret")
	}
	for name, value := range signed.Headers {
		if strings.Contains(value, testSecret) {
			t.Fatalf("header %s spells the secret", name)
		}
	}

	partSigned, err := b.PresignPart(t.Context(), blobID, "u1", 1, 8)
	if err != nil {
		t.Fatalf("presign part over a dead endpoint = %v, want a local success", err)
	}
	if strings.Contains(partSigned.URL, testSecret) {
		t.Fatal("the presigned part URL spells the secret")
	}

	probes := []struct {
		name  string
		probe func() error
	}{
		{"start upload", func() error {
			_, err := b.StartUpload(t.Context(), blobID, "image/png")
			return err
		}},
		{"complete upload", func() error {
			return b.CompleteUpload(t.Context(), blobID, "u1", 1)
		}},
		{"abort upload", func() error {
			return b.AbortUpload(t.Context(), blobID, "u1")
		}},
	}
	for _, p := range probes {
		err := p.probe()
		if err == nil {
			t.Fatalf("%s survived a dead endpoint", p.name)
		}
		if strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), testAccess) {
			t.Fatalf("%s spelled the credential: %v", p.name, err)
		}
	}
}

// TestDirectUploadsAreOptIn pins the opt-in rule. Presigning lives on
// the concrete type and no interface the store drives can issue a URL,
// and the store handler streams through the backend instead of
// answering any request with a bucket address.
func TestDirectUploadsAreOptIn(t *testing.T) {
	_, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	s := openStore(t, b, newMemIndex())
	data := deterministic(128)
	blobID, err := s.Persist(t.Context(), bytes.NewReader(data), mediastore.Put{
		ContentType: "image/png",
		Visibility:  mediastore.Public,
	})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}

	backend := reflect.TypeOf((*mediastore.Backend)(nil)).Elem()
	for i := 0; i < backend.NumMethod(); i++ {
		if name := backend.Method(i).Name; strings.Contains(name, "Presign") {
			t.Fatalf("the backend interface carries %s, issuing is a concrete-type act", name)
		}
	}

	mux := http.NewServeMux()
	mux.Handle("GET /media/{id}", s)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/media/" + blobID)
	if err != nil {
		t.Fatalf("get through the store: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read through the store: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("get through the store = %d with %d bytes, want 200 with %d", resp.StatusCode, len(body), len(data))
	}
	if resp.Header.Get("Location") != "" {
		t.Fatal("the store handler answered a read with a redirect")
	}
	if strings.Contains(string(body), endpointHost(endpoint)) {
		t.Fatal("the store handler answered with a body naming the bucket")
	}
}

// endpointHost returns the host of an endpoint URL.
func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	return u.Host
}
