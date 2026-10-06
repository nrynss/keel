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

// TestSweepQuotesPlanUsesIndexes pins the query plan of the quote sweep
// and the shape of the index the claim branch seeks. The statement pairs
// an expiry branch with a claim branch, and SQLite only keeps the indexes
// when every branch has one of its own. A missing index drops the whole
// statement to a full scan, and every quote write then pays for the whole
// table on the single writer.
//
// The plan alone cannot see the data the store writes. Every open and
// done row keeps claim_expires_at at 0, which sits inside the claim
// branch's range, so a full index on that column makes the sweep fetch
// and reject every live row. The claim index is partial for exactly that
// reason, and the schema pin below refuses the full form, whose plan is
// identical while its cost grows with the table.
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
	// The partial clause is what keeps the claim index as small as the
	// claims it serves. A full index on the same column plans the same
	// way and still walks every live row, because open and done rows
	// carry a claim deadline of 0, and 0 sits inside the sweep's range.
	var indexSQL string
	if err := tx.QueryRowContext(t.Context(),
		`SELECT sql FROM sqlite_schema WHERE type = 'index' AND name = 'cost_quote_claim'`).Scan(&indexSQL); err != nil {
		t.Fatalf("read the claim index: %v", err)
	}
	if !strings.Contains(indexSQL, "WHERE state = 'claimed'") {
		t.Fatalf("the claim index lost its partial clause: %s", indexSQL)
	}
}
