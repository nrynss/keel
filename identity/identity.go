// Package identity carries guest sessions, sign-in, and account deletion.
//
// A first visit creates a guest user row and a server side session row.
// The browser holds a signed cookie that carries only the session id, and
// a native client sends the same signed value as a bearer token. Every
// request resolves the token against the rows, so revoking a session takes
// effect on the next request.
//
// Sign-in arrives by emailed code or through an external provider. A
// sign-in attaches to the current guest when the address or the provider
// subject is fresh, so the user id never changes and the guest's rows
// become the account's rows. An address that already belongs to another
// user moves the device to that user, or refuses with a conflict while
// the guest still owns app data.
//
// Deleting an account needs a code typed within its short life. The work
// runs as one durable job that fans out over app-registered targets and
// removes the user row last, so a restart resumes what is left.
//
// The rows live behind the Store interface, which the sqlitestore
// sub-package implements over one shared SQLite file.
package identity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nrynss/keel/erase"
	"github.com/nrynss/keel/id"
	"github.com/nrynss/keel/job"
	"github.com/nrynss/keel/mediastore"
	"github.com/nrynss/keel/wire"
)

// KindGuest marks a user row created on a first visit.
const KindGuest = "guest"

// KindOwner marks a user row with a sign-in. The sign-in flows attach an
// identity to the current user and flip its kind to this value.
const KindOwner = "owner"

// defaultCookieName is the session cookie name when Config leaves it
// empty.
const defaultCookieName = "identity_session"

// defaultCookieLifetime is how long the browser keeps the session cookie
// when Config leaves the lifetime at zero. A returning visitor stays on
// one user row across restarts, while the server rows remain revocable
// and erasable at any time.
const defaultCookieLifetime = 180 * 24 * time.Hour

// providerEmail names the emailed-code identity a sign-in stores.
const providerEmail = "email"

// Sentinel errors. Every failure path this package produces wraps one of
// these, so callers branch with errors.Is.
var (
	// ErrInvalid reports input or configuration this package cannot
	// honour, such as an empty signing key or a blank address.
	ErrInvalid = errors.New("identity: invalid input")

	// ErrNoSession reports a request with no usable session. The token is
	// absent, malformed, badly signed, unknown, or revoked.
	ErrNoSession = errors.New("identity: no session")

	// ErrUnknownSession is the Store contract for a session id with no
	// row. Resolution turns it into ErrNoSession.
	ErrUnknownSession = errors.New("identity: unknown session")

	// ErrUnknownIdentity is the Store contract for a provider subject
	// with no row.
	ErrUnknownIdentity = errors.New("identity: unknown identity")

	// ErrUnknownCode is the Store contract for a sign-in code with no
	// live row.
	ErrUnknownCode = errors.New("identity: unknown code")

	// ErrInvalidCode reports a sign-in code the server refuses. The code
	// is unknown, requested by another session, expired, used, or past
	// its attempts. One sentinel covers every case, so the answer never
	// names an account.
	ErrInvalidCode = errors.New("identity: invalid code")

	// ErrGuestDataConflict reports a sign-in that would strand the
	// guest's app data on this device. Nothing changes, and the caller
	// repeats the request with the switch choice to move anyway.
	ErrGuestDataConflict = errors.New("identity: guest data conflict")

	// ErrNotConfigured reports a sign-in route the process cannot serve.
	// The code key or the mail sender is missing, so the route answers
	// 500 until the boot wires both.
	ErrNotConfigured = errors.New("identity: sign-in is not configured")

	// ErrSendLimited reports a code mail the ceilings refuse. The caller
	// answers 429 with the wait the limiter returns, and sends nothing.
	// Known and unknown addresses share this error, so the answer never
	// reveals who registered.
	ErrSendLimited = errors.New("identity: code send limited")

	// ErrNoRunner reports a deletion asked while no job runner is bound.
	// The app binds the runner that carries the deletion kind before the
	// first deletion starts.
	ErrNoRunner = errors.New("identity: deletion runner is not bound")

	// ErrNotOwner reports an account the request does not hold. Deletion
	// refuses an address that names an identity on another user, so a
	// stranger learns nothing about the account.
	ErrNotOwner = errors.New("identity: not the account owner")

	// ErrProviderState reports a provider callback with no usable start.
	// The state is unknown, expired, or bound to another session, so the
	// callback stops before any provider call.
	ErrProviderState = errors.New("identity: provider sign-in state is unknown or expired")

	// ErrProviderToken reports a provider answer the server refuses. The
	// code exchange failed, or the token carries the wrong issuer,
	// audience, expiry, nonce, signature or subject. One sentinel covers
	// every case, so the answer never teaches which guard tripped.
	ErrProviderToken = errors.New("identity: provider token was refused")
)

