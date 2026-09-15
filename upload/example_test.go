package upload_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"github.com/nrynss/keel/mediastore"
	"github.com/nrynss/keel/upload"
)

// memoryStore records persisted blobs in memory, so the example needs no
// database.
type memoryStore struct {
	blobs map[string][]byte
}

// PersistWithID implements upload.BlobStore.
func (s *memoryStore) PersistWithID(_ context.Context, blobID string, src io.Reader, _ mediastore.Put) error {
	data, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	s.blobs[blobID] = data
	return nil
}

// call serves one request against mux and returns its recorder.
func call(mux *http.ServeMux, method, target string, body []byte, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	for name, value := range header {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// ExampleHandler uploads a small file in one chunk and prints what the store
// accepted.
func ExampleHandler() {
	root, err := os.MkdirTemp("", "keel-upload-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(root)

	store := &memoryStore{blobs: make(map[string][]byte)}
	handler, err := upload.New(upload.Config{Dir: filepath.Join(root, "staging"), Store: store})
	if err != nil {
		fmt.Println("new handler failed:", err)
		return
	}

	mux := http.NewServeMux()
	handler.Mount(mux)

	body := []byte("hello")
	open := fmt.Sprintf(`{"owner":"alice","content_type":"image/png","size_bytes":%d,"chunk_size":%d}`, len(body), len(body))
	var opened struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(call(mux, http.MethodPost, "/uploads", []byte(open), nil).Body.Bytes(), &opened); err != nil {
		fmt.Println("open failed:", err)
		return
	}

	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	chunk := call(mux, http.MethodPut, "/uploads/"+opened.ID+"/chunks/0", body, map[string]string{"X-Chunk-SHA256": digest})
	if chunk.Code != http.StatusOK {
		fmt.Println("chunk failed:", chunk.Code, chunk.Body.String())
		return
	}

	finish := fmt.Sprintf(`{"sha256":"%s"}`, digest)
	var done struct {
		SizeBytes int64  `json:"size_bytes"`
		SHA256    string `json:"sha256"`
	}
	if err := json.Unmarshal(call(mux, http.MethodPost, "/uploads/"+opened.ID+"/complete", []byte(finish), nil).Body.Bytes(), &done); err != nil {
		fmt.Println("complete failed:", err)
		return
	}

	fmt.Println(done.SizeBytes)
	fmt.Println(done.SHA256)
	fmt.Println(string(store.blobs[opened.ID]))
	// Output:
	// 5
	// 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
	// hello
}
