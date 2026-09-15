package sqlite_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing/fstest"

	"github.com/nrynss/keel/sqlite"
)

// ExampleOpen opens a database, applies one migration, writes a row through
// the write pool and reads it back through the read pool.
func ExampleOpen() {
	dir, err := os.MkdirTemp("", "keel-sqlite-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "app.db")})
	if err != nil {
		fmt.Println("open failed:", err)
		return
	}
	defer db.Close()

	migrations := fstest.MapFS{
		"0001_notes.sql": &fstest.MapFile{Data: []byte("CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT NOT NULL)")},
	}
	if err := sqlite.Migrate(ctx, db, "app", migrations); err != nil {
		fmt.Println("migrate failed:", err)
		return
	}
	if _, err := db.Writer().ExecContext(ctx, "INSERT INTO notes (body) VALUES (?)", "hello"); err != nil {
		fmt.Println("insert failed:", err)
		return
	}

	var body string
	if err := db.Reader().QueryRowContext(ctx, "SELECT body FROM notes").Scan(&body); err != nil {
		fmt.Println("select failed:", err)
		return
	}

	fmt.Println(body)
	// Output:
	// hello
}
