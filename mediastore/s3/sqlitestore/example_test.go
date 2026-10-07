package sqlitestore_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nrynss/keel/mediastore/s3"
	"github.com/nrynss/keel/mediastore/s3/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// abandoner is the Aborter an app supplies. In an app it is the object
// storage backend itself, whose AbortUpload discards every part the
// upload holds.
type abandoner struct{}

func (abandoner) AbortUpload(_ context.Context, blobID, _ string) error {
	if blobID == "" {
		return errors.New("abandoner: no blob id")
	}
	return nil
}

// Example walks one session through its life: the app records the plan
// it opened in the bucket, the part routes read it back while the
// client uploads, and a sweep abandons what the client left behind.
func Example() {
	dir, err := os.MkdirTemp("", "keel-s3-sqlitestore-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "sessions.db")})
	if err != nil {
		fmt.Println("open failed:", err)
		return
	}
	defer db.Close()
	store, err := sqlitestore.Open(ctx, sqlitestore.Config{DB: db})
	if err != nil {
		fmt.Println("open store failed:", err)
		return
	}

	// The app opened the multipart upload in the bucket and holds the
	// upload id the service returned. The record lands before the first
	// part URL is issued, so a restart finds the plan. The part size
	// carries the dialect's minimum, which the service checks when the
	// parts assemble.
	createdAt := time.Now().Add(-48 * time.Hour)
	err = store.Create(ctx, sqlitestore.Session{
		BlobID:      "0f4a2c9e6b1d4a7f8c3e2b5d6a9f0e1c",
		UploadID:    "upload-from-the-service",
		ContentType: "video/mp4",
		SizeBytes:   1500,
		SHA256:      "6a4f2c9e6b1d4a7f8c3e2b5d6a9f0e1c6a4f2c9e6b1d4a7f8c3e2b5d6a9f0e1c",
		PartSize:    s3.MinPartSize,
		PartCount:   2,
		CreatedAt:   createdAt,
	})
	fmt.Println("created:", err)

	sess, err := store.Get(ctx, "0f4a2c9e6b1d4a7f8c3e2b5d6a9f0e1c")
	fmt.Println(sess.PartCount, err)

	// The client gave up, and the record carries the day it was
	// written. The sweep aborts the upload in the bucket, which
	// discards its parts, and then drops the row.
	removed, err := store.Sweep(ctx, abandoner{}, 24*time.Hour)
	fmt.Println(removed, err)

	_, err = store.Get(ctx, "0f4a2c9e6b1d4a7f8c3e2b5d6a9f0e1c")
	fmt.Println(errors.Is(err, sqlitestore.ErrNotFound))

	// Output:
	// created: <nil>
	// 2 <nil>
	// 1 <nil>
	// true
}
