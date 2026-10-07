// Package s3 stores mediastore blob bytes in an S3-compatible object
// store.
//
// The Backend here implements mediastore.Backend and reaches the store
// through mediastore.Config.Backend, so an app that keeps its bytes on
// disk never compiles this package or its client. The client speaks
// the S3 REST dialect over one configured endpoint, and it constructs
// itself from Config. The package reads no environment and resolves no
// secrets.
//
// The store writes the metadata row only after Backend.Write returns,
// so the bytes-before-row order holds over this backend too. Persist
// names an object only after the service acknowledged the put. What
// the service promises past that acknowledgement is the storage's own
// durability and not this package's.
//
// Presigning lives on the concrete type rather than on the interface,
// because a short-lived read URL is a capability a bucket has and a
// directory has not. The app authorizes its own request, asks
// PresignGet for a URL, and answers with a redirect. A deployment that
// cannot redirect serves through the store handler, which streams and
// seeks through this backend, so Range requests work in that proxy
// mode as they do on disk.
//
// The bucket is the backend's alone. Every object sits at the bucket
// root named by its blob id, so the listing the orphan sweep consumes
// is the whole bucket and nothing else is kept there.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	aws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/nrynss/keel/mediastore"
)

// ErrInvalid is returned by Open for a configuration it cannot
// honour: a missing endpoint, bucket, region or credential, or a
// presign expiry outside the range the service accepts.
var ErrInvalid = errors.New("s3: invalid config")

// defaultPresignExpiry is how long a presigned read URL stays good
// when Config leaves PresignExpiry at zero. Fifteen minutes is long
// enough for a reader to follow a redirect and short enough that a
// leaked link closes on its own.
const defaultPresignExpiry = 15 * time.Minute

// maxPresignExpiry is the longest expiry a presigned URL can carry.
// One week is the most the service honours on a signed read.
const maxPresignExpiry = 7 * 24 * time.Hour

// Config configures Open.
type Config struct {
	// Endpoint is the base URL of the S3-compatible service. It must
	// not be empty.
	Endpoint string
	// Bucket is the one bucket this backend owns. It must not be
	// empty. The backend keeps every object at the bucket root named
	// by its blob id, and it shares the bucket with nothing else.
	Bucket string
	// Region is the signing region the service expects. It must not be
	// empty. Many S3-compatible services accept any string here, and
	// the signature covers whatever this names.
	Region string
	// AccessKey and SecretKey are the credential the service accepts.
	// The app resolves them through its own settings and injects them
	// here, so the package reads no environment and never learns where
	// they came from. Both must not be empty. The secret appears in no
	// log line and no error string this package writes.
	AccessKey string
	SecretKey string
	// PresignExpiry is how long a presigned read URL stays good. Zero
	// means defaultPresignExpiry. The service honours at most
	// maxPresignExpiry, and anything past that is ErrInvalid.
	PresignExpiry time.Duration
	// PathStyle addresses the bucket in the URL path, as
	// endpoint/bucket/key. Many S3-compatible services serve only that
	// shape. False addresses the bucket as a subdomain of the
	// endpoint, which a bucket host with a name for every tenant
	// serves.
	PathStyle bool
	// Client is the HTTP client the SDK runs on. Nil means the one the
	// SDK builds. An operator points it at a proxy or at a client that
	// trusts an internal certificate authority.
	Client *http.Client
}

// Backend stores mediastore blob bytes in one S3-compatible bucket.
// Create it with Open and hand it to mediastore.Config.Backend.
// Backend is safe for concurrent use.
type Backend struct {
	client  *s3sdk.Client
	presign *s3sdk.PresignClient
	bucket  string
	expiry  time.Duration
}

