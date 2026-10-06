package identity

import (
	"context"
	"time"
)

// Store keeps the user, session, identity and sign-in code rows. The
// sqlitestore sub-package implements it over SQLite, and a test can
// implement it in memory. Implementations must be safe for concurrent
// use, and must return an error matching the sentinel each method names
// for its refusal, so a caller classifies with errors.Is.
type Store interface {
	// CreateGuest writes the user row and its session row in one
	// transaction, so either both rows land or neither does.
	CreateGuest(ctx context.Context, user User, session Session) error

	// Session reads one session row joined with its user. An unknown id
	// reports ErrUnknownSession. A revoked row is reported as stored,
	// with Revoked set, because revocation is the caller's decision.
	Session(ctx context.Context, id string) (Session, User, error)

	// TouchUser stamps the user's last seen time.
	TouchUser(ctx context.Context, userID string, at time.Time) error

	// RevokeSession marks one session revoked. An unknown id is not an
	// error, because the session is already unusable.
	RevokeSession(ctx context.Context, sessionID string) error

	// CreateIdentity attaches one sign-in identity to a user. A provider
	// subject that already names a row reports ErrIdentityTaken, and no
	// row changes.
	CreateIdentity(ctx context.Context, ident Identity) error

	// IdentityHolder reads the user id one provider subject is attached
	// to. An unknown pair reports ErrUnknownIdentity.
	IdentityHolder(ctx context.Context, provider, subject string) (string, error)

	// PutCode retires every live code the address hash and session
	// already hold, then stores the fresh code row, in one transaction.
	PutCode(ctx context.Context, code SignInCode) error

	// LiveCode reads the newest live code one address hash and session
	// hold. No live row reports ErrUnknownCode.
	LiveCode(ctx context.Context, addressHash, sessionID string) (SignInCode, error)

	// CloseCode retires one code row. A later LiveCode skips it.
	CloseCode(ctx context.Context, codeID string, at time.Time) error

	// DeleteCode removes one code row entirely. A send that failed never
	// delivered, so its row leaves the send ceilings untouched.
	DeleteCode(ctx context.Context, codeID string) error

	// CountCodeAttempt records one wrong try against one code row, as the
	// attempts value the caller read plus one.
	CountCodeAttempt(ctx context.Context, codeID string, attempts int) error

	// CodeSends counts the code rows one address hash holds since the
	// cutoff, with the oldest row's creation time.
	CodeSends(ctx context.Context, addressHash string, since time.Time) (SendWindow, error)

	// AllCodeSends counts the code rows every address holds since the
	// cutoff, with the oldest row's creation time.
	AllCodeSends(ctx context.Context, since time.Time) (SendWindow, error)

	// ResolveSignIn runs one verified sign-in in one transaction. It
	// consumes the code while it is still live, so a second verify of the
	// same code reports ErrUnknownCode. It attaches the identity to the
	// user that asked and flips the user kind to owner on a fresh
	// subject, keeps an identity that already names the user, and joins
	// the holder when a racing attach landed first. A join would strand
	// the asking user's data while GuestData holds and AllowSwitch does
	// not, and reports ErrGuestDataConflict with nothing changed. It
	// opens the new session for the target user and revokes the session
	// that asked.
	ResolveSignIn(ctx context.Context, in SignInResolution) (SignInTarget, error)

	// DeleteUserRows removes one user's code rows, session rows and
	// identity rows, then the user row last, in one transaction. Codes go
	// before sessions, and sessions and identities go before the user,
	// because each names the next. Every delete repeats safely.
	DeleteUserRows(ctx context.Context, userID string) error
}

// Identity is one sign-in identity attached to a user. The provider name
// and the subject together are the identity key, and the subject is the
// provider's account id, never an address.
type Identity struct {
	// Provider names the sign-in method, such as the emailed code or the
	// external provider.
	Provider string
	// Subject names the account behind the provider.
	Subject string
	// UserID is the user the identity is attached to.
	UserID string
	// CreatedAt is when the identity attached.
	CreatedAt time.Time
}

// SignInCode is one stored sign-in code row. Storage keeps hashes only,
// so a stolen database copy reveals neither the code nor the address.
type SignInCode struct {
	// ID is the code row id.
	ID string
	// AddressHash authenticates the normalized address with the code
	// key.
	AddressHash string
	// CodeHash authenticates the typed code with the code key.
	CodeHash string
	// RequestingSession is the session that asked, and the only session
	// that may verify the code.
	RequestingSession string
	// ExpiresAt bounds the code.
	ExpiresAt time.Time
	// Attempts counts the wrong tries the code absorbed.
	Attempts int
	// UsedAt is when the code was consumed, zero while it is live.
	UsedAt time.Time
	// CreatedAt is when the code was stored.
	CreatedAt time.Time
}

// SendWindow is one sliding-window count of code sends, as the send
// ceilings read it.
type SendWindow struct {
	// Count is the rows in the window.
	Count int64
	// Oldest is the window's oldest creation time, zero when the window
	// is empty.
	Oldest time.Time
}

// SignInResolution carries one verified sign-in a Store resolves in one
// transaction.
type SignInResolution struct {
	// SessionID is the session that asked, and the resolution revokes it.
	SessionID string
	// UserID is the user that asked.
	UserID string
	// NewSessionID is the fresh session the target user receives.
	NewSessionID string
	// Provider names the sign-in method.
	Provider string
	// Subject names the account.
	Subject string
	// CodeID is the code the resolution consumes.
	CodeID string
	// At stamps the identity, the kind flip and both sessions.
	At time.Time
	// GuestData reports whether UserID owns app data. It decides a
	// conflict on a racing attach, because the pre-read inside the
	// transaction cannot call the app back.
	GuestData bool
	// AllowSwitch reports the caller accepted a move to another user.
	AllowSwitch bool
}

// SignInTarget is the user a resolved sign-in landed on, with its fresh
// session.
type SignInTarget struct {
	// UserID is the target user.
	UserID string
	// SessionID is the fresh session for the target user.
	SessionID string
	// Switched reports a move to another user than the one that asked.
	Switched bool
}