// Sender delivers one sign-in code to one address. The provider client
// stays in the app, which composes its own subject and body around the
// code it receives.
type Sender interface {
	// Send delivers code to the normalized address. A nil return means
	// the provider accepted the message.
	Send(ctx context.Context, address, code string) error
}

// GuestData reports whether one user owns app data. A sign-in consults it
// for the guest before the device moves to another user, because a move
// would strand that data on the guest row. An app with no data model
// leaves the field nil, and no conflict is ever reported.
type GuestData func(ctx context.Context, userID string) (bool, error)

// TargetSource lists the erase targets one user owns. Account deletion
// fans out over the list, and rebuilds it from the user id when a
// restart resumes the work.
type TargetSource func(ctx context.Context, userID string) ([]erase.Target, error)

// Config configures a Service. Store, SigningKey must be set. CodeKey and
// Mail together enable the code sign-in, and either one alone leaves it
// disabled. Eraser and Targets together enable account deletion. Now and
// Log default to the wall clock and slog.Default.
type Config struct {
	// Store keeps the user, session, identity and code rows. It must not
	// be nil.
	Store Store

	// SigningKey signs the session token. The app loads it from its
	// secret source and injects the bytes here. It must not be empty.
	SigningKey []byte

	// CodeKey signs the sign-in code and address hashes, so a stolen
	// database copy verifies nothing and reveals neither. Empty disables
	// the code sign-in.
	CodeKey []byte

	// Mail sends one sign-in code. Nil disables the code sign-in.
	Mail Sender

	// CookieName is the session cookie name. Empty means
	// defaultCookieName.
	CookieName string

	// CookieLifetime is how long the browser keeps the session cookie.
	// Zero means defaultCookieLifetime.
	CookieLifetime time.Duration

	// GuestData reports whether a user owns app data. Nil means no user
	// ever conflicts.
	GuestData GuestData

	// Eraser fans the account deletion out over the targets. Nil disables
	// account deletion.
	Eraser *erase.Eraser

	// Targets lists the targets one user owns. Required when Eraser is
	// set. The deletion starts its erasure with the user id as the ref,
	// so the eraser's Source rebuilds the same list from it.
	Targets TargetSource

	// Now supplies the clock every stamp and every expiry reads. Nil
	// means time.Now.
	Now func() time.Time

	// Log receives one line per server fault, such as a session row that
	// could not be read. Nil means slog.Default.
	Log *slog.Logger
}

// User is one user row, a guest or an owner. Handlers compare its ID
// against the owner of the rows they read.
type User struct {
	// ID is the user id. It never changes across a sign-in that attaches
	// to this row.
	ID string
	// Kind is KindGuest or KindOwner.
	Kind string
	// CreatedAt is when the row was minted.
	CreatedAt time.Time
	// LastSeen is the last request the row served.
	LastSeen time.Time
}

// Session is one server-side session row. The token the client holds
// names the row, so a revoked row refuses on the next request.
type Session struct {
	// ID is the session id inside the signed token.
	ID string
	// UserID is the user the session belongs to.
	UserID string
	// CreatedAt is when the session was opened.
	CreatedAt time.Time
	// Revoked reports the session no longer resolves. The Store reports
	// the stored flag, and resolution treats a revoked row as no session.
	Revoked bool
}

// Service resolves sessions, runs the sign-in flows, and deletes
// accounts. Create it with New, because the zero value has no store and
// no key. A Service is safe for concurrent use.
type Service struct {
	store          Store
	key            []byte
	codeKey        []byte
	mail           Sender
	guestData      GuestData
	eraser         *erase.Eraser
	targets        TargetSource
	limiter        *sendLimiter
	cookieName     string
	cookieLifetime time.Duration
	now            func() time.Time
	log            *slog.Logger

	// pending holds the live provider sign-in starts, keyed by state.
	// Every access sweeps expired rows first. Guarded by mu.
	mu      sync.Mutex
	pending map[string]pendingStart

	// runner is the bound job runner, and bound closes when it lands. A
	// resumed deletion waits on the channel, because recovery schedules
	// it while the runner opens, ahead of the app's BindRunner call.
	runner *job.Runner
	bound  chan struct{}
}

// pendingStart binds one provider sign-in start to its callback. The
// nonce and the verifier stay server side, so the client carries only
// the state. Subject fills in after the first validation, which keeps
// the entry alive for the switch retry past a conflict.
type pendingStart struct {
	nonce     string
	verifier  string
	sessionID string
	userID    string
	subject   string
	validated bool
	expires   time.Time
}

