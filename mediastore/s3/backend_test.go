package s3

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nrynss/keel/mediastore"
)

// base is the instant the clocked tests measure against.
var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// openBackend returns a Backend over the double at endpoint. It
// speaks the double's path style, carries the client that trusts its
// certificate, and sets a presign expiry the tests read back off a
// generated URL.
func openBackend(t *testing.T, endpoint string, client *http.Client) *Backend {
	t.Helper()
	b, err := Open(t.Context(), Config{
		Endpoint:      endpoint,
		Bucket:        testBucket,
		Region:        testRegion,
		AccessKey:     testAccess,
		SecretKey:     testSecret,
		PresignExpiry: 15 * time.Minute,
		PathStyle:     true,
		Client:        client,
	})
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	return b
}

// openStore returns a mediastore Store running on the backend with an
// in-memory index, the way a supplied backend reaches the store.
func openStore(t *testing.T, b *Backend, index mediastore.BlobIndex) *mediastore.Store {
	t.Helper()
	s, err := mediastore.Open(t.Context(), mediastore.Config{
		Index:   index,
		Backend: b,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

// deterministic returns bytes a slice assertion cannot pass by accident.
func deterministic(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func TestRoundTripThroughTheStore(t *testing.T) {
	f, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	s := openStore(t, b, newMemIndex())
	data := deterministic(4096)

	blobID, err := s.Persist(t.Context(), bytes.NewReader(data), mediastore.Put{
		ContentType: "image/png",
		Visibility:  mediastore.Public,
	})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if got := f.bodyOf(blobID); !bytes.Equal(got, data) {
		t.Fatalf("the service holds %d bytes, want the %d persisted", len(got), len(data))
	}

	r, row, err := s.Open(t.Context(), blobID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes, want the %d stored", len(got), len(data))
	}
	if row.ContentType != "image/png" || row.SizeBytes != int64(len(data)) {
		t.Fatalf("row = %+v, want the persisted metadata", row)
	}

	if err := s.Delete(t.Context(), blobID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if f.holds(blobID) {
		t.Fatal("the object survived the delete")
	}
}

func TestWriteRefusesATakenIDAndKeepsTheStoredBytes(t *testing.T) {
	f, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	s := openStore(t, b, newMemIndex())
	reserved := strings.Repeat("b", 32)
	first := deterministic(100)

	if err := s.PersistWithID(t.Context(), reserved, bytes.NewReader(first), mediastore.Put{
		ContentType: "image/png",
	}); err != nil {
		t.Fatalf("first persist: %v", err)
	}
	err := s.PersistWithID(t.Context(), reserved, bytes.NewReader(deterministic(40)), mediastore.Put{
		ContentType: "image/png",
	})
	if !errors.Is(err, mediastore.ErrAlreadyExists) {
		t.Fatalf("retry on a taken id = %v, want ErrAlreadyExists", err)
	}
	if got := f.bodyOf(reserved); !bytes.Equal(got, first) {
		t.Fatal("the stored bytes changed after a refused retry")
	}
}

func TestDeleteOfAnAbsentObjectMatchesNotFound(t *testing.T) {
	_, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	unknown := strings.Repeat("c", 32)
	if err := b.Delete(t.Context(), unknown); !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("delete of an absent object = %v, want ErrNotFound", err)
	}
	if err := b.Delete(t.Context(), "../escape"); !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("delete of a malformed id = %v, want ErrNotFound", err)
	}
	if _, err := b.Open(t.Context(), "../escape"); !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("open of a malformed id = %v, want ErrNotFound", err)
	}
}

func TestObjectLandsBeforeTheRow(t *testing.T) {
	events := &recorder{}
	f, endpoint, client := newFakeS3(t, func() time.Time { return base })
	f.events = events
	b := openBackend(t, endpoint, client)
	index := &recordingIndex{memIndex: newMemIndex(), events: events}
	s := openStore(t, b, index)

	if _, err := s.Persist(t.Context(), bytes.NewReader(deterministic(64)), mediastore.Put{
		ContentType: "image/png",
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	got := events.snapshot()
	if len(got) != 2 {
		t.Fatalf("events = %v, want one put and one row create", got)
	}
	if !strings.HasPrefix(got[0], "put ") || !strings.HasPrefix(got[1], "row ") {
		t.Fatalf("events = %v, want the object before the row", got)
	}
	if got[0][len("put "):] != got[1][len("row "):] {
		t.Fatalf("events = %v, want both halves to name one id", got)
	}
}

func TestSweepAgesOrphansByTheServiceTime(t *testing.T) {
	f, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	s := openStore(t, b, newMemIndex())

	aged := strings.Repeat("d", 32)
	fresh := strings.Repeat("e", 32)
	for _, blobID := range []string{aged, fresh} {
		if _, err := b.Write(t.Context(), blobID, bytes.NewReader(deterministic(32))); err != nil {
			t.Fatalf("seed %s: %v", blobID, err)
		}
	}
	f.backdate(aged, base.Add(-2*time.Hour))

	sweeper, err := s.NewSweeper(mediastore.RetentionConfig{
		Now:           func() time.Time { return base },
		OrphanFileAge: time.Hour,
		MaxBytes:      mediastore.Unbounded,
	})
	if err != nil {
		t.Fatalf("new sweeper: %v", err)
	}
	result, err := sweeper.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.OrphanFilesDeleted != 1 || result.OrphanFileBytes != 32 {
		t.Fatalf("sweep = %+v, want one aged orphan of 32 bytes", result)
	}
	if f.holds(aged) {
		t.Fatal("the aged orphan survived")
	}
	if !f.holds(fresh) {
		t.Fatal("the fresh orphan was removed")
	}
}

func TestListCarriesTheServiceTimeAndSizes(t *testing.T) {
	f, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	first := strings.Repeat("f", 32)
	second := strings.Repeat("0", 32)
	if _, err := b.Write(t.Context(), first, bytes.NewReader(deterministic(10))); err != nil {
		t.Fatalf("seed %s: %v", first, err)
	}
	if _, err := b.Write(t.Context(), second, bytes.NewReader(deterministic(20))); err != nil {
		t.Fatalf("seed %s: %v", second, err)
	}
	stamps := map[string]time.Time{
		first:  base.Add(-time.Hour),
		second: base,
	}
	for key, stamp := range stamps {
		f.backdate(key, stamp)
	}

	objects, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(objects) != 2 {
		t.Fatalf("list = %d objects, want 2", len(objects))
	}
	for _, o := range objects {
		switch o.SizeBytes {
		case 10:
			if !o.ModTime.Equal(stamps[first]) {
				t.Fatalf("object %s carries %s, want the service time %s", o.ID, o.ModTime, stamps[first])
			}
		case 20:
			if !o.ModTime.Equal(stamps[second]) {
				t.Fatalf("object %s carries %s, want the service time %s", o.ID, o.ModTime, stamps[second])
			}
		default:
			t.Fatalf("object %s carries %d bytes, want a seeded size", o.ID, o.SizeBytes)
		}
	}
}

func TestPresignedReadServesVerifiesAndExpires(t *testing.T) {
	f, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	data := deterministic(512)
	blobID := strings.Repeat("1", 32)
	if _, err := b.Write(t.Context(), blobID, bytes.NewReader(data)); err != nil {
		t.Fatalf("write: %v", err)
	}

	raw, err := b.PresignGet(t.Context(), blobID)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if !strings.Contains(raw, "X-Amz-Expires=900") {
		t.Fatalf("presigned URL = %s, want the configured 15 minute expiry", raw)
	}
	if strings.Contains(raw, testSecret) {
		t.Fatal("the presigned URL spells the secret")
	}

	// A generated URL must satisfy the verification a service runs:
	// signature, scope and expiry all checked, then the bytes served.
	resp, err := client.Get(raw)
	if err != nil {
		t.Fatalf("follow presigned URL: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read presigned body: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned read = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(body, data) {
		t.Fatalf("presigned read = %d bytes, want the %d stored", len(body), len(data))
	}

	// A flipped character in the signature is a different URL, and the
	// service must refuse it before it serves a byte.
	tampered := raw[:len(raw)-1] + "0"
	if raw[len(raw)-1] == '0' {
		tampered = raw[:len(raw)-1] + "1"
	}
	tamperResp, err := client.Get(tampered)
	if err != nil {
		t.Fatalf("follow tampered URL: %v", err)
	}
	tamperResp.Body.Close()
	if tamperResp.StatusCode != http.StatusForbidden {
		t.Fatalf("tampered read = %d, want 403", tamperResp.StatusCode)
	}

	// The signature stays valid past the expiry, so only the clock
	// makes the URL close. The clock moves past the URL's own signing
	// stamp, which the SDK took from the real time of day.
	stamped, err := time.Parse("20060102T150405Z", queryValue(raw, "X-Amz-Date"))
	if err != nil {
		t.Fatalf("parse the URL signing date: %v", err)
	}
	f.setNow(func() time.Time { return stamped.Add(16 * time.Minute) })
	expiredResp, err := client.Get(raw)
	if err != nil {
		t.Fatalf("follow expired presigned URL: %v", err)
	}
	expiredResp.Body.Close()
	if expiredResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expired presigned read = %d, want 403", expiredResp.StatusCode)
	}
}

func TestProxyModeServesRangeThroughTheStore(t *testing.T) {
	_, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	s := openStore(t, b, newMemIndex())
	data := deterministic(4096)
	blobID, err := s.Persist(t.Context(), bytes.NewReader(data), mediastore.Put{
		ContentType: "audio/mpeg",
		Visibility:  mediastore.Public,
	})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /media/{id}", s)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	url := srv.URL + "/media/" + blobID

	whole, err := http.Get(url)
	if err != nil {
		t.Fatalf("whole read: %v", err)
	}
	body, readErr := io.ReadAll(whole.Body)
	whole.Body.Close()
	if readErr != nil {
		t.Fatalf("read whole body: %v", readErr)
	}
	if whole.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("whole read = %d with %d bytes, want 200 with %d", whole.StatusCode, len(body), len(data))
	}
	if got := whole.Header.Get("Content-Type"); got != "audio/mpeg" {
		t.Fatalf("content type = %q, want the stored type", got)
	}

	sliced, err := rangeGet(client, url, "bytes=2-5")
	if err != nil {
		t.Fatalf("range read: %v", err)
	}
	rangeBody, readErr := io.ReadAll(sliced.Body)
	sliced.Body.Close()
	if readErr != nil {
		t.Fatalf("read range body: %v", readErr)
	}
	if sliced.StatusCode != http.StatusPartialContent {
		t.Fatalf("range read = %d, want 206", sliced.StatusCode)
	}
	if got := sliced.Header.Get("Content-Range"); got != "bytes 2-5/4096" {
		t.Fatalf("content range = %q, want bytes 2-5/4096", got)
	}
	if !bytes.Equal(rangeBody, data[2:6]) {
		t.Fatalf("range body = %d bytes, want the 4 requested", len(rangeBody))
	}

	suffix, err := rangeGet(client, url, "bytes=-5")
	if err != nil {
		t.Fatalf("suffix read: %v", err)
	}
	suffixBody, readErr := io.ReadAll(suffix.Body)
	suffix.Body.Close()
	if readErr != nil {
		t.Fatalf("read suffix body: %v", readErr)
	}
	if suffix.StatusCode != http.StatusPartialContent || !bytes.Equal(suffixBody, data[len(data)-5:]) {
		t.Fatalf("suffix read = %d with %d bytes, want 206 with the last 5", suffix.StatusCode, len(suffixBody))
	}
}

// queryValue reads one query parameter out of a URL.
func queryValue(raw, name string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Query().Get(name)
}

// rangeGet reads one URL with one Range header through client.
func rangeGet(client *http.Client, url, spec string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", spec)
	return client.Do(req)
}

func TestStoreOpenSeeksBothWays(t *testing.T) {
	_, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	s := openStore(t, b, newMemIndex())
	data := deterministic(1024)
	blobID, err := s.Persist(t.Context(), bytes.NewReader(data), mediastore.Put{
		ContentType: "image/png",
	})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	r, _, err := s.Open(t.Context(), blobID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()

	size, err := r.Seek(0, io.SeekEnd)
	if err != nil || size != int64(len(data)) {
		t.Fatalf("seek end = %d, %v, want %d", size, err, len(data))
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek start: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("whole read = %d bytes, %v, want all %d", len(got), err, len(data))
	}

	if _, err := r.Seek(2, io.SeekStart); err != nil {
		t.Fatalf("seek forward: %v", err)
	}
	tail, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(tail, data[2:]) {
		t.Fatalf("read after seek = %d bytes, %v, want the %d from offset 2", len(tail), err, len(data)-2)
	}

	if _, err := r.Seek(-1, io.SeekEnd); err != nil {
		t.Fatalf("seek backward: %v", err)
	}
	last, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(last, data[len(data)-1:]) {
		t.Fatalf("read after backward seek = %d bytes, %v, want the last one", len(last), err)
	}
}

func TestAnEmptyBlobReadsAsEOFWithoutAFetch(t *testing.T) {
	f, endpoint, client := newFakeS3(t, func() time.Time { return base })
	f.events = &recorder{}
	b := openBackend(t, endpoint, client)
	s := openStore(t, b, newMemIndex())
	blobID, err := s.Persist(t.Context(), bytes.NewReader(nil), mediastore.Put{
		ContentType: "image/png",
	})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	r, _, err := s.Open(t.Context(), blobID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()
	n, err := r.Read(make([]byte, 8))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read = %d, %v, want 0 with io.EOF", n, err)
	}
	if got := f.events.snapshot(); len(got) != 1 {
		t.Fatalf("events = %v, want only the persisting put and no read", got)
	}
}

func TestOpenValidatesConfig(t *testing.T) {
	ctx := t.Context()
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no endpoint", Config{Bucket: testBucket, Region: testRegion, AccessKey: testAccess, SecretKey: testSecret}},
		{"no bucket", Config{Endpoint: "http://localhost", Region: testRegion, AccessKey: testAccess, SecretKey: testSecret}},
		{"no region", Config{Endpoint: "http://localhost", Bucket: testBucket, AccessKey: testAccess, SecretKey: testSecret}},
		{"no access key", Config{Endpoint: "http://localhost", Bucket: testBucket, Region: testRegion, SecretKey: testSecret}},
		{"no secret key", Config{Endpoint: "http://localhost", Bucket: testBucket, Region: testRegion, AccessKey: testAccess}},
		{"negative expiry", Config{Endpoint: "http://localhost", Bucket: testBucket, Region: testRegion, AccessKey: testAccess, SecretKey: testSecret, PresignExpiry: -time.Second}},
		{"expiry past a week", Config{Endpoint: "http://localhost", Bucket: testBucket, Region: testRegion, AccessKey: testAccess, SecretKey: testSecret, PresignExpiry: 8 * 24 * time.Hour}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Open(ctx, c.cfg); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestZeroExpiryMeansTheDefault(t *testing.T) {
	_, endpoint, client := newFakeS3(t, func() time.Time { return base })
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
	raw, err := b.PresignGet(t.Context(), strings.Repeat("2", 32))
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if !strings.Contains(raw, "X-Amz-Expires=900") {
		t.Fatalf("presigned URL = %s, want the default 15 minute expiry", raw)
	}
}

func TestFaultsNeverSpellTheCredential(t *testing.T) {
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

	// Presigning talks to no service, so it works against a dead one.
	if _, err := b.PresignGet(t.Context(), strings.Repeat("5", 32)); err != nil {
		t.Fatalf("presign over a dead endpoint = %v, want a local success", err)
	}

	probes := []struct {
		name  string
		probe func() error
	}{
		{"write", func() error {
			_, err := b.Write(t.Context(), strings.Repeat("3", 32), bytes.NewReader(deterministic(8)))
			return err
		}},
		{"open", func() error {
			_, err := b.Open(t.Context(), strings.Repeat("4", 32))
			return err
		}},
		{"list", func() error {
			_, err := b.List(t.Context())
			return err
		}},
		{"delete", func() error {
			return b.Delete(t.Context(), strings.Repeat("6", 32))
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

func TestPersistOverADeadServiceLeavesNoRow(t *testing.T) {
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
	index := newMemIndex()
	s := openStore(t, b, index)
	if _, err := s.Persist(t.Context(), bytes.NewReader(deterministic(8)), mediastore.Put{
		ContentType: "image/png",
	}); err == nil {
		t.Fatal("persist survived a dead service")
	}
	rows, err := index.Blobs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("%d rows landed for a persist the service refused, want none", len(rows))
	}
}

// TestOpenOfAnAbsentIDMatchesNotFound pins the absent-id read
// classification directly. A well formed id the service does not hold
// is an absence, and it arrives as the sentinel the store classifies
// on, with no reader alongside.
func TestOpenOfAnAbsentIDMatchesNotFound(t *testing.T) {
	_, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	r, err := b.Open(t.Context(), strings.Repeat("7", 32))
	if !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("open of an absent id = %v, want ErrNotFound", err)
	}
	if r != nil {
		r.Close()
		t.Fatal("open returned a reader alongside an error")
	}
}

// TestARefusedReadIsAFaultAndNotAnAbsence pins outage versus absence.
// A permission fault on an object the service holds must stay a fault,
// so no caller can mistake a dead or refused service for a deleted
// blob. The store's own read path must classify it the same way.
func TestARefusedReadIsAFaultAndNotAnAbsence(t *testing.T) {
	f, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	s := openStore(t, b, newMemIndex())
	blobID, err := s.Persist(t.Context(), bytes.NewReader(deterministic(32)), mediastore.Put{
		ContentType: "image/png",
	})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	f.deny(blobID)

	if _, err := b.Open(t.Context(), blobID); err == nil || errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("open = %v, want a fault that matches no sentinel", err)
	}
	if err := b.Delete(t.Context(), blobID); err == nil || errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("delete = %v, want a fault that matches no sentinel", err)
	}
	if _, _, err := s.Open(t.Context(), blobID); err == nil || errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("store open = %v, want a fault that matches no sentinel", err)
	}
}

// TestSeekPastTheEndMatchesTheDiskReader pins the seek semantics the
// one Store API promises over every backend. A seek past the end
// reports the position it was asked for, the next read answers EOF,
// and the object keeps its size.
func TestSeekPastTheEndMatchesTheDiskReader(t *testing.T) {
	_, endpoint, client := newFakeS3(t, func() time.Time { return base })
	b := openBackend(t, endpoint, client)
	s := openStore(t, b, newMemIndex())
	data := deterministic(32)
	blobID, err := s.Persist(t.Context(), bytes.NewReader(data), mediastore.Put{
		ContentType: "image/png",
	})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	r, _, err := s.Open(t.Context(), blobID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()

	pos, err := r.Seek(9999, io.SeekStart)
	if err != nil || pos != 9999 {
		t.Fatalf("seek past the end = %d, %v, want 9999 with no error", pos, err)
	}
	n, err := r.Read(make([]byte, 8))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read past the end = %d, %v, want 0 with io.EOF", n, err)
	}

	if _, err := r.Seek(3, io.SeekStart); err != nil {
		t.Fatalf("seek back: %v", err)
	}
	tail, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(tail, data[3:]) {
		t.Fatalf("read after seeking back = %d bytes, %v, want the %d remaining", len(tail), err, len(data)-3)
	}
}