// Open validates cfg and builds the client over the configured
// endpoint. It talks to no service, so a wrong endpoint or credential
// surfaces on the first call rather than here.
func Open(_ context.Context, cfg Config) (*Backend, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.Region == "" {
		return nil, fmt.Errorf("%w: Endpoint, Bucket and Region must not be empty", ErrInvalid)
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("%w: AccessKey and SecretKey must not be empty", ErrInvalid)
	}
	if cfg.PresignExpiry < 0 {
		return nil, fmt.Errorf("%w: PresignExpiry must not be negative", ErrInvalid)
	}
	if cfg.PresignExpiry > maxPresignExpiry {
		return nil, fmt.Errorf("%w: PresignExpiry is past the %s a signed read honours", ErrInvalid, maxPresignExpiry)
	}
	expiry := cfg.PresignExpiry
	if expiry == 0 {
		expiry = defaultPresignExpiry
	}
	awsCfg := aws.Config{
		Region:       cfg.Region,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		BaseEndpoint: aws.String(cfg.Endpoint),
		// One attempt per call. A retry would re-send a whole media
		// body after a partial failure. Deciding that a call is worth
		// repeating belongs to the store's caller, not to the client
		// underneath it.
		RetryMaxAttempts: 1,
		HTTPClient:       cfg.Client,
	}
	client := s3sdk.NewFromConfig(awsCfg, func(o *s3sdk.Options) {
		o.UsePathStyle = cfg.PathStyle
		// Checksums only when a call asks for one. The default has the
		// client wrap every put in the chunked encoding its checksum
		// rides in, and an S3-compatible service that never grew that
		// dialect would refuse every write. With it off, a put is the
		// plain bytes the classic dialect spells.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	return &Backend{
		client:  client,
		presign: s3sdk.NewPresignClient(client),
		bucket:  cfg.Bucket,
		expiry:  expiry,
	}, nil
}

// Open returns the stored bytes of id for reading. The reader seeks by
// asking the service for the range it still needs, so the store
// handler's Range answers and every copy work through it. An error
// matching mediastore.ErrNotFound reports that id has no stored bytes.
func (b *Backend) Open(ctx context.Context, blobID string) (mediastore.BlobReader, error) {
	if !validKey(blobID) {
		return nil, notFound(blobID, errors.New("malformed id"))
	}
	head, err := b.client.HeadObject(ctx, &s3sdk.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(blobID),
	})
	if err != nil {
		if absent(err) {
			return nil, notFound(blobID, err)
		}
		return nil, fmt.Errorf("s3: head %s: %w", blobID, err)
	}
	return &objectReader{
		ctx:    ctx,
		blobID: blobID,
		size:   aws.ToInt64(head.ContentLength),
		get:    b.get,
	}, nil
}

// Write stores src as the bytes named id and returns the number of
// bytes it read from src. An object already under the id is refused
// with an error matching mediastore.ErrAlreadyExists, and the stored
// bytes stay untouched. The check and the put are two calls, so this
// is weaker than the disk backend's exclusive create: a concurrent
// writer can take the name between them. The client can carry a
// conditional put, and a service that honours it would close that
// window. This backend does not send one. The promise here is
// S3-compatible breadth, and a dialect that silently drops the unknown
// header would overwrite instead of refusing, which is the exact
// damage the refusal exists to prevent. The bytes are durable under
// the service's own guarantees before it acknowledges the put, and the
// store writes the metadata row only after.
func (b *Backend) Write(ctx context.Context, blobID string, src io.Reader) (int64, error) {
	if !validKey(blobID) {
		return 0, fmt.Errorf("s3: write %s: %w: malformed id", blobID, mediastore.ErrNotFound)
	}
	head, err := b.client.HeadObject(ctx, &s3sdk.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(blobID),
	})
	if err == nil {
		return 0, fmt.Errorf("s3: write %s: %w: the service already holds %d bytes there", blobID, mediastore.ErrAlreadyExists, aws.ToInt64(head.ContentLength))
	}
	if !absent(err) {
		return 0, fmt.Errorf("s3: write %s: head: %w", blobID, err)
	}
	counted := &countingReader{r: src}
	if _, err := b.client.PutObject(ctx, &s3sdk.PutObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(blobID),
		Body:   counted,
	}); err != nil {
		return 0, fmt.Errorf("s3: write %s: put: %w", blobID, err)
	}
	return counted.n, nil
}

// Delete removes the stored bytes of id. An error matching
// mediastore.ErrNotFound reports that id has no stored bytes. The
// service answers a delete for an absent object with success, so the
// head first is what makes an absence visible to the caller that
// cleans up a maybe-removed blob.
func (b *Backend) Delete(ctx context.Context, blobID string) error {
	if !validKey(blobID) {
		return notFound(blobID, errors.New("malformed id"))
	}
	_, err := b.client.HeadObject(ctx, &s3sdk.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(blobID),
	})
	if err != nil {
		if absent(err) {
			return notFound(blobID, err)
		}
		return fmt.Errorf("s3: delete %s: head: %w", blobID, err)
	}
	if _, err := b.client.DeleteObject(ctx, &s3sdk.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(blobID),
	}); err != nil {
		return fmt.Errorf("s3: delete %s: %w", blobID, err)
	}
	return nil
}

