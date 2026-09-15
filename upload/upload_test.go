package upload

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/nrynss/keel/mediastore"
)

// chunks is the 20-byte payload the protocol tests upload, split into three
// chunks of eight, eight and four bytes, so a declared size has a short
// last chunk.
var chunks = [][]byte{[]byte("golden u"), []byte("pload by"), []byte("tes.")}

// whole is chunks joined, the file the tests finish an upload with.
func whole() []byte {
	return bytes.Join(chunks, nil)
}

// TestUploadRefusesAChunkItCannotVerify shows a chunk whose bytes do not
// match the digest declared for it is refused and leaves nothing staged,
// and that the index is still open for the right bytes.
func TestUploadRefusesAChunkItCannotVerify(t *testing.T) {
	x := newHarness(t, Config{})
	up := x.begin("owner-1", "video/mp4", size(int64(len(whole()))), int64(len(chunks[0])))

	corrupt := bytes.Clone(chunks[0])
	corrupt[0] ^= 0xff
	got := x.refuse(x.put(up.ID, 0, corrupt, digest(chunks[0])), http.StatusConflict, codeChunkMismatch)
	var detail mismatchDetail
	x.detail(got, &detail)
	if detail.Index != 0 || detail.Declared != digest(chunks[0]) || detail.Actual != digest(corrupt) {
		t.Fatalf("refusal detail %+v", detail)
	}
	if files := x.uploadFiles(up.ID); len(files) != 0 {
		t.Fatalf("a refused chunk left staging files: %v", files)
	}
	if state := x.state(up.ID); state.StoredBytes != 0 || len(state.Received) != 0 {
		t.Fatalf("a refused chunk changed the state: %+v", state)
	}
	if out := x.putOK(up.ID, 0, chunks[0]); out.StoredBytes != int64(len(chunks[0])) {
		t.Fatalf("stored %d bytes after the correct chunk, want %d", out.StoredBytes, len(chunks[0]))
	}
}

// TestUploadAcceptsARepeatedChunkWithTheSameDigest shows a client that does
// not know whether its chunk arrived can send it again, and that the same
// index sent with a different digest is refused with the stored chunk
// intact.
func TestUploadAcceptsARepeatedChunkWithTheSameDigest(t *testing.T) {
	x := newHarness(t, Config{})
	up := x.begin("owner-1", "video/mp4", size(int64(len(whole()))), int64(len(chunks[0])))

	first := x.putOK(up.ID, 1, chunks[1])
	again := x.putOK(up.ID, 1, chunks[1])
	if again.StoredBytes != first.StoredBytes || len(again.Received) != 1 {
		t.Fatalf("a repeated chunk changed the upload: %+v, want %+v", again, first)
	}
	other := bytes.Clone(chunks[1])
	other[0] = 'X'
	got := x.refuse(x.put(up.ID, 1, other, digest(other)), http.StatusConflict, codeChunkMismatch)
	var detail mismatchDetail
	x.detail(got, &detail)
	if detail.Declared != digest(other) || detail.Actual != digest(chunks[1]) {
		t.Fatalf("refusal detail %+v, want the stored digest reported", detail)
	}
	state := x.state(up.ID)
	if state.StoredBytes != first.StoredBytes || len(state.Received) != 1 {
		t.Fatalf("a refused repeat changed the upload: %+v", state)
	}
}

// TestUploadChecksAResentChunkAgainstItsDigest shows a resend of an index
// the upload already holds is accepted only when its body hashes to the
// digest the client declares. The digest is a promise about the bytes, not
// a ticket that skips the check.
func TestUploadChecksAResentChunkAgainstItsDigest(t *testing.T) {
	x := newHarness(t, Config{})
	up := x.begin("owner-1", "video/mp4", size(int64(len(whole()))), int64(len(chunks[0])))
	x.putOK(up.ID, 0, chunks[0])

	corrupt := bytes.Clone(chunks[0])
	corrupt[0] ^= 0xff
	got := x.refuse(x.put(up.ID, 0, corrupt, digest(chunks[0])), http.StatusConflict, codeChunkMismatch)
	var detail mismatchDetail
	x.detail(got, &detail)
	if detail.Declared != digest(chunks[0]) || detail.Actual != digest(corrupt) {
		t.Fatalf("refusal detail %+v, want the declared and the received digest", detail)
	}
	state := x.state(up.ID)
	if state.StoredBytes != int64(len(chunks[0])) || len(state.Received) != 1 {
		t.Fatalf("a refused resend changed the upload: %+v", state)
	}
	if files := x.uploadFiles(up.ID); len(files) != 1 {
		t.Fatalf("a refused resend left %v on disk", files)
	}
}

