package erase_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nrynss/keel/erase"
	"github.com/nrynss/keel/job"
	jobsql "github.com/nrynss/keel/job/sqlitestore"
	"github.com/nrynss/keel/sqlite"
	"github.com/nrynss/keel/stream"
)

// exampleTarget is one delete the example performs. The gone one answers
// for a blob that left already.
type exampleTarget struct {
	name string
	gone bool
}

// Name identifies the target.
func (t *exampleTarget) Name() string { return t.name }

// Delete removes the target, or reports it already gone.
func (t *exampleTarget) Delete(ctx context.Context) error {
	if t.gone {
		return fmt.Errorf("remote says absent: %w", erase.ErrGone)
	}
	return nil
}

// ExampleEraser_Start registers two targets, waits for the erasure to
// finish, and reads the ledger back.
func ExampleEraser_Start() {
	dir, err := os.MkdirTemp("", "keel-erase-example-")
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
	er, err := erase.New(func(ctx context.Context, ref string) ([]erase.Target, error) {
		return nil, fmt.Errorf("the example never restarts")
	}, erase.Config{})
	if err != nil {
		fmt.Println("new eraser failed:", err)
		return
	}
	runner, err := job.Open(ctx, job.Config{
		Broker: stream.New(stream.Config{}),
		Store:  store,
		Kinds:  map[string]job.Kind{erase.KindName: er.Kind()},
	})
	if err != nil {
		fmt.Println("open runner failed:", err)
		return
	}

	id, err := er.Start(ctx, runner, "account-1", []erase.Target{
		&exampleTarget{name: "row-1"},
		&exampleTarget{name: "blob-1", gone: true},
	})
	if err != nil {
		fmt.Println("start failed:", err)
		return
	}

	deadline := time.Now().Add(10 * time.Second)
	var res job.Result
	for {
		res, err = runner.Result(ctx, id)
		if err != nil {
			fmt.Println("result failed:", err)
			return
		}
		if res.Status != job.StatusRunning && res.Status != job.StatusQueued {
			break
		}
		if time.Now().After(deadline) {
			fmt.Println("timed out waiting for a terminal state")
			return
		}
		time.Sleep(time.Millisecond)
	}

	rep, err := er.Inspect(ctx, runner, id)
	if err != nil {
		fmt.Println("inspect failed:", err)
		return
	}

	fmt.Println(res.Status)
	fmt.Println(rep.Complete(), rep.Confirmed)
	// Output:
	// done
	// true [blob-1 row-1]
}
