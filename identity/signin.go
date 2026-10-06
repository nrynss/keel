package identity

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/nrynss/keel/id"
	"github.com/nrynss/keel/wire"
)

// Sign-in code bounds. The code is six digits from the operating system
// generator and lives for ten minutes. Five wrong tries still leave a
// correct code usable. The sixth wrong try closes the code.
const (
	signInCodeExpiry   = 10 * time.Minute
	signInCodeWrongCap = 6
	signInCodeModulus  = 1000000
	signInMaxBody      = 1 << 20
)

// choiceSwitch is the choice a repeat verify carries to move the device
// to another user past a conflict.
const choiceSwitch = "switch"

// SignInOutcome carries a successful sign-in. UserID owns the device
// now. SessionID is the fresh session the handler sets as a cookie, and
// the old session row is revoked. Switched reports the device moved to
// another user, leaving its guest data on the guest row.
type SignInOutcome struct {
	// UserID owns the device after the sign-in.
	UserID string
	// SessionID is the fresh session id for the response cookie.
	SessionID string
	// Switched reports a move to another user.
	Switched bool
}

// normalizeAddress trims and lowercases one address. The hash, the
// send, and the identity subject all read this form, so one typed
// variant maps to one stored row.
func normalizeAddress(address string) string {
	return strings.ToLower(strings.TrimSpace(address))
}

// hashValue authenticates one address or code value with the code key.
// A stolen database copy verifies nothing and reveals neither.
func (s *Service) hashValue(value string) string {
	mac := hmac.New(sha256.New, s.codeKey)
	_, _ = mac.Write([]byte(value)) // hash.Write never returns an error
	return hex.EncodeToString(mac.Sum(nil))
}

