package mediastore_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"

	"github.com/nrynss/keel/mediastore"
	mediasql "github.com/nrynss/keel/mediastore/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// ExampleStore_Persist stores a public image and serves it back through the
// route the store expects.
func ExampleStore_Persist() {
	dir, err := os.MkdirTemp("", "keel-media-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "media.db")})
	if err != nil {
		fmt.Println("open failed:", err)
		return
	}
	defer db.Close()

	index, err := mediasql.Open(ctx, mediasql.Config{DB: db})
	if err != nil {
		fmt.Println("open index failed:", err)
		return
	}
	store, err := mediastore.Open(ctx, mediastore.Config{Dir: filepath.Join(dir, "blobs"), Index: index})
	if err != nil {
		fmt.Println("open store failed:", err)
		return
	}

	blobID, err := store.Persist(ctx, strings.NewReader("png-bytes"), mediastore.Put{
		ContentType: "image/png",
		Owner:       "alice",
		Visibility:  mediastore.Public,
	})
	if err != nil {
		fmt.Println("persist failed:", err)
		return
	}

	mux := http.NewServeMux()
	mux.Handle("GET /media/{id}", store)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/media/"+blobID, nil))

	fmt.Println(rec.Code)
	fmt.Println(rec.Header().Get("Content-Type"))
	fmt.Print(rec.Body.String())
	// Output:
	// 200
	// image/png
	// png-bytes
}
