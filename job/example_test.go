package job_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nrynss/keel/job"
	jobsql "github.com/nrynss/keel/job/sqlitestore"
	"github.com/nrynss/keel/sqlite"
	"github.com/nrynss/keel/stream"
)

// ExampleRunner_Result starts one job, waits for its terminal state, and
// prints the status and the result bytes it produced.
func ExampleRunner_Result() {
	dir, err := os.MkdirTemp("", "keel-job-example-")
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

	store, err := jobsql.Open(ctx, jobsql.Config{DB: db})
	if err != nil {
		fmt.Println("open store failed:", err)
		return
	}
	runner, err := job.Open(ctx, job.Config{Broker: stream.New(stream.Config{}), Store: store})
	if err != nil {
		fmt.Println("open runner failed:", err)
		return
	}

	jobID, err := runner.Start(ctx, func(ctx context.Context, progress func(job.Progress)) ([]byte, error) {
		progress(job.Progress{Stage: "encode"})
		return []byte("mp3-bytes"), nil
	})
	if err != nil {
		fmt.Println("start failed:", err)
		return
	}

	deadline := time.Now().Add(10 * time.Second)
	var result job.Result
	for {
		result, err = runner.Result(ctx, jobID)
		if err != nil {
			fmt.Println("result failed:", err)
			return
		}
		if result.Status != job.StatusRunning && result.Status != job.StatusQueued {
			break
		}
		if time.Now().After(deadline) {
			fmt.Println("timed out waiting for a terminal state")
			return
		}
		time.Sleep(time.Millisecond)
	}

	fmt.Println(result.Status)
	fmt.Println(string(result.Data))
	// Output:
	// done
	// mp3-bytes
}