// List returns every object the bucket holds, each with the service's
// own modification time for the orphan sweep to age by. The bucket is
// this backend's alone, so the listing is the whole of it.
func (b *Backend) List(ctx context.Context) ([]mediastore.Object, error) {
	var out []mediastore.Object
	pages := s3sdk.NewListObjectsV2Paginator(b.client, &s3sdk.ListObjectsV2Input{Bucket: aws.String(b.bucket)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3: list: %w", err)
		}
		for _, o := range page.Contents {
			entry := mediastore.Object{
				ID:        aws.ToString(o.Key),
				SizeBytes: aws.ToInt64(o.Size),
			}
			if o.LastModified != nil {
				entry.ModTime = *o.LastModified
			}
			out = append(out, entry)
		}
	}
	return out, nil
}

// PresignGet returns a short-lived URL that reads the stored bytes of
// id without this process in the path. The URL carries an expiry from
// Config and a signature the service verifies. The caller authorizes
// first and redirects after, so the URL goes to a reader the app
// already admitted. Presigning talks to no service, so an absent id
// presigns happily and the URL reads as a missing object when someone
// follows it. The secret signs the URL and appears in no part of it.
func (b *Backend) PresignGet(ctx context.Context, blobID string) (string, error) {
	if !validKey(blobID) {
		return "", notFound(blobID, errors.New("malformed id"))
	}
	req, err := b.presign.PresignGetObject(ctx, &s3sdk.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(blobID),
	}, s3sdk.WithPresignExpires(b.expiry))
	if err != nil {
		return "", fmt.Errorf("s3: presign %s: %w", blobID, err)
	}
	return req.URL, nil
}

// get fetches the object from offset to the end and returns its body.
func (b *Backend) get(ctx context.Context, blobID string, offset int64) (io.ReadCloser, error) {
	out, err := b.client.GetObject(ctx, &s3sdk.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(blobID),
		Range:  aws.String("bytes=" + strconv.FormatInt(offset, 10) + "-"),
	})
	if err != nil {
		return nil, fmt.Errorf("s3: get %s from %d: %w", blobID, offset, err)
	}
	return out.Body, nil
}

// notFound wraps err so an absent object matches the package sentinel
// the store classifies on, the way every implementation of the seam
// must.
func notFound(key string, err error) error {
	return fmt.Errorf("s3: object %s: %w: %w", key, mediastore.ErrNotFound, err)
}

// absent reports whether err is the service saying the object is not
// there. A fault of any other kind stays a fault, so an outage can
// never read as an empty bucket.
func absent(err error) bool {
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		switch coded.ErrorCode() {
		case "NotFound", "NoSuchKey":
			return true
		}
	}
	return false
}

// validKey reports whether name can serve as an object key. The store
// only hands out ids the id package produced. A key with a slash in it
// would sit outside the flat namespace this backend promises, so
// anything else is refused at the door.
func validKey(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.Contains(name, "/")
}

// countingReader counts the bytes a put streamed, which is the size
// the store records on the row.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// objectReader reads one object through ranged gets. Open learns the
// size from a head and fetches nothing until the first read, so the
// handler's seek around for a size transfers no body. A seek closes
// the body in flight and the next read resumes from the new offset. A
// seek past the end reports the position it was asked for and reads
// EOF, the way a file handle behaves. Both backends then answer the
// one Store API with one shape.
type objectReader struct {
	ctx    context.Context
	blobID string
	size   int64
	offset int64
	body   io.ReadCloser
	get    func(ctx context.Context, blobID string, offset int64) (io.ReadCloser, error)
}

func (o *objectReader) Read(p []byte) (int, error) {
	if o.body == nil {
		if o.offset >= o.size {
			return 0, io.EOF
		}
		body, err := o.get(o.ctx, o.blobID, o.offset)
		if err != nil {
			return 0, err
		}
		o.body = body
	}
	n, err := o.body.Read(p)
	o.offset += int64(n)
	if err == io.EOF {
		o.body.Close() // best effort, the object is fully read either way
		o.body = nil
	}
	return n, err
}

func (o *objectReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += o.offset
	case io.SeekEnd:
		offset += o.size
	default:
		return 0, fmt.Errorf("s3: seek %s: whence %d", o.blobID, whence)
	}
	if offset < 0 {
		return 0, fmt.Errorf("s3: seek %s: before the start of the object", o.blobID)
	}
	if offset != o.offset {
		if o.body != nil {
			o.body.Close() // best effort, the next read opens a fresh range
			o.body = nil
		}
		o.offset = offset
	}
	return o.offset, nil
}

func (o *objectReader) Close() error {
	if o.body == nil {
		return nil
	}
	err := o.body.Close()
	o.body = nil
	return err
}
