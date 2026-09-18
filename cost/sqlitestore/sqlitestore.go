// Package sqlitestore persists the cost ledger and the spend budget over
// SQLite.
//
// The package owns one schema, applied through the sqlite package's migration
// runner under its own namespace, so the ledger records what ran and a second
// open changes nothing. A charge is written through as it arrives, because a
// crash must not lose the record of a call that already spent money. A budget
// reservation carries an expiry, so a process that dies mid call stops holding
// budget when the expiry passes.
//
// A cancelled context stops a call before it commits. The call fails and
// stores nothing, so a caller may retry it, and a read returns no result
// rather than the rows it had already decoded. Open is the exception. It
// writes the schema and the ceiling before it returns. A cancelled open can
// therefore leave the schema, its migration ledger row and the ceiling on
// disk, even though it returns no store. Those bytes are inert, so a caller
// may retry the open and a second open on the same file is unaffected. The
// failure wraps context.Canceled or context.DeadlineExceeded unless the
// database refused the statement for a reason of its own first.
//
// The store enforces the ceiling when it reserves, so concurrent callers never
// hold more than the headroom, and it does not enforce it when it settles,
// because a booking records what a call actually cost. A booking that
// overshoots can therefore push booked spend past the ceiling, exactly as the
// in-memory cost.Budget does.
//
// A KeyedBudget over the same store adds a ceiling per owner. The owner
// ceilings live in their own table beside the one global ceiling, and every
// keyed hold also counts against the global one, so an owner that exhausts
// its share never touches another owner's headroom. The empty owner key
// names the unkeyed budget the store itself keeps, so the keyed budget
// refuses it.
//
// This package is one of the few allowed to import the SQLite driver. The
// ledger arithmetic itself stays in the cost package, and every total is
// summed through a cost.Ledger so the overflow rule never drifts.
package sqlitestore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/id"
	"github.com/nrynss/keel/sqlite"
)

// schemaNamespace is the migration ledger namespace this package owns. It
// shares the database file with every other namespace without colliding,
// because each namespace keeps its own ledger.
const schemaNamespace = "cost"

//go:embed migrations/*.sql
var migrations embed.FS

// defaultReservationTTL is how long a reservation holds budget when Config
// does not set one. Ten minutes outlasts a typical paid call, and frees the
// budget reasonably soon after a crash.
const defaultReservationTTL = 10 * time.Minute

// ErrInvalid is returned for an argument this package cannot honour, such as a
// nil database.
var ErrInvalid = errors.New("sqlitestore: invalid config")

// Config configures Open.
type Config struct {
	// DB is the open database. It must not be nil, and this package never
	// closes it.
	DB *sqlite.DB

	// Namespace overrides the migration ledger namespace. Empty means the
	// one this package owns.
	Namespace string

	// Limit is the budget ceiling. It must not be below zero. The store
	// writes it on every open, so a redeploy can raise or lower the
	// ceiling and the stored spend stays.
	Limit cost.Price

	// ReservationTTL is how long a reservation holds budget before the
	// store treats it as expired. Zero means ten minutes.
	ReservationTTL time.Duration

	// Now supplies the clock that stamps charges and judges expiry. Nil
	// means time.Now.
	Now func() time.Time

	// Logger receives the open record. Nil means slog.Default.
	Logger *slog.Logger
}

// Store persists the cost ledger and the spend budget over one database. Create
// it with Open, because the zero value has no database. Store is safe for
// concurrent use.
type Store struct {
	db             *sqlite.DB
	now            func() time.Time
	log            *slog.Logger
	reservationTTL time.Duration
}

// Reservation is one outstanding budget hold. Reserve returns it and the caller
// passes it back to Settle or Release.
type Reservation struct {
	// ID identifies the reservation.
	ID string

	// Amount is the price Reserve committed.
	Amount cost.Price

	// ExpiresAt is the instant after which the store stops counting the
	// hold.
	ExpiresAt time.Time
}