// TestUploadAcceptsAResentChunkWhenTheUploadIsFull shows the byte limits do
// not refuse a client that resends a chunk it already sent, because the
// bytes it sends are already counted against the limit.
func TestUploadAcceptsAResentChunkWhenTheUploadIsFull(t *testing.T) {
	x := newHarness(t, Config{MaxUploadBytes: 8, MaxOwnerBytes: 1 << 20, MaxChunks: 64})
	up := x.begin("owner-1", "video/mp4", nil, 8)
	x.putOK(up.ID, 0, []byte("12345678"))
	if out := x.putOK(up.ID, 0, []byte("12345678")); out.StoredBytes != 8 {
		t.Fatalf("a resend at the upload limit stored %d bytes, want 8", out.StoredBytes)
	}
}

// TestUploadRequiresEveryChunkBeforeItCompletes shows the completion names
// the chunks it still needs, and that filling them lets the file land.
func TestUploadRequiresEveryChunkBeforeItCompletes(t *testing.T) {
	x := newHarness(t, Config{})
	up := x.begin("owner-1", "video/mp4", size(int64(len(whole()))), int64(len(chunks[0])))

	x.putOK(up.ID, 0, chunks[0])
	x.putOK(up.ID, 2, chunks[2])
	got := x.refuse(x.complete(up.ID, digest(whole())), http.StatusConflict, codeIncomplete)
	var detail missingDetail
	x.detail(got, &detail)
	if len(detail.Missing) != 1 || detail.Missing[0] != 1 {
		t.Fatalf("missing %v, want [1]", detail.Missing)
	}
	if stored := x.store.blob(up.ID); stored != nil {
		t.Fatalf("an incomplete upload stored %d bytes", len(stored))
	}

	x.putOK(up.ID, 1, chunks[1])
	out := x.completeOK(up.ID, digest(whole()))
	if out.SizeBytes != int64(len(whole())) || out.SHA256 != digest(whole()) {
		t.Fatalf("completion %+v", out)
	}
	if !bytes.Equal(x.store.blob(up.ID), whole()) {
		t.Fatalf("stored bytes do not match the uploaded file")
	}
	if files := x.uploadFiles(up.ID); len(files) != 0 {
		t.Fatalf("a finished upload left staging files: %v", files)
	}
}

// TestUploadRefusesAFileThatDoesNotMatchItsDigest shows the whole-file
// digest is checked before anything is stored, and that the upload survives
// the refusal so a client can retry the completion.
func TestUploadRefusesAFileThatDoesNotMatchItsDigest(t *testing.T) {
	x := newHarness(t, Config{})
	up := x.begin("owner-1", "video/mp4", size(int64(len(whole()))), int64(len(chunks[0])))
	for index, body := range chunks {
		x.putOK(up.ID, index, body)
	}

	wrong := digest([]byte("something else"))
	got := x.refuse(x.complete(up.ID, wrong), http.StatusConflict, codeHashMismatch)
	var detail digestDetail
	x.detail(got, &detail)
	if detail.Declared != wrong || detail.Actual != digest(whole()) {
		t.Fatalf("refusal detail %+v", detail)
	}
	if stored := x.store.blob(up.ID); stored != nil {
		t.Fatalf("a refused upload stored %d bytes", len(stored))
	}
	if state := x.state(up.ID); len(state.Missing) != 0 {
		t.Fatalf("a refused completion lost the chunks: %+v", state)
	}
	x.completeOK(up.ID, digest(whole()))
}

// TestUploadRefusesAChunkOfTheWrongLength shows an upload that declared a
// size fixes what every chunk but the last may weigh.
func TestUploadRefusesAChunkOfTheWrongLength(t *testing.T) {
	x := newHarness(t, Config{})
	up := x.begin("owner-1", "video/mp4", size(int64(len(whole()))), int64(len(chunks[0])))

	short := chunks[0][:4]
	x.refuse(x.put(up.ID, 0, short, digest(short)), http.StatusBadRequest, codeInvalidRequest)
	long := append(bytes.Clone(chunks[0]), 'x')
	x.refuse(x.put(up.ID, 0, long, digest(long)), http.StatusBadRequest, codeInvalidRequest)
	if files := x.uploadFiles(up.ID); len(files) != 0 {
		t.Fatalf("a refused length left staging files: %v", files)
	}
}

