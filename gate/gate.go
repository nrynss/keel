// Package gate stands in front of the routes that spend money.
//
// A protected route may cost real money and take minutes of model time,
// and the process often listens on a public URL. An ungated button is
// therefore an open wallet, so the gate exists to be in place before a
// URL goes public.
//
// # Both a passcode and a per-client cap
//
// The gate enforces two things at once, and the cap is always on:
//
//   - Every protected route carries a two-level token bucket: one bucket
//     per client key, and one process-wide bucket the whole route shares.
//     This needs no configuration and keeps a demo a single tap for a
//     visitor.
//   - A shared passcode is enforced on the same routes when, and only
//     when, Config.Passcode is set. An operator flips it with one
//     setting and a restart if the URL is found and abused.
//
// The passcode is deliberately not an HTTP authentication scheme. It is
// read from a configurable header or cookie and answered with 403, never
// 401 with WWW-Authenticate, because a browser basic-auth prompt is
// worse than a 403 that a page already knows how to render.
//
// # Which routes the caller guards
//
// Protect wraps individual handlers, never the mux, so nothing is gated
// by accident. The caller decides which routes stay ungated.
//
// # The client key, behind a proxy
//
// Behind a reverse proxy the direct peer is always the proxy and never a
// client. Forwarded headers are therefore the only source of a client
// address, and they are attacker-controlled input:
//
//   - Headers are read only when the direct peer is inside
//     Config.TrustedProxies (loopback and the private ranges by default).
//     From an untrusted peer the peer address itself is the key and every
//     forwarding header is ignored.
//   - From a trusted peer, CF-Connecting-IP wins, because Cloudflare sets
//     it and overwrites whatever the client sent. X-Forwarded-For is the
//     fallback, and the rightmost entry is used, not the leftmost: the
//     leftmost is whatever the client chose to send, since each proxy
//     appends rather than replaces.
//   - IPv4 keys are the address. IPv6 keys are the /64, so one delegated
//     prefix is one client rather than a full address space of them.
//
// The per-client key is fairness, not security, and the doc says so
// plainly. Anyone who reaches the origin directly, bypassing Cloudflare,
// can put any CF-Connecting-IP they like on each request and mint a
// fresh bucket every time. That is why every Rule also carries a
// process-wide Global bucket that no client key can widen. The spoofable
// header can cost an honest client their share, but it cannot raise the
// total spend of the route above Global. Bucket memory is bounded too
// (Config.MaxClients, with full and debt-free buckets evicted first), so
// header spoofing is not a memory sink either.
//
// Configuration arrives through Config. This package never reads the
// environment, and its errors are sentinels matched with errors.Is.
package gate

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/nrynss/keel/wire"
)

// ErrInvalidConfig is returned by New for unusable configuration.
var ErrInvalidConfig = errors.New("gate: invalid config")

// ErrInvalidRule is returned by Protect's construction path for a rule
// with an empty name, a non-positive burst, or a non-positive refill
// interval. A rule that cannot be enforced must not silently pass
// traffic, so Protect returns the error rather than degrading.
var ErrInvalidRule = errors.New("gate: invalid rule")

// defaultPasscodeHeader is the header a caller supplies the shared
// passcode in when Config.PasscodeHeader is empty.
const defaultPasscodeHeader = "X-Passcode"

// defaultPasscodeCookie is the cookie the shared passcode may also
// arrive in when Config.PasscodeCookie is empty, so an operator who has
// entered it once is not asked again.
const defaultPasscodeCookie = "passcode"

// defaultMaxClients bounds the per-client bucket table. Each entry is a
// few dozen bytes, so this costs well under a megabyte and caps what a
// client-key spoofer can make the process allocate.
const defaultMaxClients = 4096

// retryDetail is the envelope detail of a rate-limit refusal. wire reads
// its retry_after_seconds member to set Retry-After, so the header and
// the body never disagree about the wait.
type retryDetail struct {
	// RetryAfterSeconds is the whole seconds a caller should wait, at
	// least 1.
	RetryAfterSeconds int `json:"retry_after_seconds"`
}

// Limit is one token bucket's shape: Burst tokens are available at once
// and one token is restored every Every. A zero Limit is invalid, see
// ErrInvalidRule, because "no limit" must be spelled by not protecting
// the route at all.
type Limit struct {
	// Burst is the bucket's capacity: how many requests may arrive back
	// to back from cold.
	Burst int
	// Every is the refill interval: one token per Every.
	Every time.Duration
}