// Open applies the cost schema to cfg.DB and returns the store. It writes the
// configured ceiling and purges reservations that already expired, so a restart
// frees the budget a dead process was holding. A cancelled context stops the
// open and no store comes back, but the schema, its migration ledger row and
// the ceiling may already have landed. Those bytes are inert, so a caller
// may retry the open and a second open on the same file is unaffected.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("sqlitestore: open: %w: DB must not be nil", ErrInvalid)
	}
	if cfg.Limit < 0 {
		return nil, fmt.Errorf("sqlitestore: open: limit %d: %w", cfg.Limit, cost.ErrNegativeLimit)
	}
	namespace := cfg.Namespace
	if namespace == "" {
		namespace = schemaNamespace
	}
	schema, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: open: %w", err)
	}
	if err := sqlite.Migrate(ctx, cfg.DB, namespace, schema); err != nil {
		return nil, fmt.Errorf("sqlitestore: open: %w", err)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ttl := cfg.ReservationTTL
	if ttl <= 0 {
		ttl = defaultReservationTTL
	}
	store := &Store{db: cfg.DB, now: now, log: logger, reservationTTL: ttl}
	if err := store.setLimit(ctx, cfg.Limit); err != nil {
		return nil, fmt.Errorf("sqlitestore: open: %w", err)
	}
	purged, err := store.purgeExpired(ctx, now())
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: open: %w", err)
	}
	logger.InfoContext(ctx, "sqlitestore: opened", "limit", int64(cfg.Limit), "expired_reservations", purged)
	return store, nil
}

// setLimit writes the configured ceiling. The row is created on the first open
// and updated on every later one, so the stored spend survives a redeploy.
func (s *Store) setLimit(ctx context.Context, limit cost.Price) error {
	const query = `INSERT INTO cost_budget (id, limit_nd, spent_nd) VALUES (1, ?, 0)
		ON CONFLICT(id) DO UPDATE SET limit_nd = excluded.limit_nd`
	if _, err := s.db.Writer().ExecContext(ctx, query, int64(limit)); err != nil {
		return fmt.Errorf("sqlitestore: set limit: %w", err)
	}
	return nil
}

// purgeExpired removes every reservation whose expiry has passed and returns
// how many it removed.
func (s *Store) purgeExpired(ctx context.Context, now time.Time) (int, error) {
	result, err := s.db.Writer().ExecContext(ctx,
		`DELETE FROM cost_reservation WHERE expires_at <= ?`, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: purge reservations: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: purge reservations: %w", err)
	}
	return int(removed), nil
}

// Add records c in the ledger. It writes the row before it returns, so a charge
// for a call that already happened survives a crash. A cancelled context stops
// the write, so no charge is recorded.
func (s *Store) Add(ctx context.Context, c cost.Charge) error {
	const query = `INSERT INTO cost_charge (kind, units, unit_price, ref, created_at)
		VALUES (?, ?, ?, ?, ?)`
	_, err := s.db.Writer().ExecContext(ctx, query,
		c.Kind, int64(c.Units), int64(c.UnitPrice), c.Ref, s.now().UnixMilli())
	if err != nil {
		return fmt.Errorf("sqlitestore: add charge: %w", err)
	}
	return nil
}

// Charges returns every recorded charge in insertion order.
func (s *Store) Charges(ctx context.Context) ([]cost.Charge, error) {
	return s.queryCharges(ctx, `SELECT kind, units, unit_price, ref FROM cost_charge ORDER BY id`)
}

// Total returns the sum of every charge in the ledger. It reports an error
// matching cost.ErrOverflow when the exact sum leaves the int64 range.
func (s *Store) Total(ctx context.Context) (cost.Price, error) {
	return s.total(ctx, `SELECT kind, units, unit_price, ref FROM cost_charge ORDER BY id`)
}

// TotalByKind returns the sum of the charges whose Kind equals kind. It reports
// an error matching cost.ErrOverflow when the exact sum leaves the int64 range.
func (s *Store) TotalByKind(ctx context.Context, kind string) (cost.Price, error) {
	return s.total(ctx,
		`SELECT kind, units, unit_price, ref FROM cost_charge WHERE kind = ? ORDER BY id`, kind)
}

// TotalForRef returns the sum of the charges whose Ref equals ref. It reports
// an error matching cost.ErrOverflow when the exact sum leaves the int64 range.
func (s *Store) TotalForRef(ctx context.Context, ref string) (cost.Price, error) {
	return s.total(ctx,
		`SELECT kind, units, unit_price, ref FROM cost_charge WHERE ref = ? ORDER BY id`, ref)
}

