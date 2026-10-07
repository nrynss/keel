// Package sqlitestore persists the cost ledger and the spend budget over
// SQLite.
//
// The package owns one schema, applied through the sqlite package's migration
// runner under its own namespace, so the ledger records what ran and a second
// open changes nothing. A charge is written through as it arrives, because a
// crash must not lose the record of a call that already spent money. The
// shared handle commits with synchronous FULL, so a charge the store
// acknowledged also survives an operating system or power failure. The
// sqlite package documents the one exception, the creation window of the
// database file itself. A budget
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
// hold more than the headroom. It does not enforce it when it settles,
// because a booking records what a call actually cost. A booking that
// overshoots can therefore push booked spend past the ceiling, exactly as the
// in-memory cost.Budget does.
//
// A KeyedBudget over the same store adds a ceiling per owner. The owner
// ceilings live in their own table beside the one global ceiling, and every
// keyed hold also counts against the global one. An owner that exhausts its
// share therefore never touches another owner's headroom. The empty owner key
// names the unkeyed budget the store itself keeps, so the keyed budget
// refuses it.
//
// A store opened with a period restarts its ceiling on a calendar window,
// so a limit bounds one day or one month rather than all spend ever booked.
// The spend a window reads comes from the settle history, which every
// Settle extends, so a rollover needs no scheduler and no caller code.
// Totals and charges keep all history either way. Spend booked before this
// version stays in the lifetime total and outside every window.
//
// A store speaks one Denomination for its whole life: its ceiling, its
// owners, its grants and its ledger. The zero denomination is USD
// nanodollars, the behaviour every store had before denominations existed.
// A charge that names another denomination is refused with
// cost.ErrDenominationMismatch, and no path converts one unit into another,
// so a credit pool never reports itself in dollars. Open refuses a file
// whose stored denomination differs from the configured one, because a
// reopen under another name would silently reprice every recorded amount.
//
// Grant funds the pool with credit that lapses. The balance a reservation
// draws on is the sum of the unexpired grants minus what spend has drawn
// from them. Spend draws from the grant that expires soonest first, so a
// short grant is consumed before a longer one. Expiry is judged by the
// store against its own clock at read and spend time, so a restart cannot
// resurrect lapsed credit. Once a pool holds a grant, that balance bounds
// reservations beside the ceiling, and the smaller bound decides. A pool
// with no grant draws on its ceiling alone, exactly as before.
//
// Quote states a price for one action, and Run carries it out once on
// confirmation. Quote holds no budget. Run claims the quote and takes the
// owner's reservation in one transaction, so a crash or a retry between
// claim and work can never charge twice. The claim lives exactly as long
// as a reservation hold, so a runner that vanishes stops holding the
// quote when the hold lapses. A finished run records its outcome on the
// quote row, so every later Run of the same id returns that outcome and
// charges nothing. Run re-checks the price against the configured Reprice
// callback before it claims, and refuses with a fresh quote when the
// price moved past the tolerance. An expired quote refuses the same way.
// Expired quotes are swept beside the expired reservations, at open and
// inside every write a quote drives.
//
// This package is one of the few allowed to import the SQLite driver. The
// ledger arithmetic itself stays in the cost package, and every total is
// summed through a cost.Ledger so the overflow rule never drifts. The Store
// satisfies cost.ChargeSink, so a cost.Meter can record its settled charges
// straight into the durable ledger.
package sqlitestore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sync"
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

