package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/mediastore"
)

// The credential this whole test file runs on. The tests assert the
// secret never reaches a URL or an error, so it carries a shape a
// search would find.
const (
	testAccess = "test-access-key"
	testSecret = "test-secret-key-never-prints"
	testRegion = "eu-test-1"
	testBucket = "media"
)

// fakeS3 speaks enough of the S3 REST dialect for the operations the
// backend uses. It stores objects in memory with backdatable
// modification times, answers ranged gets, and lists the bucket. It
// verifies the full signature and expiry of a presigned request the way
// a service does before it acts on it, which also enforces every header
// the signature binds, and it validates a signed checksum against the
// body the way a service that honours checksums does. It runs multipart
// uploads: create, part put, part listing, complete and abort, with the
// parts held beside the object namespace and an assembled upload
// discarding its parts. It is deterministic and reaches no network
// beyond the httptest server that carries it.
type fakeS3 struct {
	mu        sync.Mutex
	objects   map[string][]byte
	modTimes  map[string]time.Time
	uploads   map[string]*fakeUpload
	uploadSeq int
	denied    map[string]bool
	region    string
	secret    string
	now       func() time.Time
	events    *recorder
}

// fakeUpload is one multipart upload the service holds: its key and
// the parts that arrived for it, keyed by part number.
type fakeUpload struct {
	key   string
	parts map[int][]byte
}

// errExpired marks a signature that verified but outlived its expiry,
// so the double can answer it with the expired shape.
var errExpired = errors.New("presigned read has expired")

// newFakeS3 starts the double on an httptest TLS server and returns it
// with the endpoint a backend is pointed at and the client that trusts
// the double's own certificate. TLS is the shape a real deployment
// runs, and the client signs its puts with an unsigned payload over
// it, so the double reads the plain bytes.
func newFakeS3(t *testing.T, now func() time.Time) (*fakeS3, string, *http.Client) {
	t.Helper()
	f := &fakeS3{
		objects:  map[string][]byte{},
		modTimes: map[string]time.Time{},
		uploads:  map[string]*fakeUpload{},
		region:   testRegion,
		secret:   testSecret,
		now:      now,
	}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL, srv.Client()
}