// mintSignInCode draws six digits from the operating system generator.
func mintSignInCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(signInCodeModulus))
	if err != nil {
		return "", fmt.Errorf("identity: mint code: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// RequestSignInCode stores a fresh sign-in code for one session and
// hands it to the mail sender. It answers nil for every well formed
// address, whether or not an account holds it, so the caller never
// learns who registered. A new request retires earlier unused codes for
// the same address and session. A send failure removes the stored code
// and reports the failure, so no dead code lingers and no refused send
// counts against the ceilings. The stored row never holds the plain
// address, and storage keeps hashes only.
func (s *Service) RequestSignInCode(ctx context.Context, sessionID, address string) error {
	clean := normalizeAddress(address)
	if clean == "" || sessionID == "" {
		return fmt.Errorf("%w: address and session are required", ErrInvalid)
	}
	if len(s.codeKey) == 0 || s.mail == nil {
		return ErrNotConfigured
	}
	code, err := mintSignInCode()
	if err != nil {
		return err
	}
	codeID, err := id.New()
	if err != nil {
		return fmt.Errorf("identity: mint code: %w", err)
	}
	now := s.now()
	// The reservation claims one send slot inside the transaction that
	// inserts the row, so concurrent requests cannot oversend the durable
	// windows. A send that later fails releases the slot, so a refused
	// send never eats the budget.
	caps := SendCaps{
		Address: sendAddressBurst,
		Global:  sendGlobalBurst,
		Since:   now.Add(-sendAddressWindow),
	}
	if err := s.store.PutCode(ctx, SignInCode{
		ID:                codeID,
		AddressHash:       s.hashValue(clean),
		CodeHash:          s.hashValue(code),
		RequestingSession: sessionID,
		ExpiresAt:         now.Add(signInCodeExpiry),
		CreatedAt:         now,
	}, caps); err != nil {
		if errors.Is(err, ErrSendLimited) {
			return err
		}
		return fmt.Errorf("identity: store code: %w", err)
	}
	if err := s.mail.Send(ctx, clean, code); err != nil {
		if delErr := s.store.DeleteCode(ctx, codeID); delErr != nil {
			s.log.Warn("identity: remove unsent code", "error", delErr)
		}
		return fmt.Errorf("identity: send code: %w", err)
	}
	return nil
}

// checkCode finds the live code for one address and session and checks
// the typed value against it. A mismatch counts one attempt, and the
// sixth wrong try closes the code. An expired or exhausted code closes
// on sight. It returns the row id on a match and ErrInvalidCode on any
// refusal, so the answer never says which guard tripped.
func (s *Service) checkCode(ctx context.Context, address, sessionID, code string) (string, error) {
	row, err := s.store.LiveCode(ctx, s.hashValue(address), sessionID)
	if errors.Is(err, ErrUnknownCode) {
		return "", ErrInvalidCode
	}
	if err != nil {
		return "", fmt.Errorf("identity: read code: %w", err)
	}
	if !row.ExpiresAt.After(s.now()) {
		if err := s.store.CloseCode(ctx, row.ID, s.now()); err != nil {
			return "", err
		}
		return "", ErrInvalidCode
	}
	want, err := hex.DecodeString(row.CodeHash)
	if err != nil {
		return "", fmt.Errorf("identity: decode code hash: %w", err)
	}
	mac := hmac.New(sha256.New, s.codeKey)
	_, _ = mac.Write([]byte(strings.TrimSpace(code))) // hash.Write never returns an error
	if !hmac.Equal(mac.Sum(nil), want) {
		// The increment and the close at the cap are one write, so
		// concurrent wrong guesses cannot collapse onto one count.
		if err := s.store.RecordCodeAttempt(ctx, row.ID, signInCodeWrongCap, s.now()); err != nil {
			return "", fmt.Errorf("identity: record attempt: %w", err)
		}
		return "", ErrInvalidCode
	}
	return row.ID, nil
}

// VerifySignInCode checks one code and resolves the sign-in. It accepts
// only a code requested by this same session, unexpired, unused, and
// within five wrong attempts. On success it consumes the code, attaches
// or joins the identity, and rotates the session. A conflict without the
// switch choice changes nothing and reports ErrGuestDataConflict, so the
// caller repeats the verify with the switch choice to move anyway, and
// the code stays live for that repeat.
func (s *Service) VerifySignInCode(ctx context.Context, sessionID, userID, address, code, choice string) (SignInOutcome, error) {
	clean := normalizeAddress(address)
	if clean == "" || sessionID == "" || userID == "" || strings.TrimSpace(code) == "" {
		return SignInOutcome{}, fmt.Errorf("%w: session, user, address and code are required", ErrInvalid)
	}
	if choice != "" && choice != choiceSwitch {
		return SignInOutcome{}, fmt.Errorf("%w: unknown choice", ErrInvalid)
	}
	if len(s.codeKey) == 0 {
		return SignInOutcome{}, ErrNotConfigured
	}
	codeID, err := s.checkCode(ctx, clean, sessionID, code)
	if err != nil {
		return SignInOutcome{}, err
	}
	holder, err := s.store.IdentityHolder(ctx, providerEmail, clean)
	if err != nil && !errors.Is(err, ErrUnknownIdentity) {
		return SignInOutcome{}, fmt.Errorf("identity: read identity: %w", err)
	}
	ownsData := false
	if s.guestData != nil {
		ownsData, err = s.guestData(ctx, userID)
		if err != nil {
			return SignInOutcome{}, fmt.Errorf("identity: read guest data: %w", err)
		}
	}
	if holder != "" && holder != userID && ownsData && choice != choiceSwitch {
		return SignInOutcome{}, ErrGuestDataConflict
	}
	nextSession, err := id.New()
	if err != nil {
		return SignInOutcome{}, fmt.Errorf("identity: resolve sign-in: %w", err)
	}
	target, err := s.store.ResolveSignIn(ctx, SignInResolution{
		SessionID:    sessionID,
		UserID:       userID,
		NewSessionID: nextSession,
		Provider:     providerEmail,
		Subject:      clean,
		CodeID:       codeID,
		At:           s.now(),
		GuestData:    ownsData,
		AllowSwitch:  choice == choiceSwitch,
	})
	if err != nil {
		if errors.Is(err, ErrGuestDataConflict) {
			return SignInOutcome{}, err
		}
		if errors.Is(err, ErrUnknownCode) {
			return SignInOutcome{}, ErrInvalidCode
		}
		return SignInOutcome{}, fmt.Errorf("identity: resolve sign-in: %w", err)
	}
	return SignInOutcome(target), nil
}

// CodeSignInHandler serves the sign-in code routes on one handler. Mount
// it behind the Middleware, so every request carries a user and a
// session, at the two POST paths the caller names. A route the process
// cannot serve, for want of the code key or the mail sender, answers 500
// so a wiring fault surfaces instead of hiding.
func (s *Service) CodeSignInHandler(requestPath, verifyPath string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+requestPath, s.handleRequestCode)
	mux.HandleFunc("POST "+verifyPath, s.handleVerifyCode)
	return mux
}

// signInRequestJSON carries the address a code goes to.
type signInRequestJSON struct {
	// Address is the address the code goes to, before normalization.
	Address string `json:"address"`
}

// signInVerifyJSON carries one typed code with its address. Choice
// carries the switch value when the caller moves to the account despite
// guest data on this device.
type signInVerifyJSON struct {
	// Address is the address the code went to, before normalization.
	Address string `json:"address"`
	// Code is the typed six digit value.
	Code string `json:"code"`
	// Choice carries the switch value on a repeat verify past a
	// conflict.
	Choice string `json:"choice"`
}

// signInOKJSON answers a code request and a successful verify. The code
// answer is identical for known and unknown addresses.
type signInOKJSON struct {
	// OK reports the request landed.
	OK bool `json:"ok"`
}

// decodeSignInBody reads one JSON body up to the cap. It reports false
// when the body is missing or malformed, and the caller answers invalid
// request.
func decodeSignInBody(w http.ResponseWriter, r *http.Request, shape any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, signInMaxBody)
	if err := json.NewDecoder(r.Body).Decode(shape); err != nil {
		_ = wire.WriteError(w, http.StatusBadRequest, wire.CodeInvalidRequest, "this request carries no usable body", nil) // a failed write cannot replace the refusal
		return false
	}
	return true
}