// TestUploadRefusesAChunkItCannotPlace shows the indices an upload accepts
// are bounded by the size it declared.
func TestUploadRefusesAChunkItCannotPlace(t *testing.T) {
	x := newHarness(t, Config{})
	up := x.begin("owner-1", "video/mp4", size(int64(len(whole()))), int64(len(chunks[0])))

	x.refuse(x.put(up.ID, 3, chunks[0], digest(chunks[0])), http.StatusBadRequest, codeInvalidRequest)
	x.refuse(x.put(up.ID, -1, chunks[0], digest(chunks[0])), http.StatusBadRequest, codeInvalidRequest)
	x.refuse(x.put(up.ID, 0, chunks[0], "0123"), http.StatusBadRequest, codeInvalidRequest)
	x.refuse(x.put(up.ID, 0, nil, digest(nil)), http.StatusBadRequest, codeInvalidRequest)
}

// TestUploadStoresAnEmptyFile shows a declared zero is a complete upload,
// which a client finishes with the digest of no bytes.
func TestUploadStoresAnEmptyFile(t *testing.T) {
	x := newHarness(t, Config{})
	up := x.begin("owner-1", "video/mp4", size(0), int64(len(chunks[0])))
	if up.ChunkCount == nil || *up.ChunkCount != 0 {
		t.Fatalf("chunk count %v, want 0", up.ChunkCount)
	}
	if out := x.completeOK(up.ID, digest(nil)); out.SizeBytes != 0 {
		t.Fatalf("size %d, want 0", out.SizeBytes)
	}
	if stored := x.store.blob(up.ID); len(stored) != 0 {
		t.Fatalf("stored %d bytes, want none", len(stored))
	}
}

// TestUploadKeepsTheDeclaredMetadata shows what a client declares reaches
// the store unchanged, so the bytes are owned by who the client said and
// carry the visibility it asked for.
func TestUploadKeepsTheDeclaredMetadata(t *testing.T) {
	x := newHarness(t, Config{})
	rec := x.postJSON(x.h.base, startRequest{
		Owner:       "owner-1",
		ContentType: "video/mp4",
		Group:       "clip-7",
		Visibility:  "public",
		SizeBytes:   size(int64(len(whole()))),
		ChunkSize:   int64(len(chunks[0])),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("begin: status %d: %s", rec.Code, rec.Body)
	}
	var up stateResponse
	x.decode(rec, &up)
	if up.Visibility != "public" {
		t.Fatalf("visibility %q, want %q", up.Visibility, "public")
	}
	for index, body := range chunks {
		x.putOK(up.ID, index, body)
	}
	x.completeOK(up.ID, digest(whole()))

	meta := x.store.metadata(up.ID)
	if meta.Owner != "owner-1" || meta.Group != "clip-7" || meta.ContentType != "video/mp4" {
		t.Fatalf("stored metadata %+v", meta)
	}
	if meta.Visibility != mediastore.Public {
		t.Fatalf("stored visibility %q, want %q", meta.Visibility, mediastore.Public)
	}
}

// TestUploadRefusesAVisibilityItDoesNotKnow shows a client cannot ask for a
// visibility the store has no policy for.
func TestUploadRefusesAVisibilityItDoesNotKnow(t *testing.T) {
	x := newHarness(t, Config{})
	rec := x.postJSON(x.h.base, startRequest{Owner: "owner-1", ContentType: "video/mp4", Visibility: "friends"})
	x.refuse(rec, http.StatusBadRequest, codeInvalidRequest)
}

// TestUploadMapsAStoreRefusal shows the failures the store reports reach a
// client as the codes the envelope names, and that a failure the client did
// not cause leaves the chunks staged for a retry.
func TestUploadMapsAStoreRefusal(t *testing.T) {
	cases := []struct {
		name   string
		fail   error
		status int
		code   string
	}{
		{"unaccepted type", mediastore.ErrInvalidContentType, http.StatusUnsupportedMediaType, codeUnsupportedType},
		{"unknown blob", mediastore.ErrNotFound, http.StatusBadRequest, codeInvalidRequest},
		{"taken id", mediastore.ErrAlreadyExists, http.StatusConflict, codeConflict},
		{"unknown fault", errors.New("disk is full"), http.StatusInternalServerError, codeStorageError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := newHarness(t, Config{})
			up := x.begin("owner-1", "video/mp4", size(int64(len(whole()))), int64(len(chunks[0])))
			for index, body := range chunks {
				x.putOK(up.ID, index, body)
			}
			x.store.failWith(tc.fail)
			x.refuse(x.complete(up.ID, digest(whole())), tc.status, tc.code)
			x.store.failWith(nil)

			out := x.completeOK(up.ID, digest(whole()))
			if !bytes.Equal(x.store.blob(out.ID), whole()) {
				t.Fatalf("a retried completion stored the wrong bytes")
			}
		})
	}
}

