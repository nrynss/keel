package sqlitestore

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/sqlite"
)

// TestSweepQuotesPlanUsesIndexes pins the query plan of the quote sweep.
// The statement pairs an expiry branch with a claim branch, and SQLite
// only keeps the indexes when every branch has one of its own. A missing
// index drops the whole statement to a full scan, and every quote write
// then pays for the whole table on the single writer.
func TestSweepQuotesPlanUsesIndexes(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path:   filepath.Join(t.TempDir(), "cost.db"),
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() }) // the handle is discarded here, so a close failure cannot fail the test
	if _, err := Open(t.Context(), Config{DB: db, Limit: cost.Dollar, Logger: logger}); err != nil {
		t.Fatalf("open store: %v", err)
	}
	tx, err := db.Writer().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }() // the plan read needs no commit
	now := time.Now().UnixMilli()
	rows, err := tx.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+sweepQuotesSQL,
		quoteClaimed, now, quoteClaimed, now)
	if err != nil {
		t.Fatalf("explain the sweep: %v", err)
	}
	defer func() {
		_ = rows.Close() // the cursor is drained before the caller returns
		_ = rows.Err()
	}()
	var detail strings.Builder
	for rows.Next() {
		var id, parent, notused, line string
		if err := rows.Scan(&id, &parent, &notused, &line); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		detail.WriteString(line)
		detail.WriteByte('\n')
	}
	plan := detail.String()
	for _, want := range []string{
		"MULTI-INDEX OR",
		"USING INDEX cost_quote_expires",
		"USING INDEX cost_quote_claim",
	} {
		if !strings.Contains(plan, want) {
			t.Fatalf("sweep plan is missing %q:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "SCAN cost_quote") {
		t.Fatalf("sweep plan scans the quote table:\n%s", plan)
	}
}