// writeSignInJSON answers with one named payload. Every sign-in answer
// travels no-store, so a shared cache never keeps it.
func writeSignInJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload) // a failed client write has nowhere to report
}

// signInSession returns the user and session the Middleware resolved for
// this request. It answers 500 when no middleware ran, which never
// happens behind the documented mount.
func signInSession(w http.ResponseWriter, r *http.Request) (User, string, bool) {
	user, ok := UserFromContext(r.Context())
	session, sok := SessionFromContext(r.Context())
	if !ok || !sok || user.ID == "" || session.ID == "" {
		_ = wire.WriteError(w, http.StatusInternalServerError, wire.CodeInternal, "the guest session is not wired", nil) // a failed write cannot replace the refusal
		return User{}, "", false
	}
	return user, session.ID, true
}

// signInRetryDetail carries the retry hint a 429 refusal reports. The
// shared error writer reads retry_after_seconds into the Retry-After
// header, rounding up with a floor of one second.
type signInRetryDetail struct {
	// RetryAfterSeconds is the wait until a retry may succeed.
	RetryAfterSeconds float64 `json:"retry_after_seconds"`
}

// refuseSignInCode answers a refused code request with 429 and the wait
// until a retry may succeed. Known and unknown addresses share this one
// shape, so neither learns who registered. A zero wait answers a one
// second hint, so a limiter fault refuses without naming its half.
func refuseSignInCode(w http.ResponseWriter, wait time.Duration) {
	_ = wire.WriteError(w, http.StatusTooManyRequests, wire.CodeSendLimited, // a failed write cannot replace the refusal
		"too many codes were requested, retry after the wait",
		signInRetryDetail{RetryAfterSeconds: wait.Seconds()})
}

