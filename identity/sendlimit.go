package identity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"time"

	"github.com/nrynss/keel/gate"
)

// Send ceilings. Every ceiling refuses before the send, never after it,
// so a refused request leaves no row a sender could bill. The
// identical-response rule holds above this limiter. A refused known
// address and a refused unknown address take the same shape, and the
// route answers both alike.
const (
	// sendAddressBurst caps the codes one address may receive per window.
	// The rows carry every send, so the count reads the store and
	// survives a restart.
	sendAddressBurst = 5
	// sendAddressWindow is the sliding window the address cap counts.
	sendAddressWindow = 24 * time.Hour
	// sendClientBurst caps the code requests one client may make per
	// window. The client key comes from the shared gate, exactly as the
	// rest of the app keys clients, so a proxy header policy change
	// applies here with no second config.
	sendClientBurst = 10
	// sendClientWindow is the window the client cap sustains. The gate
	// restores one token per interval, so the interval is the window
	// divided by the burst.
	sendClientWindow = time.Hour
	// sendGlobalBurst caps the code mails the whole process may send per
	// window. The store window is the durable record, and the gate
	// bucket beside it is the same ceiling in token form, because the
	// rule needs both halves.
	sendGlobalBurst = 200
	// sendGlobalWindow is the sliding window the global cap counts.
	sendGlobalWindow = 24 * time.Hour
)

// sendRuleName names the gate bucket family for code sends. One name
// keeps the client and global buckets of this route apart from every
// other route the process guards.
const sendRuleName = "identity-code-send"

// sendLimiter guards code mails with three ceilings. It is safe for
// concurrent use. The service builds one in New when the code sign-in is
// configured, and consults it before every send. The route answers a
// refusal with 429 and the returned wait, known and unknown addresses
// alike.
type sendLimiter struct {
	store Store
	now   func() time.Time
	probe http.Handler
}

// newSendLimiter returns a limiter reading past sends from store, with
// the refill clock the service injected.
func newSendLimiter(store Store, now func() time.Time) (*sendLimiter, error) {
	inner, err := gate.New(gate.Config{Now: now})
	if err != nil {
		return nil, fmt.Errorf("identity: open send gate: %w", err)
	}
	probe, err := inner.Protect(gate.Rule{
		Name:      sendRuleName,
		PerClient: gate.Limit{Burst: sendClientBurst, Every: sendClientWindow / sendClientBurst},
		Global:    gate.Limit{Burst: sendGlobalBurst, Every: sendGlobalWindow / sendGlobalBurst},
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	if err != nil {
		return nil, fmt.Errorf("identity: guard code sends: %w", err)
	}
	return &sendLimiter{store: store, now: now, probe: probe}, nil
}

// Allow reports whether one code mail may go out to addressHash. It
// returns the wait until a retry may succeed and ok true when the send
// may proceed. A refusal consumes no durable budget. The store windows
// run first and the gate probe runs last, so a refused request spends no
// client token either. A refused address, known or not, takes the same
// shape, so the route answers both alike.
func (l *sendLimiter) Allow(ctx context.Context, addressHash string, r *http.Request) (time.Duration, bool, error) {
	if addressHash == "" {
		return 0, false, fmt.Errorf("%w: send limiter needs an address hash", ErrInvalid)
	}
	if r == nil {
		return 0, false, fmt.Errorf("%w: send limiter needs the request", ErrInvalid)
	}
	now := l.now()
	address, err := l.store.CodeSends(ctx, addressHash, now.Add(-sendAddressWindow))
	if err != nil {
		return 0, false, fmt.Errorf("identity: count code sends: %w", err)
	}
	if wait, limited := overWindow(address, sendAddressBurst, sendAddressWindow, now); limited {
		return wait, false, nil
	}
	global, err := l.store.AllCodeSends(ctx, now.Add(-sendGlobalWindow))
	if err != nil {
		return 0, false, fmt.Errorf("identity: count code sends: %w", err)
	}
	if wait, limited := overWindow(global, sendGlobalBurst, sendGlobalWindow, now); limited {
		return wait, false, nil
	}
	wait, limited, err := overClient(l.probe, r)
	if err != nil {
		return 0, false, err
	}
	if limited {
		return wait, false, nil
	}
	return 0, true, nil
}

// overWindow reports the wait once one window reaches its burst. The
// wait is the time until the oldest row ages out, never below one
// second, so a client never retries into the same refusal.
func overWindow(window SendWindow, burst int, span time.Duration, now time.Time) (time.Duration, bool) {
	if window.Count < int64(burst) {
		return 0, false
	}
	wait := window.Oldest.Add(span).Sub(now)
	if window.Oldest.IsZero() || wait < time.Second {
		wait = time.Second
	}
	return wait, true
}

// overClient draws one token from the shared gate for r. The gate keys
// the client exactly as the rest of the app does, so proxy trust stays
// in one place. It runs only after both store windows pass, so a request
// the store refuses never reaches it. A refusal consumes nothing, so
// hammering on 429s cannot spend the route budget. The wait comes from
// the refusal the gate wrote, in whole seconds.
func overClient(probe http.Handler, r *http.Request) (time.Duration, bool, error) {
	rec := httptest.NewRecorder()
	probe.ServeHTTP(rec, r)
	switch rec.Code {
	case http.StatusNoContent:
		return 0, false, nil
	case http.StatusTooManyRequests:
		seconds, err := strconv.Atoi(rec.Header().Get("Retry-After"))
		if err != nil || seconds < 1 {
			seconds = 1
		}
		return time.Duration(seconds) * time.Second, true, nil
	default:
		return 0, false, fmt.Errorf("%w: send gate answered %d", ErrSendLimited, rec.Code)
	}
}