// New migrates nothing and returns the Service. The app opens its store
// with identity/sqlitestore, which applies the schema, before it calls
// New.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("%w: store must not be nil", ErrInvalid)
	}
	if len(cfg.SigningKey) == 0 {
		return nil, fmt.Errorf("%w: signing key must not be empty", ErrInvalid)
	}
	if (cfg.Eraser == nil) != (cfg.Targets == nil) {
		return nil, fmt.Errorf("%w: deletion needs an eraser and a target source", ErrInvalid)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	s := &Service{
		store:          cfg.Store,
		key:            cfg.SigningKey,
		codeKey:        cfg.CodeKey,
		mail:           cfg.Mail,
		guestData:      cfg.GuestData,
		eraser:         cfg.Eraser,
		targets:        cfg.Targets,
		cookieName:     cfg.CookieName,
		cookieLifetime: cfg.CookieLifetime,
		pending:        make(map[string]pendingStart),
		now:            now,
		log:            log,
		runner:         nil,
		bound:          make(chan struct{}),
	}
	if s.cookieName == "" {
		s.cookieName = defaultCookieName
	}
	if s.cookieLifetime <= 0 {
		s.cookieLifetime = defaultCookieLifetime
	}
	if len(s.codeKey) > 0 && s.mail != nil {
		limiter, err := newSendLimiter(s.store, now)
		if err != nil {
			return nil, fmt.Errorf("identity: open: %w", err)
		}
		s.limiter = limiter
	}
	return s, nil
}

// Resolve returns the user and session behind the request token without
// minting. The token travels as the session cookie or as the bearer
// value of the Authorization header, and both carry the same signed
// value. Resolve returns ErrNoSession when the token is absent,
// malformed, badly signed, unknown, or revoked. A resolved session
// touches the user's last seen time.
func (s *Service) Resolve(r *http.Request) (User, Session, error) {
	return s.resolve(r)
}

// Middleware ensures every request carries a user. A request with a valid
// token keeps its user. Any other request mints a fresh guest and sets
// its cookie, so the response orients the next visit. A revoked token
// never resolves again, so the request it rides on still fails every
// ownership check. A database fault answers 500.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, session, err := s.resolve(r)
		if err == nil {
			s.serve(next, w, r, user, session)
			return
		}
		if !errors.Is(err, ErrNoSession) {
			s.log.Warn("identity: resolve session", "error", err)
			_ = wire.WriteError(w, http.StatusInternalServerError, wire.CodeInternal, "the session could not be resolved", nil)
			return
		}
		user, session, err = s.mint(r.Context())
		if err != nil {
			s.log.Warn("identity: mint guest", "error", err)
			_ = wire.WriteError(w, http.StatusInternalServerError, wire.CodeInternal, "the session could not be opened", nil)
			return
		}
		s.setSessionCookie(w, session.ID)
		s.serve(next, w, r, user, session)
	})
}

// serve carries the resolved user and session into the request context
// and runs the next handler.
func (s *Service) serve(next http.Handler, w http.ResponseWriter, r *http.Request, user User, session Session) {
	ctx := context.WithValue(r.Context(), userContextKey{}, user)
	ctx = context.WithValue(ctx, sessionContextKey{}, session)
	next.ServeHTTP(w, r.WithContext(ctx))
}

// Owns reports whether the request user owns ownerID. It reads only the
// context the Middleware set, so it never touches the store. An empty
// ownerID never matches.
func (s *Service) Owns(ctx context.Context, ownerID string) bool {
	user, ok := UserFromContext(ctx)
	if !ok || ownerID == "" {
		return false
	}
	return user.ID == ownerID
}

// AuthorizeMedia reports whether a request may read a private blob. It
// fits mediastore.Config.Authorize directly. A public blob passes. Any
// other blob passes only when the request session owns the owner name
// the blob was persisted with. Persist each private blob with Owner set
// to its user id. A refusal returns false, and the media store answers
// the same 404 as an unknown id.
func (s *Service) AuthorizeMedia(r *http.Request, blob mediastore.Blob) bool {
	if blob.Visibility == mediastore.Public {
		return true
	}
	user, _, err := s.resolve(r)
	if err != nil {
		return false
	}
	return blob.Owner != "" && user.ID == blob.Owner
}

// Revoke marks a session revoked. The next request carrying it resolves
// as anonymous, because every request reads the rows. Revoking an
// unknown id succeeds, since an unknown id is already unusable.
func (s *Service) Revoke(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("%w: session id must not be empty", ErrInvalid)
	}
	if err := s.store.RevokeSession(ctx, sessionID); err != nil {
		return fmt.Errorf("identity: revoke session: %w", err)
	}
	return nil
}

// SetSessionCookie writes the session cookie for one session id. The
// value carries only the session id and its signature, never user data.
// The sign-in handlers call it for a rotated session, and it writes the
// same cookie the Middleware sets for a minted guest.
func (s *Service) SetSessionCookie(w http.ResponseWriter, sessionID string) {
	s.setSessionCookie(w, sessionID)
}