// total loads the matching charges and sums them through a cost.Ledger, so the
// overflow rule and the exact arithmetic stay the ones the cost package
// publishes.
func (s *Store) total(ctx context.Context, query string, args ...any) (cost.Price, error) {
	charges, err := s.queryCharges(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	ledger := cost.NewLedger()
	for _, c := range charges {
		ledger.Add(c)
	}
	return ledger.Total()
}

// queryCharges runs one charge query and decodes every row.
func (s *Store) queryCharges(ctx context.Context, query string, args ...any) ([]cost.Charge, error) {
	rows, err := s.db.Reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: read charges: %w", err)
	}
	defer rows.Close() // the cursor is drained before the caller returns
	var out []cost.Charge
	for rows.Next() {
		var (
			c         cost.Charge
			units     int64
			unitPrice int64
		)
		if err := rows.Scan(&c.Kind, &units, &unitPrice, &c.Ref); err != nil {
			return nil, fmt.Errorf("sqlitestore: scan charge: %w", err)
		}
		c.Units = int(units)
		c.UnitPrice = cost.Price(unitPrice)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlitestore: read charges: %w", err)
	}
	return out, nil
}

// Limit returns the ceiling the store was opened with.
func (s *Store) Limit(ctx context.Context) (cost.Price, error) {
	state, err := loadState(ctx, s.db.Reader(), s.now())
	if err != nil {
		return 0, err
	}
	return state.limit, nil
}

// Spent returns the sum of the prices Settle has booked.
func (s *Store) Spent(ctx context.Context) (cost.Price, error) {
	state, err := loadState(ctx, s.db.Reader(), s.now())
	if err != nil {
		return 0, err
	}
	return state.spent, nil
}

// Reserved returns the sum of the reservations that are still holding budget.
// An expired reservation never counts, so a dead process stops holding budget
// when its hold expires.
func (s *Store) Reserved(ctx context.Context) (cost.Price, error) {
	state, err := loadState(ctx, s.db.Reader(), s.now())
	if err != nil {
		return 0, err
	}
	return state.reserved, nil
}

// Remaining returns the headroom left for new reservations. It is the ceiling
// minus booked spend minus unexpired holds. It reports an error matching
// cost.ErrOverflow when that difference leaves the int64 range.
func (s *Store) Remaining(ctx context.Context) (cost.Price, error) {
	state, err := loadState(ctx, s.db.Reader(), s.now())
	if err != nil {
		return 0, err
	}
	return remainingOf(state)
}

// Reserve commits estimate against the durable budget and returns the hold. It
// fails with an error matching cost.ErrOverBudget when the ceiling, the booked
// spend and the unexpired holds leave less than estimate, and it commits
// nothing then. The check and the insert share one transaction, so concurrent
// callers never overspend. The hold expires after the configured TTL, so a
// caller that dies still returns its budget. A cancelled context stops the
// reserve, so no hold is taken.
func (s *Store) Reserve(ctx context.Context, estimate cost.Price) (Reservation, error) {
	if estimate < 0 {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve %d: %w", estimate, cost.ErrNegativeEstimate)
	}
	now := s.now()
	tx, err := s.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM cost_reservation WHERE expires_at <= ?`, now.UnixMilli()); err != nil {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve: purge expired: %w", err)
	}
	state, err := loadState(ctx, tx, now)
	if err != nil {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve: %w", err)
	}
	remaining, err := remainingOf(state)
	if err != nil {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve: %w", err)
	}
	if estimate > remaining {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve %s over remaining %s: %w",
			estimate, remaining, cost.ErrOverBudget)
	}
	reservationID, err := id.New()
	if err != nil {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve: %w", err)
	}
	expires := now.Add(s.reservationTTL)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO cost_reservation (id, amount_nd, expires_at, created_at) VALUES (?, ?, ?, ?)`,
		reservationID, int64(estimate), expires.UnixMilli(), now.UnixMilli()); err != nil {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Reservation{}, fmt.Errorf("sqlitestore: reserve: %w", err)
	}
	return Reservation{ID: reservationID, Amount: estimate, ExpiresAt: expires}, nil
}