// TestUploadRefusesAnIdThatAlreadyNamesBytes shows an upload whose id is
// taken is refused without damaging the blob already stored under it.
func TestUploadRefusesAnIdThatAlreadyNamesBytes(t *testing.T) {
	x := newHarness(t, Config{})
	// The harness mints ids counting up from one, so the first upload
	// takes this id.
	taken := "00000000000000000000000000000001"
	x.store.seed(taken, []byte("already stored"))

	up := x.begin("owner-1", "video/mp4", size(int64(len(whole()))), int64(len(chunks[0])))
	if up.ID != taken {
		t.Fatalf("upload id %q, want %q", up.ID, taken)
	}
	for index, body := range chunks {
		x.putOK(up.ID, index, body)
	}
	x.refuse(x.complete(up.ID, digest(whole())), http.StatusConflict, codeConflict)
	if got := string(x.store.blob(taken)); got != "already stored" {
		t.Fatalf("stored blob is %q, want it untouched", got)
	}
}

// TestUploadAnswersAnUnknownUploadWithNotFound shows every route an upload
// has answers the same way for an id this handler does not hold.
func TestUploadAnswersAnUnknownUploadWithNotFound(t *testing.T) {
	x := newHarness(t, Config{})
	unknown := "ffffffffffffffffffffffffffffffff"
	x.refuse(x.call(http.MethodGet, x.uploadPath(unknown), nil, nil), http.StatusNotFound, codeNotFound)
	x.refuse(x.put(unknown, 0, chunks[0], digest(chunks[0])), http.StatusNotFound, codeNotFound)
	x.refuse(x.complete(unknown, digest(whole())), http.StatusNotFound, codeNotFound)
	x.refuse(x.call(http.MethodGet, x.h.base+"/not-an-id", nil, nil), http.StatusNotFound, codeNotFound)
}

// TestUploadRefusesTheWrongMethod shows each route names the method it
// serves.
func TestUploadRefusesTheWrongMethod(t *testing.T) {
	x := newHarness(t, Config{})
	up := x.begin("owner-1", "video/mp4", size(int64(len(whole()))), int64(len(chunks[0])))

	rec := x.call(http.MethodPost, x.uploadPath(up.ID), nil, nil)
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow %q, want %q", allow, http.MethodGet)
	}
	x.refuse(rec, http.StatusMethodNotAllowed, codeInvalidRequest)
	x.refuse(x.call(http.MethodGet, x.h.base, nil, nil), http.StatusMethodNotAllowed, codeInvalidRequest)
}

// TestUploadRefusesABodyItCannotRead shows a request body that is not the
// JSON object the route expects is refused, not guessed at.
func TestUploadRefusesABodyItCannotRead(t *testing.T) {
	x := newHarness(t, Config{})
	up := x.begin("owner-1", "video/mp4", nil, defaultChunkSize)
	x.refuse(x.call(http.MethodPost, x.uploadPath(up.ID)+"/complete", []byte("{}"), nil), http.StatusBadRequest, codeInvalidRequest)
}

// TestUploadServesOnlyItsBasePath shows a route that merely shares a prefix
// with the handler is not the handler's.
func TestUploadServesOnlyItsBasePath(t *testing.T) {
	x := newHarness(t, Config{BasePath: "/media/uploads"})
	up := x.begin("owner-1", "video/mp4", size(int64(len(whole()))), int64(len(chunks[0])))

	x.refuse(x.call(http.MethodGet, "/media/uploadsX/"+up.ID, nil, nil), http.StatusNotFound, codeNotFound)
	x.refuse(x.call(http.MethodGet, "/uploads/"+up.ID, nil, nil), http.StatusNotFound, codeNotFound)
	if state := x.state(up.ID); state.ID != up.ID {
		t.Fatalf("state %+v", state)
	}
}

// TestUploadRefusesARequestOverItsChunkLimit shows an upload that would need
// more chunk files than the handler accounts for is refused at the start
// rather than after it has staged them.
func TestUploadRefusesARequestOverItsChunkLimit(t *testing.T) {
	x := newHarness(t, Config{MaxChunks: 2, MaxUploadBytes: 1 << 20})
	rec := x.postJSON(x.h.base, startRequest{Owner: "owner-1", ContentType: "video/mp4", SizeBytes: size(4096), ChunkSize: 1024})
	x.refuse(rec, http.StatusBadRequest, codeInvalidRequest)
	if names := x.stagingNames(); len(names) != 0 {
		t.Fatalf("a refused start left %v behind", names)
	}
}