// UserFromContext returns the user the Middleware resolved for this
// request. It returns false when no middleware ran.
func UserFromContext(ctx context.Context) (User, bool) {
	user, ok := ctx.Value(userContextKey{}).(User)
	return user, ok
}

// SessionFromContext returns the session the Middleware resolved for this
// request. It returns false when no middleware ran. Handlers that rotate
// or revoke the session read it here instead of parsing the token again.
func SessionFromContext(ctx context.Context) (Session, bool) {
	session, ok := ctx.Value(sessionContextKey{}).(Session)
	return session, ok
}

// resolve validates the request token against the rows. The cookie wins
// over the bearer header, and both carry the same signed value. A valid
// session touches the last seen time before it returns.
func (s *Service) resolve(r *http.Request) (User, Session, error) {
	token, ok := requestToken(r, s.cookieName)
	if !ok {
		return User{}, Session{}, ErrNoSession
	}
	sessionID, ok := verifyToken(token, s.key)
	if !ok {
		return User{}, Session{}, ErrNoSession
	}
	session, user, err := s.store.Session(r.Context(), sessionID)
	if errors.Is(err, ErrUnknownSession) {
		return User{}, Session{}, ErrNoSession
	}
	if err != nil {
		return User{}, Session{}, fmt.Errorf("identity: resolve session: %w", err)
	}
	if session.Revoked {
		return User{}, Session{}, ErrNoSession
	}
	stamp := s.now()
	if err := s.store.TouchUser(r.Context(), user.ID, stamp); err != nil {
		return User{}, Session{}, fmt.Errorf("identity: touch last seen: %w", err)
	}
	user.LastSeen = stamp
	return user, session, nil
}

// mint creates a guest user row and its session row. Either both rows
// land or neither does.
func (s *Service) mint(ctx context.Context) (User, Session, error) {
	userID, err := id.New()
	if err != nil {
		return User{}, Session{}, fmt.Errorf("identity: mint guest: %w", err)
	}
	sessionID, err := id.New()
	if err != nil {
		return User{}, Session{}, fmt.Errorf("identity: mint guest: %w", err)
	}
	stamp := s.now()
	user := User{ID: userID, Kind: KindGuest, CreatedAt: stamp, LastSeen: stamp}
	session := Session{ID: sessionID, UserID: userID, CreatedAt: stamp}
	if err := s.store.CreateGuest(ctx, user, session); err != nil {
		return User{}, Session{}, fmt.Errorf("identity: mint guest: %w", err)
	}
	return user, session, nil
}

// signToken authenticates a session id for its token value.
func (s *Service) signToken(sessionID string) string {
	return signToken(sessionID, s.key)
}

// setSessionCookie writes the session cookie for one session id.
func (s *Service) setSessionCookie(w http.ResponseWriter, sessionID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName,
		Value:    s.signToken(sessionID),
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.cookieLifetime.Seconds()),
		Expires:  s.now().Add(s.cookieLifetime),
	})
}

// clearSessionCookie drops the session cookie. The deletion answers with
// it, because the deletion revokes every session the user holds.
func (s *Service) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

// signToken authenticates a session id with the signing key. The value
// carries the id and its signature and nothing else.
func signToken(sessionID string, key []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(sessionID))
	return sessionID + "." + hex.EncodeToString(mac.Sum(nil))
}

// verifyToken splits a token value and checks its signature in constant
// time. It returns false for any malformed or forged value.
func verifyToken(value string, key []byte) (string, bool) {
	sessionID, sig, found := strings.Cut(value, ".")
	if !found || sessionID == "" || sig == "" {
		return "", false
	}
	if !id.Valid(sessionID) {
		return "", false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(sessionID))
	want, err := hex.DecodeString(sig)
	if err != nil {
		return "", false
	}
	if !hmac.Equal(mac.Sum(nil), want) {
		return "", false
	}
	return sessionID, true
}

// requestToken returns the signed session token a request carries. The
// cookie comes first, and the Authorization bearer header is the same
// token in another transport.
func requestToken(r *http.Request, cookieName string) (string, bool) {
	if cookie, err := r.Cookie(cookieName); err == nil && cookie.Value != "" {
		return cookie.Value, true
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", false
	}
	const scheme = "Bearer "
	if len(auth) <= len(scheme) || !strings.EqualFold(auth[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(auth[len(scheme):])
	if token == "" {
		return "", false
	}
	return token, true
}

// userContextKey carries the resolved user. Its unexported type keeps
// other packages from colliding with it.
type userContextKey struct{}

// sessionContextKey carries the resolved session. Its unexported type
// keeps other packages from colliding with it.
type sessionContextKey struct{}