// handleRequestCode answers the code route. It checks the send ceilings
// before it stores or mails anything, and answers the same 202 body
// whether or not the address holds an account. A refused address
// answers 429 with a Retry-After, known and unknown alike. A limiter
// fault refuses too, and sends nothing.
func (s *Service) handleRequestCode(w http.ResponseWriter, r *http.Request) {
	_, sessionID, ok := signInSession(w, r)
	if !ok {
		return
	}
	if s.limiter == nil {
		_ = wire.WriteError(w, http.StatusInternalServerError, wire.CodeInternal, "the sign-in code route is not wired", nil) // a failed write cannot replace the refusal
		return
	}
	var body signInRequestJSON
	if !decodeSignInBody(w, r, &body) {
		return
	}
	address := normalizeAddress(body.Address)
	if address == "" {
		_ = wire.WriteError(w, http.StatusBadRequest, wire.CodeInvalidRequest, "this request names no address", nil) // a failed write cannot replace the refusal
		return
	}
	wait, allow, err := s.limiter.Allow(r.Context(), s.hashValue(address), r)
	if err != nil || !allow {
		if err != nil {
			s.log.Warn("identity: consult send ceilings", "error", err)
		}
		refuseSignInCode(w, wait)
		return
	}
	if err := s.RequestSignInCode(r.Context(), sessionID, body.Address); err != nil {
		switch {
		case errors.Is(err, ErrInvalid):
			_ = wire.WriteError(w, http.StatusBadRequest, wire.CodeInvalidRequest, "this request names no address", nil) // a failed write cannot replace the refusal
		case errors.Is(err, ErrSendLimited):
			refuseSignInCode(w, 0) // the wait is unknown at the reservation, so the floor applies
		default:
			s.log.Warn("identity: request sign-in code", "error", err)
			_ = wire.WriteError(w, http.StatusInternalServerError, wire.CodeInternal, "the sign-in code could not be sent", nil) // a failed write cannot replace the refusal
		}
		return
	}
	writeSignInJSON(w, http.StatusAccepted, signInOKJSON{OK: true})
}

// handleVerifyCode answers the verify route. A refused code answers 401
// without saying which guard tripped. A conflict answers 409 and changes
// nothing. Any success rotates the session and answers 200.
func (s *Service) handleVerifyCode(w http.ResponseWriter, r *http.Request) {
	user, sessionID, ok := signInSession(w, r)
	if !ok {
		return
	}
	var body signInVerifyJSON
	if !decodeSignInBody(w, r, &body) {
		return
	}
	outcome, err := s.VerifySignInCode(r.Context(), sessionID, user.ID, body.Address, body.Code, body.Choice)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalid):
			_ = wire.WriteError(w, http.StatusBadRequest, wire.CodeInvalidRequest, "this request carries no usable code", nil) // a failed write cannot replace the refusal
		case errors.Is(err, ErrInvalidCode):
			_ = wire.WriteError(w, http.StatusUnauthorized, wire.CodeInvalidCode, "this code is expired, used, or never requested on this device", nil) // a failed write cannot replace the refusal
		case errors.Is(err, ErrGuestDataConflict):
			_ = wire.WriteError(w, http.StatusConflict, wire.CodeGuestDataConflict, "this device holds guest data the account would leave behind", nil) // a failed write cannot replace the refusal
		default:
			s.log.Warn("identity: verify sign-in code", "error", err)
			_ = wire.WriteError(w, http.StatusInternalServerError, wire.CodeInternal, "the sign-in could not complete", nil) // a failed write cannot replace the refusal
		}
		return
	}
	s.SetSessionCookie(w, outcome.SessionID)
	writeSignInJSON(w, http.StatusOK, signInOKJSON{OK: true})
}