// ErrReservationLive is returned by KeyedBudget.ForgetOwner when the owner
// still holds an unexpired reservation. That is money in flight, so the
// erase waits until the hold is settled, released or expired.
var ErrReservationLive = errors.New("sqlitestore: owner still holds an unexpired reservation")

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

	// Period restarts the ceiling on a calendar window. None keeps one
	// lifetime ceiling, which is the zero value and the behaviour every
	// store has today. DailyUTC bounds each UTC day and MonthlyUTC each
	// UTC month. The same window applies to the global ceiling and to
	// every owner ceiling. Reserve, Remaining and Spent read the spend
	// booked since the current window began, while Total and Charges
	// keep all history. The period is configuration, so reopening with
	// another period re-windows the same history.
	Period cost.Period

	// Denomination names the unit every price in this store counts. The
	// zero value keeps the USD nanodollar behaviour every store has. A
	// store speaks one denomination for its whole life. Open refuses a
	// file whose stored denomination differs from the configured one,
	// because a reopen under another name would silently reprice every
	// recorded amount.
	Denomination cost.Denomination

	// ReservationTTL is how long a reservation holds budget before the
	// store treats it as expired. Zero means ten minutes. A quote claim
	// lives exactly as long as one of these holds, so a runner that
	// vanishes stops holding its quote when the TTL passes.
	ReservationTTL time.Duration

	// Reprice reports the current price of one quoted action for owner.
	// Run calls it before it claims an open quote, live or past its
	// confirm window, so a price that moved re-confirms before any
	// spend. The price it reports counts minor
	// units of the store's denomination, and a negative one is refused.
	// Nil means the quoted price never moves and no check runs.
	Reprice func(ctx context.Context, owner string) (cost.Price, error)

	// QuoteTolerance is how far the price Reprice reports may move from
	// the quoted price before Run refuses with a fresh quote. It counts
	// minor units of the store's denomination, in both directions. Zero
	// requires an exact match when Reprice is set. A negative tolerance
	// is refused at open.
	QuoteTolerance cost.Price

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
	period         cost.Period
	denom          cost.Denomination
	quoteTolerance cost.Price
	reprice        func(ctx context.Context, owner string) (cost.Price, error)

	// flightMu guards flights, the in-process single-flight table for
	// quote runs. One quote runs one work body at a time in this
	// process, and a second caller waits on the leader's outcome. The
	// durable claim covers every other process.
	flightMu sync.Mutex
	flights  map[string]*quoteFlight

	// onWaiterJoin, when not nil, runs when a caller joins a flight as
	// a waiter. The tests set it to make the join deterministic, so a
	// leader never finishes before the waiter that must share its
	// outcome is on the flight.
	onWaiterJoin func(quoteID string)
}

// A Store satisfies cost.ChargeSink, so a meter can take it as the sink that
// records settled charges.
var _ cost.ChargeSink = (*Store)(nil)

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
// frees the budget a dead process was holding. It refuses a file whose stored
// denomination differs from cfg.Denomination, because a reopen under another
// name would silently reprice every recorded amount. A cancelled context stops
// the open and no store comes back, but the schema, its migration ledger row and
// the ceiling may already have landed. Those bytes are inert, so a caller
// may retry the open and a second open on the same file is unaffected.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("sqlitestore: open: %w: DB must not be nil", ErrInvalid)
	}
	if cfg.Limit < 0 {
		return nil, fmt.Errorf("sqlitestore: open: limit %d: %w", cfg.Limit, cost.ErrNegativeLimit)
	}
	switch cfg.Period {
	case cost.None, cost.DailyUTC, cost.MonthlyUTC:
	default:
		return nil, fmt.Errorf("sqlitestore: open: period %d: %w", cfg.Period, ErrInvalid)
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
	if cfg.QuoteTolerance < 0 {
		return nil, fmt.Errorf("sqlitestore: open: quote tolerance %d: %w", cfg.QuoteTolerance, ErrInvalid)
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
	store := &Store{
		db:             cfg.DB,
		now:            now,
		log:            logger,
		reservationTTL: ttl,
		period:         cfg.Period,
		denom:          cfg.Denomination,
		quoteTolerance: cfg.QuoteTolerance,
		reprice:        cfg.Reprice,
		flights:        make(map[string]*quoteFlight),
	}
	if err := store.checkDenomination(ctx); err != nil {
		return nil, fmt.Errorf("sqlitestore: open: %w", err)
	}
	if err := store.setLimit(ctx, cfg.Limit); err != nil {
		return nil, fmt.Errorf("sqlitestore: open: %w", err)
	}
	purged, err := store.purgeExpired(ctx, now())
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: open: %w", err)
	}
	swept, err := store.sweepQuotesAtOpen(ctx, now())
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: open: %w", err)
	}
	logger.InfoContext(ctx, "sqlitestore: opened", "limit", int64(cfg.Limit), "denomination", cfg.Denomination.Name, "expired_reservations", purged, "expired_quotes", swept)
	return store, nil
}