// setNow moves the double's clock, which is how a test ages a
// presigned URL past its expiry without sleeping.
func (f *fakeS3) setNow(now func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

// backdate stamps one object's modification time directly, which is
// how a test seeds an object the sweep ages by the service's own time.
func (f *fakeS3) backdate(key string, modTime time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modTimes[key] = modTime
}

// deny makes the double answer every read of key with the service's
// permission-refused shape, which is how a test drives a fault that is
// not an absence.
func (f *fakeS3) deny(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.denied == nil {
		f.denied = map[string]bool{}
	}
	f.denied[key] = true
}

// holds reports whether the double stores an object under key.
func (f *fakeS3) holds(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

// bodyOf returns the stored bytes of key.
func (f *fakeS3) bodyOf(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[key]
}

// partsOf returns the parts an upload holds, keyed by part number. An
// upload the service no longer holds reads as holding none.
func (f *fakeS3) partsOf(uploadID string) map[int][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int][]byte{}
	upload := f.uploads[uploadID]
	if upload == nil {
		return out
	}
	for number, body := range upload.parts {
		out[number] = body
	}
	return out
}

// uploadHeld reports whether the service still holds the upload.
func (f *fakeS3) uploadHeld(uploadID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.uploads[uploadID]
	return ok
}

// ServeHTTP routes one request. The lock is held for the whole
// request, so a test that moves the clock or reads a map never races
// an in-flight handler.
func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bucket, key, ok := splitPath(r.URL.Path)
	if !ok || bucket != testBucket {
		http.Error(w, "no such bucket", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	switch {
	case q.Get("X-Amz-Signature") != "":
		f.servePresigned(w, r, key)
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.createUpload(w, key)
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		f.completeUpload(w, r, key)
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		f.abortUpload(w, r, key)
	case r.Method == http.MethodGet && q.Get("uploadId") != "":
		f.listParts(w, key, q.Get("uploadId"))
	case r.Method == http.MethodPut && key != "":
		f.put(w, r, key)
	case r.Method == http.MethodHead && key != "":
		f.head(w, key)
	case r.Method == http.MethodGet && key != "":
		f.get(w, r, key)
	case r.Method == http.MethodDelete && key != "":
		f.delete(w, key)
	case r.Method == http.MethodGet && key == "":
		f.list(w, bucket)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// splitPath splits /bucket/key. A path with one segment names the
// bucket alone, which is a listing.
func splitPath(path string) (bucket, key string, ok bool) {
	trimmed := strings.TrimPrefix(path, "/")
	bucket, rest, found := strings.Cut(trimmed, "/")
	if bucket == "" {
		return "", "", false
	}
	if !found {
		return bucket, "", true
	}
	if strings.Contains(rest, "/") {
		return "", "", false
	}
	return bucket, rest, true
}

// put stores the body under key and stamps the double's own clock.
func (f *fakeS3) put(w http.ResponseWriter, r *http.Request, key string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	f.objects[key] = body
	f.modTimes[key] = f.now()
	if f.events != nil {
		f.events.add("put " + key)
	}
	w.Header().Set("ETag", etagFor(body))
	w.WriteHeader(http.StatusOK)
}

// head answers what a caller learns before it reads or deletes.
func (f *fakeS3) head(w http.ResponseWriter, key string) {
	if f.denied[key] {
		serveXMLError(w, http.StatusForbidden, "AccessDenied", "access denied")
		return
	}
	body, ok := f.objects[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("ETag", etagFor(body))
	w.Header().Set("Last-Modified", f.modTimes[key].UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}

// get answers a ranged or whole read of one object.
func (f *fakeS3) get(w http.ResponseWriter, r *http.Request, key string) {
	if f.events != nil {
		f.events.add("get " + key)
	}
	if f.denied[key] {
		serveXMLError(w, http.StatusForbidden, "AccessDenied", "access denied")
		return
	}
	body, ok := f.objects[key]
	if !ok {
		serveXMLError(w, http.StatusNotFound, "NoSuchKey", "the key you specified does not exist")
		return
	}
	spec := r.Header.Get("Range")
	start, end, ok := parseRange(spec, len(body))
	if !ok {
		serveXMLError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "the requested range is not satisfiable")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if spec != "" {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_, _ = w.Write(body[start : end+1])
}

func (f *fakeS3) delete(w http.ResponseWriter, key string) {
	delete(f.objects, key)
	delete(f.modTimes, key)
	if f.events != nil {
		f.events.add("deleted " + key)
	}
	w.WriteHeader(http.StatusNoContent)
}

// list answers a v2 listing of the whole bucket with every object,
// keys in the service's binary order, because the bucket is this
// backend's alone and no page ever truncates.
func (f *fakeS3) list(w http.ResponseWriter, bucket string) {
	keys := make([]string, 0, len(f.objects))
	for key := range f.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` + "\n")
	b.WriteString("  <Name>" + bucket + "</Name>\n")
	b.WriteString("  <IsTruncated>false</IsTruncated>\n")
	for _, key := range keys {
		body := f.objects[key]
		b.WriteString("  <Contents>\n")
		b.WriteString("    <Key>" + key + "</Key>\n")
		b.WriteString("    <LastModified>" + f.modTimes[key].UTC().Format(time.RFC3339) + "</LastModified>\n")
		b.WriteString("    <Size>" + strconv.Itoa(len(body)) + "</Size>\n")
		b.WriteString("    <ETag>" + etagFor(body) + "</ETag>\n")
		b.WriteString("  </Contents>\n")
	}
	b.WriteString("</ListBucketResult>\n")
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(b.String()))
}

// servePresigned verifies a presigned request the way a service does
// before it acts on it, then answers it. A read serves the whole
// object. A write stores the body under the key the URL names, and a
// write that carries a signed checksum is refused unless the body
// hashes to it, which is what a service that honours checksums does.
func (f *fakeS3) servePresigned(w http.ResponseWriter, r *http.Request, key string) {
	if err := f.verifyPresigned(r); err != nil {
		if errors.Is(err, errExpired) {
			serveXMLError(w, http.StatusForbidden, "AccessDenied", "request has expired")
			return
		}
		serveXMLError(w, http.StatusForbidden, "SignatureDoesNotMatch", "the signature does not match")
		return
	}
	switch {
	case r.Method == http.MethodGet:
		body, ok := f.objects[key]
		if !ok {
			serveXMLError(w, http.StatusNotFound, "NoSuchKey", "the key you specified does not exist")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	case r.Method == http.MethodPut && r.URL.Query().Get("partNumber") != "":
		f.servePresignedPart(w, r, key)
	case r.Method == http.MethodPut:
		f.servePresignedPut(w, r, key)
	default:
		serveXMLError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "the method is not allowed")
	}
}

// servePresignedPut stores the body as the object after the checksum
// the signature binds has matched it.
func (f *fakeS3) servePresignedPut(w http.ResponseWriter, r *http.Request, key string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		serveXMLError(w, http.StatusBadRequest, "IncompleteBody", "the body did not arrive whole")
		return
	}
	if signed := r.URL.Query().Get("X-Amz-SignedHeaders"); strings.Contains(signed, "x-amz-checksum-sha256") {
		want, decErr := base64.StdEncoding.DecodeString(r.Header.Get("x-amz-checksum-sha256"))
		got := sha256.Sum256(body)
		if decErr != nil || !bytes.Equal(want, got[:]) {
			serveXMLError(w, http.StatusBadRequest, "BadDigest", "the checksum does not match the body")
			return
		}
	}
	f.objects[key] = body
	f.modTimes[key] = f.now()
	if f.events != nil {
		f.events.add("put " + key)
	}
	w.Header().Set("ETag", etagFor(body))
	w.WriteHeader(http.StatusOK)
}

// servePresignedPart stores the body as one part of the upload the URL
// names, which the signature has already bound.
func (f *fakeS3) servePresignedPart(w http.ResponseWriter, r *http.Request, key string) {
	upload := f.uploads[r.URL.Query().Get("uploadId")]
	if upload == nil || upload.key != key {
		serveXMLError(w, http.StatusNotFound, "NoSuchUpload", "the specified upload does not exist")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		serveXMLError(w, http.StatusBadRequest, "IncompleteBody", "the body did not arrive whole")
		return
	}
	number, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil || number < 1 {
		serveXMLError(w, http.StatusBadRequest, "InvalidArgument", "part number out of range")
		return
	}
	upload.parts[number] = body
	w.Header().Set("ETag", etagFor(body))
	w.WriteHeader(http.StatusOK)
}

// createUpload mints a multipart upload and answers the create shape
// the dialect spells, carrying the id the client will presign parts
// against.
func (f *fakeS3) createUpload(w http.ResponseWriter, key string) {
	f.uploadSeq++
	uploadID := fmt.Sprintf("upload-%d", f.uploadSeq)
	f.uploads[uploadID] = &fakeUpload{key: key, parts: map[int][]byte{}}
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, testBucket, key, uploadID)
}

// completeManifest is the body a complete call carries: the parts to
// assemble, in the order the caller lists them.
type completeManifest struct {
	Parts []struct {
		PartNumber int32  `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	} `xml:"Part"`
}

// completeUpload assembles the listed parts into the object under key.
// Every listed part must exist under the ETag the manifest names, the
// way a service validates the manifest before it assembles, and the
// upload is discarded once the object stands.
func (f *fakeS3) completeUpload(w http.ResponseWriter, r *http.Request, key string) {
	uploadID := r.URL.Query().Get("uploadId")
	upload := f.uploads[uploadID]
	if upload == nil || upload.key != key {
		serveXMLError(w, http.StatusNotFound, "NoSuchUpload", "the specified upload does not exist")
		return
	}
	var manifest completeManifest
	if err := xml.NewDecoder(r.Body).Decode(&manifest); err != nil {
		serveXMLError(w, http.StatusBadRequest, "MalformedXML", "the manifest did not parse")
		return
	}
	var body []byte
	for _, part := range manifest.Parts {
		stored, ok := upload.parts[int(part.PartNumber)]
		if !ok || etagFor(stored) != part.ETag {
			serveXMLError(w, http.StatusBadRequest, "InvalidPart", "a listed part is not held under the etag given")
			return
		}
		body = append(body, stored...)
	}
	f.objects[key] = body
	f.modTimes[key] = f.now()
	delete(f.uploads, uploadID)
	if f.events != nil {
		f.events.add("put " + key)
	}
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>%s</ETag></CompleteMultipartUploadResult>`, testBucket, key, etagFor(body))
}

// abortUpload discards the upload and every part it holds. The upload
// the service does not hold answers NoSuchUpload, which is what makes
// an abort after a completed assembly visible.
func (f *fakeS3) abortUpload(w http.ResponseWriter, r *http.Request, key string) {
	uploadID := r.URL.Query().Get("uploadId")
	if f.uploads[uploadID] == nil || f.uploads[uploadID].key != key {
		serveXMLError(w, http.StatusNotFound, "NoSuchUpload", "the specified upload does not exist")
		return
	}
	delete(f.uploads, uploadID)
	w.WriteHeader(http.StatusNoContent)
}

// listParts answers the part listing a complete call is built from.
func (f *fakeS3) listParts(w http.ResponseWriter, key, uploadID string) {
	upload := f.uploads[uploadID]
	if upload == nil || upload.key != key {
		serveXMLError(w, http.StatusNotFound, "NoSuchUpload", "the specified upload does not exist")
		return
	}
	numbers := make([]int, 0, len(upload.parts))
	for number := range upload.parts {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString("<ListPartsResult>\n")
	b.WriteString("  <Bucket>" + testBucket + "</Bucket>\n")
	b.WriteString("  <Key>" + key + "</Key>\n")
	b.WriteString("  <UploadId>" + uploadID + "</UploadId>\n")
	b.WriteString("  <IsTruncated>false</IsTruncated>\n")
	for _, number := range numbers {
		stored := upload.parts[number]
		fmt.Fprintf(&b, "  <Part><PartNumber>%d</PartNumber><ETag>%s</ETag><Size>%d</Size></Part>\n", number, etagFor(stored), len(stored))
	}
	b.WriteString("</ListPartsResult>\n")
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(b.String()))
}

// verifyPresigned recomputes the signature of a presigned read from
// the canonical request the signing standard spells out, then checks
// the expiry against the double's own clock. A tampered URL, a wrong
// secret or a moved clock all land here.
func (f *fakeS3) verifyPresigned(r *http.Request) error {
	q := r.URL.Query()
	if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
		return fmt.Errorf("algorithm %q", q.Get("X-Amz-Algorithm"))
	}
	scope := strings.Split(q.Get("X-Amz-Credential"), "/")
	if len(scope) != 5 || scope[3] != "s3" || scope[4] != "aws4_request" {
		return fmt.Errorf("credential %q", q.Get("X-Amz-Credential"))
	}
	if scope[2] != f.region {
		return fmt.Errorf("region %q", scope[2])
	}
	expires, err := strconv.ParseInt(q.Get("X-Amz-Expires"), 10, 64)
	if err != nil || expires < 1 {
		return fmt.Errorf("expires %q", q.Get("X-Amz-Expires"))
	}
	signed := q.Get("X-Amz-SignedHeaders")
	if signed == "" {
		return errors.New("no signed headers")
	}

	// The canonical query is every parameter but the signature, in
	// sorted order, with spaces in the %20 form.
	qq := r.URL.Query()
	qq.Del("X-Amz-Signature")
	canonicalQuery := strings.ReplaceAll(qq.Encode(), "+", "%20")

	var canonicalHeaders strings.Builder
	for _, name := range strings.Split(signed, ";") {
		value := r.Header.Get(name)
		if name == "host" {
			value = r.Host
		}
		canonicalHeaders.WriteString(name + ":" + strings.TrimSpace(value) + "\n")
	}
	canonicalRequest := strings.Join([]string{
		r.Method,
		r.URL.EscapedPath(),
		canonicalQuery,
		canonicalHeaders.String(),
		signed,
		"UNSIGNED-PAYLOAD",
	}, "\n")

	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		q.Get("X-Amz-Date"),
		strings.Join(scope[1:], "/"),
		hexSHA256(canonicalRequest),
	}, "\n")

	key := hmacBytes([]byte("AWS4"+f.secret), []byte(scope[1]))
	key = hmacBytes(key, []byte(scope[2]))
	key = hmacBytes(key, []byte(scope[3]))
	key = hmacBytes(key, []byte(scope[4]))
	want := hex.EncodeToString(hmacBytes(key, []byte(stringToSign)))
	if !hmac.Equal([]byte(want), []byte(q.Get("X-Amz-Signature"))) {
		return errors.New("signature does not match")
	}

	stamped, err := time.Parse("20060102T150405Z", q.Get("X-Amz-Date"))
	if err != nil {
		return fmt.Errorf("date %q", q.Get("X-Amz-Date"))
	}
	if f.now().After(stamped.Add(time.Duration(expires) * time.Second)) {
		return errExpired
	}
	return nil
}

// parseRange reads the range forms the tests use and returns the
// inclusive start and end. An absent header is the whole object.
func parseRange(header string, total int) (start, end int, ok bool) {
	if header == "" {
		return 0, total - 1, true
	}
	spec, found := strings.CutPrefix(header, "bytes=")
	if !found {
		return 0, 0, false
	}
	first, last, both := strings.Cut(spec, "-")
	if !both {
		return 0, 0, false
	}
	if first == "" {
		n, err := strconv.Atoi(last)
		if err != nil || n < 1 {
			return 0, 0, false
		}
		return max(total-n, 0), total - 1, true
	}
	start, err := strconv.Atoi(first)
	if err != nil || start < 0 || start >= total {
		return 0, 0, false
	}
	if last == "" {
		return start, total - 1, true
	}
	end, err = strconv.Atoi(last)
	if err != nil || end < start {
		return 0, 0, false
	}
	return start, min(end, total-1), true
}

// serveXMLError answers in the error shape the service dialect spells.
func serveXMLError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, message)
}

// etagFor returns the quoted digest stand-in a stored object carries.
func etagFor(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// hexSHA256 returns the lowercase hex digest of s.
func hexSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// hmacBytes is the one HMAC the signature arithmetic needs.
func hmacBytes(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// recorder is the ordered event list the store's two fakes share, so a
// test can read the order the bytes and the row landed in.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// recordingIndex is a memIndex that reports every create beside a
// shared recorder, which is how a test reads the row half of the
// object-before-row order.
type recordingIndex struct {
	*memIndex
	events *recorder
}

func (m *recordingIndex) Create(ctx context.Context, b mediastore.Blob) error {
	m.events.add("row " + b.ID)
	return m.memIndex.Create(ctx, b)
}

// memIndex is an in-memory mediastore.BlobIndex. It classifies a
// missing row as ErrNotFound and a taken id as ErrAlreadyExists, the
// contract every index implementation follows.
type memIndex struct {
	mu   sync.Mutex
	rows map[string]mediastore.Blob
}

func newMemIndex() *memIndex {
	return &memIndex{rows: map[string]mediastore.Blob{}}
}

func (m *memIndex) Create(_ context.Context, b mediastore.Blob) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[b.ID]; ok {
		return fmt.Errorf("memindex: id taken: %w", mediastore.ErrAlreadyExists)
	}
	m.rows[b.ID] = b
	return nil
}

func (m *memIndex) Get(_ context.Context, blobID string) (mediastore.Blob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.rows[blobID]
	if !ok {
		return mediastore.Blob{}, fmt.Errorf("memindex: no such row: %w", mediastore.ErrNotFound)
	}
	return b, nil
}

func (m *memIndex) Delete(_ context.Context, blobID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[blobID]; !ok {
		return fmt.Errorf("memindex: no such row: %w", mediastore.ErrNotFound)
	}
	delete(m.rows, blobID)
	return nil
}

func (m *memIndex) DeleteGroup(_ context.Context, group string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	found := false
	for blobID, b := range m.rows {
		if b.Group == group {
			delete(m.rows, blobID)
			found = true
		}
	}
	if !found {
		return fmt.Errorf("memindex: no such group: %w", mediastore.ErrNotFound)
	}
	return nil
}

func (m *memIndex) Groups(_ context.Context) ([]mediastore.Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return nil, nil
}

func (m *memIndex) Blobs(_ context.Context) ([]mediastore.Blob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mediastore.Blob, 0, len(m.rows))
	for _, b := range m.rows {
		out = append(out, b)
	}
	return out, nil
}
