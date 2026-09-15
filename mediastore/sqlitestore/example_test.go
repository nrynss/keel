package sqlitestore_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nrynss/keel/id"
	"github.com/nrynss/keel/mediastore"
	"github.com/nrynss/keel/mediastore/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// ExampleOpen writes two blob rows in one group and reads a row and the group
// back.
func ExampleOpen() {
	dir, err := os.MkdirTemp("", "keel-media-index-example-")
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

	index, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: db})
	if err != nil {
		fmt.Println("open index failed:", err)
		return
	}

	firstID, err := id.New()
	if err != nil {
		fmt.Println("generate failed:", err)
		return
	}
	secondID, err := id.New()
	if err != nil {
		fmt.Println("generate failed:", err)
		return
	}
	for _, blobID := range []string{firstID, secondID} {
		if err := index.Create(ctx, mediastore.Blob{ID: blobID, ContentType: "image/png", SizeBytes: 3, Group: "g1"}); err != nil {
			fmt.Println("create failed:", err)
			return
		}
	}

	blob, err := index.Get(ctx, firstID)
	if err != nil {
		fmt.Println("get failed:", err)
		return
	}
	groups, err := index.Groups(ctx)
	if err != nil {
		fmt.Println("groups failed:", err)
		return
	}

	fmt.Println(blob.ContentType, blob.SizeBytes)
	fmt.Println(len(groups), groups[0].ID, len(groups[0].Blobs))
	// Output:
	// image/png 3
	// 1 g1 2
}