// Settle books the price a paid call actually cost and releases the hold. It
// books actual even when the hold already expired, because the call happened
// and the charge is a fact. It reports an error matching cost.ErrOverflow and
// commits nothing when actual would push the booked spend past the int64 range.
// A cancelled context stops the settle, so nothing is booked.
func (s *Store) Settle(ctx context.Context, r Reservation, actual cost.Price) error {
	tx, err := s.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: settle: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	var spent int64
	if err := tx.QueryRowContext(ctx,
		`SELECT spent_nd FROM cost_budget WHERE id = 1`).Scan(&spent); err != nil {
		return fmt.Errorf("sqlitestore: settle: %w", err)
	}
	next, err := addPrice(cost.Price(spent), actual)
	if err != nil {
		return fmt.Errorf("sqlitestore: settle: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE cost_budget SET spent_nd = ? WHERE id = 1`, int64(next)); err != nil {
		return fmt.Errorf("sqlitestore: settle: %w", err)
	}
	if r.ID != "" {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM cost_reservation WHERE id = ?`, r.ID); err != nil {
			return fmt.Errorf("sqlitestore: settle: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitestore: settle: %w", err)
	}
	return nil
}

// Release frees a hold the caller never spent. A hold that already expired or
// was already settled frees nothing, and that is not an error. A cancelled
// context stops the release, so the hold stays until its expiry frees it.
func (s *Store) Release(ctx context.Context, r Reservation) error {
	if r.ID == "" {
		return nil
	}
	if _, err := s.db.Writer().ExecContext(ctx,
		`DELETE FROM cost_reservation WHERE id = ?`, r.ID); err != nil {
		return fmt.Errorf("sqlitestore: release: %w", err)
	}
	return nil
}

// querier is the statement subset shared by the write pool and a transaction,
// so one budget read serves a plain read and a reserve alike.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// budgetState is the three numbers the budget arithmetic needs, read in one
// place so a read and a reserve never disagree.
type budgetState struct {
	limit    cost.Price
	spent    cost.Price
	reserved cost.Price
}

// loadState reads the ceiling, the booked spend and the unexpired holds. It
// counts only reservations whose expiry is still in the future, so an expired
// hold never keeps budget.
func loadState(ctx context.Context, q querier, now time.Time) (budgetState, error) {
	var state budgetState
	var limit, spent int64
	if err := q.QueryRowContext(ctx,
		`SELECT limit_nd, spent_nd FROM cost_budget WHERE id = 1`).Scan(&limit, &spent); err != nil {
		return budgetState{}, fmt.Errorf("sqlitestore: read budget: %w", err)
	}
	state.limit = cost.Price(limit)
	state.spent = cost.Price(spent)
	var reserved int64
	if err := q.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_nd), 0) FROM cost_reservation WHERE expires_at > ?`,
		now.UnixMilli()).Scan(&reserved); err != nil {
		return budgetState{}, fmt.Errorf("sqlitestore: read reservations: %w", err)
	}
	state.reserved = cost.Price(reserved)
	return state, nil
}

// remainingOf computes the headroom. It subtracts the holds first, because the
// ceiling and the holds are both non-negative, so that difference cannot leave
// the int64 range. Subtracting the booked spend last reports an overflow only
// when the headroom itself is unrepresentable.
func remainingOf(state budgetState) (cost.Price, error) {
	left, err := subPrice(state.limit, state.reserved)
	if err != nil {
		return 0, err
	}
	return subPrice(left, state.spent)
}

// addPrice returns a plus b. It reports cost.ErrOverflow when the exact sum
// leaves the int64 range.
func addPrice(a, b cost.Price) (cost.Price, error) {
	sum := a + b
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return 0, fmt.Errorf("sqlitestore: sum %d + %d: %w", a, b, cost.ErrOverflow)
	}
	return sum, nil
}

// subPrice returns a minus b. It reports cost.ErrOverflow when the exact
// difference leaves the int64 range.
func subPrice(a, b cost.Price) (cost.Price, error) {
	diff := a - b
	if (b > 0 && diff > a) || (b < 0 && diff < a) {
		return 0, fmt.Errorf("sqlitestore: difference %d - %d: %w", a, b, cost.ErrOverflow)
	}
	return diff, nil
}