// TestNewRefusesAConfigItCannotServe shows a configuration this package
// cannot serve from is reported rather than defaulted.
func TestNewRefusesAConfigItCannotServe(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{name: "no directory", cfg: Config{Store: newFakeStore()}},
		{name: "no store", cfg: Config{Dir: t.TempDir()}},
		{name: "negative upload limit", cfg: Config{Dir: t.TempDir(), Store: newFakeStore(), MaxUploadBytes: -1}},
		{name: "negative owner limit", cfg: Config{Dir: t.TempDir(), Store: newFakeStore(), MaxOwnerBytes: -1}},
		{name: "negative chunk limit", cfg: Config{Dir: t.TempDir(), Store: newFakeStore(), MaxChunks: -1}},
		{name: "negative ttl", cfg: Config{Dir: t.TempDir(), Store: newFakeStore(), UploadTTL: -1}},
		{name: "negative sweep interval", cfg: Config{Dir: t.TempDir(), Store: newFakeStore(), SweepInterval: -1}},
		{name: "relative base path", cfg: Config{Dir: t.TempDir(), Store: newFakeStore(), BasePath: "uploads"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); !errors.Is(err, errConfig) {
				t.Fatalf("New: err %v, want %v", err, errConfig)
			}
		})
	}
}

// TestUploadNamesEveryRefusalInTheEnvelope shows a refusal carries the code
// a client branches on and a sentence it may show, and never a bare status.
func TestUploadNamesEveryRefusalInTheEnvelope(t *testing.T) {
	x := newHarness(t, Config{})
	rec := x.call(http.MethodGet, x.uploadPath("ffffffffffffffffffffffffffffffff"), nil, nil)
	got := x.refuse(rec, http.StatusNotFound, codeNotFound)
	if got.Error.Message == "" {
		t.Fatal("refusal carries no message")
	}
	if !strings.HasSuffix(rec.Body.String(), "\n") {
		t.Fatalf("refusal body %q does not end in a newline", rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q, want application/json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q, want no-store", cc)
	}
}

// TestUploadReportsEveryGapInAnUndeclaredUpload shows an upload with no
// declared size spans up to its highest stored chunk, names the gaps below
// that, and reports no gap once the run is contiguous.
func TestUploadReportsEveryGapInAnUndeclaredUpload(t *testing.T) {
	x := newHarness(t, Config{})
	up := x.begin("owner-1", "video/mp4", nil, defaultChunkSize)
	x.putOK(up.ID, 0, chunks[0])
	x.putOK(up.ID, 3, chunks[2])

	state := x.state(up.ID)
	if len(state.Missing) != 2 || state.Missing[0] != 1 || state.Missing[1] != 2 {
		t.Fatalf("missing %v after chunks 0 and 3, want [1 2]", state.Missing)
	}
	if len(state.Received) != 2 || state.Received[0] != 0 || state.Received[1] != 3 {
		t.Fatalf("received %v after chunks 0 and 3, want [0 3]", state.Received)
	}

	x.putOK(up.ID, 1, chunks[1])
	x.putOK(up.ID, 2, chunks[2])
	if state := x.state(up.ID); len(state.Missing) != 0 {
		t.Fatalf("missing %v after a contiguous run, want none", state.Missing)
	}
}

// TestUploadStateRenderCostStaysProportionalToStoredChunks pins that
// rendering an upload's state costs time in proportion to the chunks it
// reports rather than to their square. A render that rescanned the whole
// index for every stored index would make a fourfold chunk count about
// sixteen times the work, so the fourfold count must stay under eightfold.
func TestUploadStateRenderCostStaysProportionalToStoredChunks(t *testing.T) {
	render := func(n int) float64 {
		up := &upload{
			id:          "id",
			owner:       "owner",
			contentType: "video/mp4",
			chunkSize:   1,
			highest:     int64(n) - 1,
			chunks:      make(map[int64]chunk, n),
		}
		for i := 0; i < n; i++ {
			up.chunks[int64(i)] = chunk{sizeBytes: 1}
		}
		result := testing.Benchmark(func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_ = up.snapshot()
			}
		})
		return float64(result.NsPerOp())
	}
	const small, large = 1000, 4000
	smallCost, largeCost := render(small), render(large)
	if largeCost > 8*smallCost {
		t.Fatalf("rendering %d stored chunks cost %.0f ns/op and %d cost %.0f ns/op, want the fourfold count under eightfold the cost", small, smallCost, large, largeCost)
	}
}
