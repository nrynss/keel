package providertask_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nrynss/keel/providertask"
	"github.com/nrynss/keel/providertask/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// ExampleRun drives one task from create to result under an idempotency
// key. The provider here is three functions with canned answers, standing
// in for the client the application writes.
func ExampleRun() {
	dir, err := os.MkdirTemp("", "keel-providertask-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "tasks.db")})
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

	// The canned provider: a create that names the task, a status that
	// answers running once and then succeeded, and a result link that
	// would expire.
	polled := false
	spec := providertask.Spec[string]{
		Create: func(ctx context.Context) (string, error) {
			return "task-1", nil
		},
		Status: func(ctx context.Context, taskID string) (providertask.Status, error) {
			if polled {
				return providertask.Status{State: providertask.StateSucceeded}, nil
			}
			polled = true
			return providertask.Status{State: providertask.StateRunning}, nil
		},
		Result: func(ctx context.Context, taskID string) (string, error) {
			return "s3://kept/link", nil
		},
		Keep: func(ctx context.Context, link string) error {
			// Copy the bytes behind the link somewhere they outlive it.
			return nil
		},
	}

	cfg := providertask.Config{
		PollBase:    time.Millisecond,
		PollCeiling: 2 * time.Millisecond,
	}
	outcome, err := providertask.Run(ctx, cfg, store, "order-1", spec)
	fmt.Println(outcome.TaskID, outcome.Value, err)

	// A restart that runs the same key again resumes the recorded task and
	// never creates again.
	polled = false
	spec.Create = func(ctx context.Context) (string, error) {
		return "", errors.New("create must never run twice for one key")
	}
	spec.Result = func(ctx context.Context, taskID string) (string, error) {
		return "s3://kept/link-again", nil
	}
	resumed, err := providertask.Run(ctx, cfg, store, "order-1", spec)
	fmt.Println(resumed.TaskID, resumed.Value, err)
	// Output:
	// task-1 s3://kept/link <nil>
	// task-1 s3://kept/link-again <nil>
}