// checkDenomination refuses a file whose stored denomination differs from
// the configured one. A file with no ceiling row yet accepts anything,
// because the first open writes the denomination beside the ceiling.
func (s *Store) checkDenomination(ctx context.Context) error {
	var name string
	var minor int64
	err := s.db.Writer().QueryRowContext(ctx,
		`SELECT denomination, minor_units FROM cost_budget WHERE id = 1`).Scan(&name, &minor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sqlitestore: read budget: %w", err)
	}
	stored := cost.Denomination{Name: name, Minor: int(minor)}
	if stored != s.denom {
		return fmt.Errorf("sqlitestore: open: stored denomination %q does not match configured %q: %w",
			stored.Name, s.denom.Name, cost.ErrDenominationMismatch)
	}
	return nil
}

// Denomination reports the unit every price in this store counts. The zero
// value names USD nanodollars.
func (s *Store) Denomination() cost.Denomination {
	return s.denom
}

// setLimit writes the configured ceiling. The row is created on the first open
// and updated on every later one, so the stored spend survives a redeploy. The
// denomination is written with the ceiling on the first open, and every later
// open has already checked itself against the stored one.
func (s *Store) setLimit(ctx context.Context, limit cost.Price) error {
	const query = `INSERT INTO cost_budget (id, limit_nd, spent_nd, denomination, minor_units)
		VALUES (1, ?, 0, ?, ?)
		ON CONFLICT(id) DO UPDATE SET limit_nd = excluded.limit_nd`
	if _, err := s.db.Writer().ExecContext(ctx, query, int64(limit), s.denom.Name, int64(s.denom.Minor)); err != nil {
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
// for a call that already happened survives a crash. A charge that names a
// denomination the store does not speak is refused with an error matching
// cost.ErrDenominationMismatch, and no path converts it. A credit charge never
// lands in a dollar ledger, or the other way round. A cancelled context
// stops the write, so no charge is recorded.
func (s *Store) Add(ctx context.Context, c cost.Charge) error {
	if c.Denomination.Name != s.denom.Name {
		return fmt.Errorf("sqlitestore: add charge in %q over a %q store: %w",
			c.Denomination.Name, s.denom.Name, cost.ErrDenominationMismatch)
	}
	const query = `INSERT INTO cost_charge (kind, units, unit_price, ref, denomination, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`
	_, err := s.db.Writer().ExecContext(ctx, query,
		c.Kind, int64(c.Units), int64(c.UnitPrice), c.Ref, s.denom.Name, s.now().UnixMilli())
	if err != nil {
		return fmt.Errorf("sqlitestore: add charge: %w", err)
	}
	return nil
}

// Charges returns every recorded charge in insertion order. Each charge names
// the denomination it was written under.
func (s *Store) Charges(ctx context.Context) ([]cost.Charge, error) {
	return s.queryCharges(ctx, `SELECT kind, units, unit_price, ref, denomination FROM cost_charge ORDER BY id`)
}

// Total returns the sum of every charge in the ledger. It reports an error
// matching cost.ErrOverflow when the exact sum leaves the int64 range.
func (s *Store) Total(ctx context.Context) (cost.Price, error) {
	return s.total(ctx, `SELECT kind, units, unit_price, ref, denomination FROM cost_charge ORDER BY id`)
}

// TotalByKind returns the sum of the charges whose Kind equals kind. It reports
// an error matching cost.ErrOverflow when the exact sum leaves the int64 range.
func (s *Store) TotalByKind(ctx context.Context, kind string) (cost.Price, error) {
	return s.total(ctx,
		`SELECT kind, units, unit_price, ref, denomination FROM cost_charge WHERE kind = ? ORDER BY id`, kind)
}

// TotalForRef returns the sum of the charges whose Ref equals ref. It reports
// an error matching cost.ErrOverflow when the exact sum leaves the int64 range.
func (s *Store) TotalForRef(ctx context.Context, ref string) (cost.Price, error) {
	return s.total(ctx,
		`SELECT kind, units, unit_price, ref, denomination FROM cost_charge WHERE ref = ? ORDER BY id`, ref)
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
		_ = ledger.Add(ctx, c) // an in-memory add cannot fail
	}
	return ledger.Total()
}

// queryCharges runs one charge query and decodes every row. The store's
// ledger carries one denomination, so each charge is filled with the stored
// name beside the store's own minor-unit count.
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
			denom     string
		)
		if err := rows.Scan(&c.Kind, &units, &unitPrice, &c.Ref, &denom); err != nil {
			return nil, fmt.Errorf("sqlitestore: scan charge: %w", err)
		}
		c.Units = int(units)
		c.UnitPrice = cost.Price(unitPrice)
		c.Denomination = cost.Denomination{Name: denom, Minor: s.denom.Minor}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlitestore: read charges: %w", err)
	}
	return out, nil
}

