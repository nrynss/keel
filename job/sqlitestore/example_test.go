package sqlitestore_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nrynss/keel/job"
	"github.com/nrynss/keel/job/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// ExampleOpen records a job's terminal state and reads it back, which is what
// a process does after a restart.
func ExampleOpen() {
	dir, err := os.MkdirTemp("", "keel-job-store-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "jobs.db")})
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

	const jobID = "0123456789abcdef0123456789abcdef"
	if err := store.Create(ctx, job.Record{ID: jobID, Kind: "default", Status: job.StatusRunning, Attempt: 1, RootID: jobID}); err != nil {
		fmt.Println("create failed:", err)
		return
	}
	if err := store.Finish(ctx, job.Record{ID: jobID, Kind: "default", Status: job.StatusDone, Attempt: 1, RootID: jobID, Data: []byte("mp3-bytes")}); err != nil {
		fmt.Println("finish failed:", err)
		return
	}

	record, err := store.Get(ctx, jobID)
	if err != nil {
		fmt.Println("get failed:", err)
		return
	}
	unfinished, err := store.Unfinished(ctx)
	if err != nil {
		fmt.Println("unfinished failed:", err)
		return
	}

	fmt.Println(record.Status, string(record.Data))
	fmt.Println(len(unfinished))
	// Output:
	// done mp3-bytes
	// 0
}
