package sqlitestore

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nrynss/keel/sqlite"
)

// TestPagedQueriesUseTheirIndexes reads the query plan of the statements the
// paged listings run, so a schema or query change that falls back to a scan
// or a sort fails here.
func TestPagedQueriesUseTheirIndexes(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(t.TempDir(), "revision.db"), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() }) // the handle is discarded here, so a close failure cannot fail the test
	if _, err := Open(ctx, Config{DB: db, Logger: slog.New(slog.DiscardHandler)}); err != nil {
		t.Fatalf("open store: %v", err)
	}
	cases := []struct {
		name  string
		query string
		args  []any
		index string
	}{
		{"history", historyPageQuery, []any{"d", 9, 5}, "sqlite_autoindex_revision_entry_1"},
		{"documents", documentsPageQuery, []any{"o", "", 5}, "revision_document_owner"},
		{"proposals", proposalsPageQuery, []any{"d", 9, 5}, "revision_proposal_seq"},
	}
	for _, c := range cases {
		rows, err := db.Reader().QueryContext(ctx, "EXPLAIN QUERY PLAN "+c.query, c.args...)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatalf("%s: scan: %v", c.name, err)
			}
			plan = append(plan, detail)
		}
		_ = rows.Close()
		joined := strings.Join(plan, "; ")
		if !strings.Contains(joined, "SEARCH") || !strings.Contains(joined, c.index) || strings.Contains(joined, "TEMP B-TREE") {
			t.Errorf("%s plan = %q, want a search on %s with no sort", c.name, joined, c.index)
		}
	}
}