// Limit returns the ceiling the store was opened with. A store with a
// period applies the same ceiling to each window in turn.
func (s *Store) Limit(ctx context.Context) (cost.Price, error) {
	state, err := s.loadGlobal(ctx, s.db.Reader(), s.now())
	if err != nil {
		return 0, err
	}
	return state.limit, nil
}

// Spent returns the sum of the prices Settle has booked. A store with no
// period returns the lifetime booked spend. A store with a period returns
// the spend booked since the current window began.
func (s *Store) Spent(ctx context.Context) (cost.Price, error) {
	state, err := s.loadGlobal(ctx, s.db.Reader(), s.now())
	if err != nil {
		return 0, err
	}
	return state.spent, nil
}

// SpentSince returns the spend booked on or after t, across every owner.
// It reads the settle history, so it serves any instant the history
// covers. A store with a period answers its current window through
// Spent, and any other window through here.
func (s *Store) SpentSince(ctx context.Context, t time.Time) (cost.Price, error) {
	return sumSettles(ctx, s.db.Reader(), "", t.UnixMilli())
}

// Reserved returns the sum of the reservations that are still holding budget.
// An expired reservation never counts, so a dead process stops holding budget
// when its hold expires. A store with a period counts only holds taken
// since the current window began, so a hold counts against the window its
// caller took it in.
func (s *Store) Reserved(ctx context.Context) (cost.Price, error) {
	state, err := s.loadGlobal(ctx, s.db.Reader(), s.now())
	if err != nil {
		return 0, err
	}
	return state.reserved, nil
}

// Remaining returns the headroom left for new reservations. It is the ceiling
// minus booked spend minus unexpired holds. A store with a period reads the
// spend and the holds of the current window, so each window starts with a
// full ceiling. Once the pool holds a grant, the live grant balance minus
// the unexpired holds bounds the headroom too, and the smaller bound is the
// answer. It reports an error matching cost.ErrOverflow when that difference
// leaves the int64 range.
func (s *Store) Remaining(ctx context.Context) (cost.Price, error) {
	state, err := s.loadGlobal(ctx, s.db.Reader(), s.now())
	if err != nil {
		return 0, err
	}
	return remainingOf(state)
}

