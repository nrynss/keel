package s3

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	aws "github.com/aws/aws-sdk-go-v2/aws"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// MaxParts is the most parts one multipart upload can carry. The
// dialect fixes the bound, so a session that plans more parts than this
// can open but can never complete.
const MaxParts = 10000

// MinPartSize is the size in bytes every part of a multipart upload
// carries except the last, which carries whatever the declared
// whole-file size leaves over. The service refuses an assembly that
// holds an earlier part under this size, so a plan keeps every part
// before the last at or above it.
const MinPartSize = 5 << 20

// ErrIncomplete is returned by CompleteUpload when the service holds
// fewer parts than the session planned. The upload keeps its parts and
// the object does not exist yet.
var ErrIncomplete = errors.New("s3: the upload is missing parts")

// ErrNoSuchUpload is returned by CompleteUpload when the service holds
// no upload under the id given. A completion that ran twice lands here
// on its second pass, because the service discards the upload once the
// parts are assembled.
var ErrNoSuchUpload = errors.New("s3: the service holds no such upload")

// Codes a direct-upload refusal carries in the shared error envelope.
// The issuer, the completion and the session routes live in the app,
// which writes the envelope itself and branches its client on the code.
const (
	// CodeInvalidRequest marks a request field the route will not
	// accept.
	CodeInvalidRequest = "invalid_request"
	// CodeNotFound marks an id or a session nothing is known about.
	CodeNotFound = "not_found"
	// CodeConflict marks an id that already names stored bytes.
	CodeConflict = "conflict"
	// CodeUnsupportedType marks a content type outside the store's set.
	CodeUnsupportedType = "unsupported_type"
	// CodeSizeMismatch marks stored bytes that do not add up to the
	// size the upload declared. Completion answers with it for an
	// error matching mediastore.ErrSizeMismatch.
	CodeSizeMismatch = "size_mismatch"
	// CodeHashMismatch marks stored bytes that do not hash to the
	// digest the upload declared. Completion answers with it for an
	// error matching mediastore.ErrDigestMismatch.
	CodeHashMismatch = "hash_mismatch"
	// CodeIncomplete marks a multipart upload whose parts have not all
	// arrived. It answers for an error matching ErrIncomplete.
	CodeIncomplete = "incomplete"
	// CodeStorageError marks a fault on this side of the wire.
	CodeStorageError = "storage_error"
)

// Presigned is a URL a client writes bytes through, the headers the
// request must carry, and the moment the service closes the URL. The
// headers are sent verbatim: a header name is case-insensitive the way
// HTTP names are, a header value is not.
type Presigned struct {
	// URL is the presigned URL. It carries the signature and the
	// credential's access key id, which the signing dialect places in
	// every URL it signs, and it never carries the secret.
	URL string
	// Headers holds every header the request must send. A missing or
	// altered value breaks the signature and the service refuses the
	// request before it reads a byte.
	Headers map[string]string
	// Expires is the instant the service stops honouring the URL. It is
	// read back from the parameters the URL itself carries, so it is
	// the expiry the service enforces and not a local guess.
	Expires time.Time
}

// PresignPutInput names the shape one direct PUT is bound to.
type PresignPutInput struct {
	// BlobID is the id the object is stored under. The client holds it
	// already, and completion records the blob under it.
	BlobID string
	// ContentType is the type the object is stored as. It signs as a
	// header, so the service refuses a PUT that carries any other
	// type.
	ContentType string
	// SizeBytes is the exact size the client declared for its bytes.
	// It signs as a header, so the service refuses a PUT of any other
	// length. The route that issues the URL applies its own byte cap
	// to this declared size before it calls, because a presigned URL
	// skips every check the proxied path passes through.
	SizeBytes int64
	// SHA256 is the digest of the client's bytes as 64 hex characters.
	// It signs as the dialect's checksum header, so the service refuses
	// bytes of any other shape before they land, and a URL that
	// outlives its completion can put the verified bytes again but
	// never any others. A service that ignores the header is caught at
	// completion, which never trusts the bucket's own accounting.
	SHA256 string
}