// valid reports whether the limit can actually be enforced.
func (l Limit) valid() bool {
	return l.Burst > 0 && l.Every > 0
}

// Rule is what one protected route costs. Both limits are enforced and
// both must be valid: PerClient is fairness between callers, Global is
// the ceiling on the route as a whole and the only half a forged client
// key cannot widen.
type Rule struct {
	// Name identifies the rule's bucket family. Two routes sharing a
	// name share their buckets, which is how a retry path and its
	// original can be made to draw on one budget.
	Name string
	// PerClient is the budget one client key gets.
	PerClient Limit
	// Global is the budget the whole route gets, across every client.
	Global Limit
}

// Config configures New. The zero value is usable: no passcode, the
// default trusted-proxy set, the default client-table bound, and the
// neutral passcode header and cookie names.
type Config struct {
	// Passcode, when non-empty, is required on every protected route.
	// Empty (the default) means rate limits alone.
	Passcode string
	// PasscodeHeader names the header a caller supplies the shared
	// passcode in. Empty means defaultPasscodeHeader.
	PasscodeHeader string
	// PasscodeCookie names the cookie the shared passcode may also
	// arrive in. Empty means defaultPasscodeCookie.
	PasscodeCookie string
	// TrustedProxies lists the peer prefixes whose forwarding headers
	// are believed. Nil means loopback plus the private ranges, which a
	// reverse proxy on the same private network presents as. An
	// explicitly empty slice trusts nothing and keys every request on
	// its peer address.
	TrustedProxies []netip.Prefix
	// MaxClients bounds the per-client bucket table. Zero means
	// defaultMaxClients.
	MaxClients int
	// Now supplies the clock the token buckets refill from. Nil means
	// time.Now.
	Now func() time.Time
	// Log receives one line per refusal. Nil discards.
	Log *slog.Logger
}

// Gate holds the buckets and the passcode shared by every route it
// protects. Create it with New. The zero value is not usable. A Gate is
// safe for concurrent use.
type Gate struct {
	passcode       string
	passcodeHeader string
	passcodeCookie string
	trusted        []netip.Prefix
	maxClients     int
	now            func() time.Time
	log            *slog.Logger

	mu      sync.Mutex
	clients map[string]*bucket
	globals map[string]*bucket
}

// New returns a Gate from cfg. It fails only on a configuration that
// cannot be honoured. An empty Config is valid and yields the default
// posture (rate limits on, no passcode).
func New(cfg Config) (*Gate, error) {
	if cfg.MaxClients < 0 {
		return nil, fmt.Errorf("gate: new: %w: MaxClients must not be negative", ErrInvalidConfig)
	}
	maxClients := cfg.MaxClients
	if maxClients == 0 {
		maxClients = defaultMaxClients
	}
	passcodeHeader := cfg.PasscodeHeader
	if passcodeHeader == "" {
		passcodeHeader = defaultPasscodeHeader
	}
	passcodeCookie := cfg.PasscodeCookie
	if passcodeCookie == "" {
		passcodeCookie = defaultPasscodeCookie
	}
	trusted := cfg.TrustedProxies
	if trusted == nil {
		trusted = defaultTrustedProxies()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Gate{
		passcode:       cfg.Passcode,
		passcodeHeader: passcodeHeader,
		passcodeCookie: passcodeCookie,
		trusted:        trusted,
		maxClients:     maxClients,
		now:            now,
		log:            log,
		clients:        make(map[string]*bucket),
		globals:        make(map[string]*bucket),
	}, nil
}

// Protect wraps next with rule. It returns an error rather than an
// unenforced handler when the rule is unusable, so a typo in a limit
// cannot quietly reopen the wallet.
//
// The wrapped handler refuses with 403 and the passcode_required
// envelope when a passcode is configured and absent or wrong, and with
// 429 and the rate_limited envelope when either bucket is empty. A
// refusal consumes no tokens from either bucket: a client that is
// already over its limit cannot also drain the global budget.
func (g *Gate) Protect(rule Rule, next http.Handler) (http.Handler, error) {
	if rule.Name == "" {
		return nil, fmt.Errorf("gate: protect: %w: Name must not be empty", ErrInvalidRule)
	}
	if next == nil {
		return nil, fmt.Errorf("gate: protect %s: %w: next handler must not be nil", rule.Name, ErrInvalidRule)
	}
	if !rule.PerClient.valid() {
		return nil, fmt.Errorf("gate: protect %s: %w: PerClient needs a positive Burst and Every", rule.Name, ErrInvalidRule)
	}
	if !rule.Global.valid() {
		return nil, fmt.Errorf("gate: protect %s: %w: Global needs a positive Burst and Every", rule.Name, ErrInvalidRule)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.hasPasscode(r) {
			g.log.Warn("gate: passcode refused", "rule", rule.Name, "path", r.URL.Path)
			g.refuse(w, http.StatusForbidden, "passcode_required", "a shared passcode is required", 0)
			return
		}
		key := g.clientKey(r)
		wait, ok := g.allow(rule, key)
		if !ok {
			g.log.Warn("gate: rate limited", "rule", rule.Name, "client", key, "retry_after", wait.String())
			g.refuse(w, http.StatusTooManyRequests, "rate_limited", "the route rate limit was reached", wait)
			return
		}
		next.ServeHTTP(w, r)
	}), nil
}

