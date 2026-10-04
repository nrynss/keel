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

// ExampleStore_Snapshot captures one owner's blobs and restores them into
// another store under the same ids.
func ExampleStore_Snapshot() {
	dir, err := os.MkdirTemp("", "keel-snapshot-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	openStore := func(name string) (*sqlite.DB, *mediastore.Store, error) {
		db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, name+".db")})
		if err != nil {
			return nil, nil, err
		}
		index, err := mediasql.Open(ctx, mediasql.Config{DB: db})
		if err != nil {
			db.Close()
			return nil, nil, err
		}
		store, err := mediastore.Open(ctx, mediastore.Config{Dir: filepath.Join(dir, name), Index: index})
		if err != nil {
			db.Close()
			return nil, nil, err
		}
		return db, store, nil
	}

	srcDB, src, err := openStore("src")
	if err != nil {
		fmt.Println("open failed:", err)
		return
	}
	defer srcDB.Close()
	blobID, err := src.Persist(ctx, strings.NewReader("png-bytes"), mediastore.Put{
		ContentType: "image/png",
		Owner:       "alice",
		Visibility:  mediastore.Public,
	})
	if err != nil {
		fmt.Println("persist failed:", err)
		return
	}
	snap := filepath.Join(dir, "snap")
	if err := src.Snapshot(ctx, snap, mediastore.Selection{Owner: "alice"}); err != nil {
		fmt.Println("snapshot failed:", err)
		return
	}

	dstDB, dst, err := openStore("dst")
	if err != nil {
		fmt.Println("open failed:", err)
		return
	}
	defer dstDB.Close()
	if err := dst.Restore(ctx, snap); err != nil {
		fmt.Println("restore failed:", err)
		return
	}

	mux := http.NewServeMux()
	mux.Handle("GET /media/{id}", dst)
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