// PresignPut returns a URL the client puts its bytes through, the
// headers the request must carry, and the moment the service closes it.
// The declared size and the content type sign as headers, and the
// declared digest signs as the dialect's checksum header, so the bucket
// itself refuses a PUT of any other length, type or shape for as long
// as the URL lives. A completion that verified the object therefore
// leaves the URL nothing to substitute: the store can only ever serve
// bytes that hash to the digest. Presigning talks to no service, so it
// works against a dead one and the URL reads as a missing object until
// the bytes arrive. The secret signs the URL and appears in no part of
// it.
func (b *Backend) PresignPut(ctx context.Context, in PresignPutInput) (Presigned, error) {
	if !validKey(in.BlobID) {
		return Presigned{}, notFound(in.BlobID, errors.New("malformed id"))
	}
	if in.ContentType == "" {
		return Presigned{}, fmt.Errorf("%w: ContentType must not be empty", ErrInvalid)
	}
	if in.SizeBytes < 0 {
		return Presigned{}, fmt.Errorf("%w: SizeBytes must not be negative", ErrInvalid)
	}
	if in.SHA256 == "" {
		return Presigned{}, fmt.Errorf("%w: SHA256 is required", ErrInvalid)
	}
	if !validDigest(in.SHA256) {
		return Presigned{}, fmt.Errorf("%w: SHA256 must be 64 hex characters", ErrInvalid)
	}
	put := &s3sdk.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(in.BlobID),
		ContentType:   aws.String(in.ContentType),
		ContentLength: aws.Int64(in.SizeBytes),
	}
	headers := map[string]string{
		"Content-Type":   in.ContentType,
		"Content-Length": strconv.FormatInt(in.SizeBytes, 10),
	}
	sum := checksumHeader(in.SHA256)
	put.ChecksumSHA256 = aws.String(sum)
	headers["x-amz-checksum-sha256"] = sum
	req, err := b.presign.PresignPutObject(ctx, put, s3sdk.WithPresignExpires(b.expiry))
	if err != nil {
		return Presigned{}, fmt.Errorf("s3: presign put %s: %w", in.BlobID, err)
	}
	return presignedFrom(req.URL, headers)
}

// PresignPart returns a URL the client uploads one part of a multipart
// upload through. The part number and the upload id travel inside the
// URL, so the signature binds them and a URL moved to another part or
// another upload reads as tampered. The part size signs as a header,
// the same way a whole PUT pins its length, and the last part carries
// whatever the declared whole-file size leaves over. PresignPart
// accepts any positive size, because the caller knows which part is
// last and the dialect lets that one sit under the floor. Every part
// before the last carries at least MinPartSize, which the service
// checks when the parts assemble, so a plan under the floor refuses at
// completion and not at issue.
func (b *Backend) PresignPart(ctx context.Context, blobID, uploadID string, partNumber, sizeBytes int64) (Presigned, error) {
	if !validKey(blobID) {
		return Presigned{}, notFound(blobID, errors.New("malformed id"))
	}
	if uploadID == "" {
		return Presigned{}, fmt.Errorf("%w: the upload id must not be empty", ErrInvalid)
	}
	if partNumber < 1 || partNumber > MaxParts {
		return Presigned{}, fmt.Errorf("%w: part number %d is outside 1 to %d", ErrInvalid, partNumber, MaxParts)
	}
	if sizeBytes < 1 {
		return Presigned{}, fmt.Errorf("%w: a part carries at least one byte", ErrInvalid)
	}
	req, err := b.presign.PresignUploadPart(ctx, &s3sdk.UploadPartInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(blobID),
		UploadId:      aws.String(uploadID),
		PartNumber:    aws.Int32(int32(partNumber)),
		ContentLength: aws.Int64(sizeBytes),
	}, s3sdk.WithPresignExpires(b.expiry))
	if err != nil {
		return Presigned{}, fmt.Errorf("s3: presign part %d of %s: %w", partNumber, blobID, err)
	}
	return presignedFrom(req.URL, map[string]string{
		"Content-Length": strconv.FormatInt(sizeBytes, 10),
	})
}

// StartUpload opens a multipart upload in the bucket for blobID and
// returns the service's upload id. The call carries the content type,
// which the completed object inherits. Session state about the parts,
// the plan and the deadline lives in the app's database, because the
// bucket's own record names nothing but the parts it holds.
func (b *Backend) StartUpload(ctx context.Context, blobID, contentType string) (string, error) {
	if !validKey(blobID) {
		return "", notFound(blobID, errors.New("malformed id"))
	}
	if contentType == "" {
		return "", fmt.Errorf("%w: ContentType must not be empty", ErrInvalid)
	}
	out, err := b.client.CreateMultipartUpload(ctx, &s3sdk.CreateMultipartUploadInput{
		Bucket:      aws.String(b.bucket),
		Key:         aws.String(blobID),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("s3: start upload %s: %w", blobID, err)
	}
	uploadID := aws.ToString(out.UploadId)
	if uploadID == "" {
		return "", fmt.Errorf("s3: start upload %s: the service returned no upload id", blobID)
	}
	return uploadID, nil
}

// CompleteUpload assembles the parts the session planned into the
// object under blobID. The manifest is built from the service's own
// part listing, so the caller passes the planned part count and
// nothing else, and a part the plan never named is left for the
// service to discard. A part that has not arrived refuses with an
// error matching ErrIncomplete, and the upload keeps every part it
// holds. A service that no longer holds the upload refuses with an
// error matching ErrNoSuchUpload, which is what a completion that
// already ran through looks like, and the caller's next stop is
// recording the object.
func (b *Backend) CompleteUpload(ctx context.Context, blobID, uploadID string, partCount int) error {
	if !validKey(blobID) {
		return notFound(blobID, errors.New("malformed id"))
	}
	if uploadID == "" {
		return fmt.Errorf("%w: the upload id must not be empty", ErrInvalid)
	}
	if partCount < 1 || partCount > MaxParts {
		return fmt.Errorf("%w: %d parts is outside 1 to %d", ErrInvalid, partCount, MaxParts)
	}
	etags, err := b.listParts(ctx, blobID, uploadID)
	if err != nil {
		return err
	}
	manifest := make([]types.CompletedPart, 0, partCount)
	for n := 1; n <= partCount; n++ {
		etag, ok := etags[n]
		if !ok {
			return fmt.Errorf("s3: complete upload %s: %w: part %d has not arrived", blobID, ErrIncomplete, n)
		}
		manifest = append(manifest, types.CompletedPart{
			PartNumber: aws.Int32(int32(n)),
			ETag:       aws.String(etag),
		})
	}
	if _, err := b.client.CompleteMultipartUpload(ctx, &s3sdk.CompleteMultipartUploadInput{
		Bucket:          aws.String(b.bucket),
		Key:             aws.String(blobID),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: manifest},
	}); err != nil {
		if uploadAbsent(err) {
			return fmt.Errorf("s3: complete upload %s: %w: %w", blobID, ErrNoSuchUpload, err)
		}
		return fmt.Errorf("s3: complete upload %s: %w", blobID, err)
	}
	return nil
}