// ProtectFunc is Protect over an http.HandlerFunc.
func (g *Gate) ProtectFunc(rule Rule, next http.HandlerFunc) (http.Handler, error) {
	if next == nil {
		return nil, fmt.Errorf("gate: protect %s: %w: next handler must not be nil", rule.Name, ErrInvalidRule)
	}
	return g.Protect(rule, next)
}

// hasPasscode reports whether the request carries the configured
// passcode. With no passcode configured every request passes. The
// comparison is constant time so a wrong passcode leaks nothing about
// how much of it was right.
func (g *Gate) hasPasscode(r *http.Request) bool {
	if g.passcode == "" {
		return true
	}
	if matches(r.Header.Get(g.passcodeHeader), g.passcode) {
		return true
	}
	cookie, err := r.Cookie(g.passcodeCookie)
	if err != nil {
		return false
	}
	return matches(cookie.Value, g.passcode)
}

// matches is a length-aware constant-time comparison. The length check
// is unavoidable (ConstantTimeCompare returns 0 for unequal lengths
// without comparing), and a passcode's length is not the secret.
func matches(provided, want string) bool {
	if len(provided) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(want)) == 1
}

// allow takes one token from the rule's global bucket and one from the
// client's, or from neither. It returns how long the caller should wait
// when it refuses.
//
// Both buckets are decided under one lock, and nothing is consumed
// unless both can pay. Charging the global bucket for a request the
// per-client bucket is about to refuse would let one hammering client
// spend the whole route's budget on 429s.
func (g *Gate) allow(rule Rule, clientKey string) (time.Duration, bool) {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()

	global, ok := g.globals[rule.Name]
	if !ok {
		global = newBucket(rule.Global, now)
		g.globals[rule.Name] = global
	}
	global.refill(now)

	key := rule.Name + "\x00" + clientKey
	client, ok := g.clients[key]
	if !ok {
		g.evictLocked()
		client = newBucket(rule.PerClient, now)
		g.clients[key] = client
	}
	client.refill(now)

	// Per-client is reported first when both are empty: it is the limit
	// the caller can do something about by waiting.
	if !client.available() {
		return client.retryAfter(), false
	}
	if !global.available() {
		return global.retryAfter(), false
	}
	client.take()
	global.take()
	return 0, true
}

// evictLocked keeps the client table under maxClients before an insert.
// A bucket at full capacity carries no debt, so dropping it is lossless,
// and those go first. Only if every bucket is in debt does the least
// recently used one go, which is the closest thing to lossless left.
//
// The caller holds g.mu.
func (g *Gate) evictLocked() {
	if len(g.clients) < g.maxClients {
		return
	}
	oldestKey, oldest := "", time.Time{}
	for key, b := range g.clients {
		if b.full() {
			delete(g.clients, key)
			return
		}
		if oldestKey == "" || b.last.Before(oldest) {
			oldestKey, oldest = key, b.last
		}
	}
	if oldestKey != "" {
		delete(g.clients, oldestKey)
	}
}

// refuse writes the gate's refusal through the shared error envelope, so
// a refusal carries the same shape as every other failed response. The
// envelope writer owns Content-Type, Cache-Control and, for a 429,
// Retry-After.
func (g *Gate) refuse(w http.ResponseWriter, status int, code, message string, wait time.Duration) {
	var detail any
	if wait > 0 {
		seconds := int(wait.Round(time.Second) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		detail = retryDetail{RetryAfterSeconds: seconds}
	}
	if err := wire.WriteError(w, status, code, message, detail); err != nil {
		g.log.Warn("gate: refusal write failed", "code", code, "error", err)
	}
}
