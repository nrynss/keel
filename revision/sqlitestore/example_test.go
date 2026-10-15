package sqlitestore_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nrynss/keel/erase"
	"github.com/nrynss/keel/revision"
	"github.com/nrynss/keel/revision/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// ExampleOpen commits a document, refuses a stale edit, and accepts a
// proposal made on the head.
func ExampleOpen() {
	dir, err := os.MkdirTemp("", "keel-revision-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "revision.db")})
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

	first, err := store.Create(ctx, "cue-list", "user-1", []byte(`{"cues":[]}`), "ana")
	if err != nil {
		fmt.Println("create failed:", err)
		return
	}
	if _, err := store.Commit(ctx, "cue-list", first.Number, []byte(`{"cues":[1]}`), "ana"); err != nil {
		fmt.Println("commit failed:", err)
		return
	}
	// A second tab still holds revision 1.
	_, err = store.Commit(ctx, "cue-list", first.Number, []byte(`{"cues":[9]}`), "ana")
	var conflict *revision.ConflictError
	if errors.As(err, &conflict) {
		fmt.Println("rebase onto", conflict.Head)
	}

	p, err := store.Propose(ctx, "cue-list", 2, []byte(`{"cues":[1,2]}`), "model")
	if err != nil {
		fmt.Println("propose failed:", err)
		return
	}
	rev, err := store.Accept(ctx, p.ID, "ana")
	if err != nil {
		fmt.Println("accept failed:", err)
		return
	}
	fmt.Println(rev.Number, string(rev.Body))
	// Output:
	// rebase onto 2
	// 3 {"cues":[1,2]}
}

// ownerTarget is the erase.Target an application registers so an erasure
// fans out to the revision store.
type ownerTarget struct {
	store *sqlitestore.Store
	owner string
}

// Name identifies the target.
func (t ownerTarget) Name() string { return "revisions" }

// Delete forgets the owner's documents. An owner with none is already gone.
func (t ownerTarget) Delete(ctx context.Context) error {
	err := t.store.ForgetOwner(ctx, t.owner)
	if errors.Is(err, revision.ErrUnknownDocument) {
		return fmt.Errorf("%w: %w", erase.ErrGone, err)
	}
	return err
}

// ExampleStore_ForgetOwner shows the erase target a consumer registers. A
// repeated delete answers erase.ErrGone, which an erasure counts as done.
func ExampleStore_ForgetOwner() {
	dir, err := os.MkdirTemp("", "keel-revision-erase-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "revision.db")})
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
	if _, err := store.Create(ctx, "script", "user-1", []byte(`{}`), "ana"); err != nil {
		fmt.Println("create failed:", err)
		return
	}
	var target erase.Target = ownerTarget{store: store, owner: "user-1"}
	fmt.Println(target.Delete(ctx))
	fmt.Println(errors.Is(target.Delete(ctx), erase.ErrGone))
	// Output:
	// <nil>
	// true
}