// AbortUpload discards every part of uploadID and closes it. A service
// that no longer holds the upload reads as already aborted, so an
// abort after a completion that went through is a no-op and not an
// error, and a sweep can run the same way on rows the completion left
// behind.
func (b *Backend) AbortUpload(ctx context.Context, blobID, uploadID string) error {
	if !validKey(blobID) {
		return notFound(blobID, errors.New("malformed id"))
	}
	if uploadID == "" {
		return fmt.Errorf("%w: the upload id must not be empty", ErrInvalid)
	}
	_, err := b.client.AbortMultipartUpload(ctx, &s3sdk.AbortMultipartUploadInput{
		Bucket:   aws.String(b.bucket),
		Key:      aws.String(blobID),
		UploadId: aws.String(uploadID),
	})
	if err != nil {
		if uploadAbsent(err) {
			return nil
		}
		return fmt.Errorf("s3: abort upload %s: %w", blobID, err)
	}
	return nil
}

// listParts returns the ETag of every part the service holds for the
// upload, keyed by part number.
func (b *Backend) listParts(ctx context.Context, blobID, uploadID string) (map[int]string, error) {
	etags := map[int]string{}
	pages := s3sdk.NewListPartsPaginator(b.client, &s3sdk.ListPartsInput{
		Bucket:   aws.String(b.bucket),
		Key:      aws.String(blobID),
		UploadId: aws.String(uploadID),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			if uploadAbsent(err) {
				return nil, fmt.Errorf("s3: list parts %s: %w: %w", blobID, ErrNoSuchUpload, err)
			}
			return nil, fmt.Errorf("s3: list parts %s: %w", blobID, err)
		}
		for _, part := range page.Parts {
			etags[int(aws.ToInt32(part.PartNumber))] = aws.ToString(part.ETag)
		}
	}
	return etags, nil
}

// validDigest reports whether s is a SHA-256 digest as 64 hex
// characters.
func validDigest(s string) bool {
	if len(s) != 2*sha256.Size {
		return false
	}
	_, err := hex.DecodeString(strings.ToLower(s))
	return err == nil
}

// checksumHeader converts a hex digest into the base64 form the
// dialect's checksum header carries. The digest is already validated.
func checksumHeader(digest string) string {
	raw, _ := hex.DecodeString(strings.ToLower(digest)) // validDigest checked the shape
	return base64.StdEncoding.EncodeToString(raw)
}

// presignedFrom builds the answer for a presigned write. The expiry is
// read back from the parameters the URL carries, so it is the expiry
// the service enforces rather than the configured duration the URL was
// stamped with.
func presignedFrom(raw string, headers map[string]string) (Presigned, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Presigned{}, fmt.Errorf("s3: presign: %w", err)
	}
	q := u.Query()
	stamped, err := time.Parse("20060102T150405Z", q.Get("X-Amz-Date"))
	if err != nil {
		return Presigned{}, fmt.Errorf("s3: presign: %w", err)
	}
	seconds, err := strconv.ParseInt(q.Get("X-Amz-Expires"), 10, 64)
	if err != nil {
		return Presigned{}, fmt.Errorf("s3: presign: %w", err)
	}
	return Presigned{
		URL:     raw,
		Headers: headers,
		Expires: stamped.Add(time.Duration(seconds) * time.Second),
	}, nil
}

// uploadAbsent reports whether err is the service saying the upload is
// not there. A fault of any other kind stays a fault, so an outage can
// never read as a finished upload.
func uploadAbsent(err error) bool {
	var coded interface{ ErrorCode() string }
	return errors.As(err, &coded) && coded.ErrorCode() == "NoSuchUpload"
}