// Reserve commits estimate against the durable budget and returns the hold. It
// fails with an error matching cost.ErrOverBudget when the ceiling, the booked
// spend and the unexpired holds leave less than estimate, and it commits
// nothing then. Once the pool holds a grant, the live grant balance minus the
// unexpired holds bounds the reserve the same way, and the smaller bound
// decides. A store with a period checks the ceiling against the current
// window alone. The check and the insert share one transaction, so concurrent
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
	state, err := s.loadGlobal(ctx, tx, now)
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
// and the charge is a fact. The booking extends the settle history, so a
// store with a period counts it in the current window. It draws actual from
// the unexpired grants that expire soonest first, so a short grant is
// consumed before a longer one. It reports an error
// matching cost.ErrOverflow and commits nothing when actual would push the
// booked spend past the int64 range. A cancelled context stops the settle,
// so nothing is booked.
func (s *Store) Settle(ctx context.Context, r Reservation, actual cost.Price) error {
	now := s.now()
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
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO cost_settle (owner, amount_nd, created_at) VALUES (?, ?, ?)`,
		"", int64(actual), now.UnixMilli()); err != nil {
		return fmt.Errorf("sqlitestore: settle: %w", err)
	}
	if err := drawGrants(ctx, tx, now, actual); err != nil {
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
// so one budget read serves a plain read and a reserve alike. Both the pool
// handle and a transaction satisfy it.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// budgetState is the numbers the budget arithmetic needs, read in one
// place so a read and a reserve never disagree. reserved counts the
// unexpired holds of the current window, which the ceiling reads. holds
// counts every unexpired hold, which the grant balance reads, because a
// hold reserves real credit whatever window it was taken in. funded is the
// live grant balance and granted says whether the pool was ever granted
// credit, because a pool with no grant draws on its ceiling alone.
type budgetState struct {
	limit    cost.Price
	spent    cost.Price
	reserved cost.Price
	holds    cost.Price
	funded   cost.Price
	granted  bool
}

// windowSince returns the created_at lower bound in millis for windowed
// budget reads. It returns zero when the store keeps a lifetime budget,
// so the bound matches every row.
func (s *Store) windowSince(now time.Time) int64 {
	if s.period == cost.None {
		return 0
	}
	return s.period.Start(now).UnixMilli()
}

// loadGlobal reads the global ceiling, the booked spend, the open holds and
// the grant pool. A store with no period reads the lifetime booked spend. A
// store with a period sums the settles booked since the current window
// began, so one ceiling bounds each window in turn. Open holds count
// against the window they were taken in, because the read bounds them by
// creation time. The grant balance counts against every window, because
// lapsed or spent credit stays gone whatever the calendar says.
func (s *Store) loadGlobal(ctx context.Context, q querier, now time.Time) (budgetState, error) {
	var state budgetState
	var limit, lifetime int64
	if err := q.QueryRowContext(ctx,
		`SELECT limit_nd, spent_nd FROM cost_budget WHERE id = 1`).Scan(&limit, &lifetime); err != nil {
		return budgetState{}, fmt.Errorf("sqlitestore: read budget: %w", err)
	}
	state.limit = cost.Price(limit)
	since := s.windowSince(now)
	if since == 0 {
		state.spent = cost.Price(lifetime)
	} else {
		spent, err := sumSettles(ctx, q, "", since)
		if err != nil {
			return budgetState{}, err
		}
		state.spent = spent
	}
	var holds, reserved int64
	if err := q.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_nd), 0),
			COALESCE(SUM(CASE WHEN created_at >= ? THEN amount_nd ELSE 0 END), 0)
		FROM cost_reservation WHERE expires_at > ?`,
		since, now.UnixMilli()).Scan(&holds, &reserved); err != nil {
		return budgetState{}, fmt.Errorf("sqlitestore: read reservations: %w", err)
	}
	state.reserved, state.holds = cost.Price(reserved), cost.Price(holds)
	var granted int
	if err := q.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM cost_grant)`).Scan(&granted); err != nil {
		return budgetState{}, fmt.Errorf("sqlitestore: read grants: %w", err)
	}
	state.granted = granted != 0
	if state.granted {
		funded, err := grantBalance(ctx, q, now)
		if err != nil {
			return budgetState{}, err
		}
		state.funded = funded
	}
	return state, nil
}

// sumSettles returns the spend booked on or after sinceMillis. An empty
// owner sums every owner, so the global ceiling reads the whole store.
// It reports an error matching cost.ErrOverflow when the exact sum leaves
// the int64 range.
func sumSettles(ctx context.Context, q querier, owner string, sinceMillis int64) (cost.Price, error) {
	var query string
	var args []any
	if owner == "" {
		query = `SELECT amount_nd FROM cost_settle WHERE created_at >= ?`
		args = []any{sinceMillis}
	} else {
		query = `SELECT amount_nd FROM cost_settle WHERE owner = ? AND created_at >= ?`
		args = []any{owner, sinceMillis}
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: read settle history: %w", err)
	}
	defer rows.Close() // the cursor is drained before the caller returns
	var total cost.Price
	for rows.Next() {
		var amount int64
		if err := rows.Scan(&amount); err != nil {
			return 0, fmt.Errorf("sqlitestore: scan settle history: %w", err)
		}
		total, err = addPrice(total, cost.Price(amount))
		if err != nil {
			return 0, err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("sqlitestore: read settle history: %w", err)
	}
	return total, nil
}

// remainingOf computes the headroom. It subtracts the holds first, because the
// ceiling and the holds are both non-negative, so that difference cannot leave
// the int64 range. Subtracting the booked spend last reports an overflow only
// when the headroom itself is unrepresentable. When the pool was ever granted
// credit, the live grant balance minus every unexpired hold bounds the
// headroom too, and the smaller of the two bounds decides.
func remainingOf(state budgetState) (cost.Price, error) {
	left, err := subPrice(state.limit, state.reserved)
	if err != nil {
		return 0, err
	}
	left, err = subPrice(left, state.spent)
	if err != nil {
		return 0, err
	}
	if !state.granted {
		return left, nil
	}
	funded, err := subPrice(state.funded, state.holds)
	if err != nil {
		return 0, err
	}
	if funded < left {
		return funded, nil
	}
	return left, nil
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
