package identity

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nrynss/keel/id"
	"github.com/nrynss/keel/wire"
)

// Provider is one external sign-in method behind the shared flow. The
// service draws the state, the nonce and the PKCE verifier, pairs the
// start with the callback, and resolves the identity. The provider
// builds the redirect and proves the callback code. Any sign-in method
// that speaks the authorization code flow can implement it.
type Provider interface {
	// Name is the provider name the identity rows store. One name maps
	// to one sign-in method, and two providers never share one.
	Name() string

	// AuthorizeURL builds the redirect that begins one sign-in. The
	// returned URL must carry the state, so the provider returns it to
	// the callback that pairs with this start.
	AuthorizeURL(ctx context.Context, start StartChallenge) (string, error)

	// Exchange trades the callback code for the provider subject it
	// authorizes. The subject keys the identity, and it is the provider's
	// account id, never an address. The nonce is the value the provider
	// answer must echo, and the verifier is the PKCE secret the start
	// drew. Every refusal matches ErrProviderToken.
	Exchange(ctx context.Context, callback CallbackChallenge) (string, error)
}

// StartChallenge carries the values one start drew.
type StartChallenge struct {
	// State pairs the callback with this start.
	State string
	// Nonce is the value the provider answer must echo.
	Nonce string
	// Verifier is the PKCE secret the exchange must prove.
	Verifier string
}

// CallbackChallenge carries the callback answer back to the provider.
type CallbackChallenge struct {
	// Code is the authorization code the callback carried.
	Code string
	// Nonce is the value the provider answer must echo.
	Nonce string
	// Verifier is the PKCE secret the exchange must prove.
	Verifier string
}

// pendingLife bounds one provider start, the same life a mailed code
// carries.
const pendingLife = 10 * time.Minute

// mintRandom draws n random bytes as unpadded base64url. State, nonce
// and verifier all read this form.
func mintRandom(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("identity: mint sign-in secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// rememberStart stores one start and sweeps expired rows first, so
// abandoned starts never accumulate past their life.
func (s *Service) rememberStart(state string, pending pendingStart) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, row := range s.pending {
		if !row.expires.After(now) {
			delete(s.pending, key)
		}
	}
	s.pending[state] = pending
}

// lookupStart returns the live start for one state. An unknown or
// expired state reports false, and an expired row leaves the map.
func (s *Service) lookupStart(state string) (pendingStart, bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, ok := s.pending[state]
	if !ok {
		return pendingStart{}, false
	}
	if !pending.expires.After(now) {
		delete(s.pending, state)
		return pendingStart{}, false
	}
	return pending, true
}

// forgetStart drops one start after its success. A replayed callback
// then meets an unknown state instead of a second sign-in.
func (s *Service) forgetStart(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, state)
}

// revisitStart keeps one start after a conflict, carrying the validated
// subject. The switch retry reads it back without a code, because the
// provider code is single use and already spent.
func (s *Service) revisitStart(state string, pending pendingStart) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending[state]; ok {
		s.pending[state] = pending
	}
}

// StartSignIn begins one provider sign-in for a session. It draws the
// state, the nonce and the verifier, asks the provider for the redirect,
// and keeps the secret values beside the session, so the callback checks
// them without trusting the client. The caller sends the visitor to the
// returned URL.
func (s *Service) StartSignIn(ctx context.Context, p Provider, sessionID, userID string) (string, error) {
	if p == nil {
		return "", fmt.Errorf("%w: provider must not be nil", ErrInvalid)
	}
	if sessionID == "" || userID == "" {
		return "", fmt.Errorf("%w: session and user are required", ErrInvalid)
	}
	state, err := mintRandom(16)
	if err != nil {
		return "", err
	}
	nonce, err := mintRandom(16)
	if err != nil {
		return "", err
	}
	verifier, err := mintRandom(32)
	if err != nil {
		return "", err
	}
	redirect, err := p.AuthorizeURL(ctx, StartChallenge{State: state, Nonce: nonce, Verifier: verifier})
	if err != nil {
		return "", err
	}
	s.rememberStart(state, pendingStart{
		nonce:     nonce,
		verifier:  verifier,
		sessionID: sessionID,
		userID:    userID,
		expires:   s.now().Add(pendingLife),
	})
	return redirect, nil
}

// CompleteSignIn finishes one provider sign-in. It checks the state
// against the start, exchanges the code once, and resolves the identity
// the same way the code sign-in does: attach on first use, keep on the
// same user, switch a guest with no data, and conflict on guest data
// without the switch choice. A conflict keeps the start, so the retry
// carries the state with the switch choice and no code.
func (s *Service) CompleteSignIn(ctx context.Context, p Provider, sessionID, userID, state, code, choice string) (SignInOutcome, error) {
	if p == nil {
		return SignInOutcome{}, fmt.Errorf("%w: provider must not be nil", ErrInvalid)
	}
	if choice != "" && choice != choiceSwitch {
		return SignInOutcome{}, fmt.Errorf("%w: unknown choice", ErrInvalid)
	}
	pending, ok := s.lookupStart(state)
	if !ok {
		return SignInOutcome{}, ErrProviderState
	}
	if pending.sessionID != sessionID || pending.userID != userID || sessionID == "" || userID == "" {
		return SignInOutcome{}, ErrProviderState
	}
	if !pending.validated {
		if strings.TrimSpace(code) == "" {
			return SignInOutcome{}, ErrProviderState
		}
		subject, err := p.Exchange(ctx, CallbackChallenge{Code: code, Nonce: pending.nonce, Verifier: pending.verifier})
		if err != nil {
			return SignInOutcome{}, err
		}
		pending.validated = true
		pending.subject = subject
	}
	outcome, err := s.resolveProviderSignIn(ctx, sessionID, userID, p.Name(), pending.subject, choice)
	if err != nil {
		if errors.Is(err, ErrGuestDataConflict) {
			s.revisitStart(state, pending)
			return SignInOutcome{}, ErrGuestDataConflict
		}
		return SignInOutcome{}, err
	}
	s.forgetStart(state)
	return outcome, nil
}

// resolveProviderSignIn attaches or joins one provider identity and
// rotates the session, the same path the code sign-in takes. A missing
// identity attaches to the current user and flips its kind to owner. An
// identity on this user changes nothing. An identity on another user
// switches a guest with no data and conflicts on guest data without the
// switch choice. A racing attach heals inside the store transaction
// exactly like the code flow, by joining the holder the race revealed.
func (s *Service) resolveProviderSignIn(ctx context.Context, sessionID, userID, provider, subject, choice string) (SignInOutcome, error) {
	if provider == "" || subject == "" {
		return SignInOutcome{}, fmt.Errorf("%w: provider and subject are required", ErrInvalid)
	}
	holder, err := s.store.IdentityHolder(ctx, provider, subject)
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
		Provider:     provider,
		Subject:      subject,
		CodeID:       "",
		At:           s.now(),
		GuestData:    ownsData,
		AllowSwitch:  choice == choiceSwitch,
	})
	if err != nil {
		if errors.Is(err, ErrGuestDataConflict) {
			return SignInOutcome{}, err
		}
		return SignInOutcome{}, fmt.Errorf("identity: resolve sign-in: %w", err)
	}
	return SignInOutcome(target), nil
}

// ProviderSignInHandler serves one provider's start and callback pair on
// one handler. Mount it behind the Middleware, so every request carries
// a user and a session, at the two GET paths the caller names. The start
// answers with a redirect to the provider. The callback sets the rotated
// session cookie and answers the wire envelope, so screens branch on
// codes and never on wording.
func (s *Service) ProviderSignInHandler(startPath, callbackPath string, p Provider) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+startPath, func(w http.ResponseWriter, r *http.Request) {
		s.handleProviderStart(w, r, p)
	})
	mux.HandleFunc("GET "+callbackPath, func(w http.ResponseWriter, r *http.Request) {
		s.handleProviderCallback(w, r, p)
	})
	return mux
}

// handleProviderStart answers the start route with the provider
// redirect. A fault answers 500, so a miswired provider surfaces instead
// of hiding.
func (s *Service) handleProviderStart(w http.ResponseWriter, r *http.Request, p Provider) {
	user, sessionID, ok := signInSession(w, r)
	if !ok {
		return
	}
	redirect, err := s.StartSignIn(r.Context(), p, sessionID, user.ID)
	if err != nil {
		s.log.Warn("identity: start provider sign-in", "error", err)
		_ = wire.WriteError(w, http.StatusInternalServerError, wire.CodeInternal, "the provider sign-in is not ready", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// handleProviderCallback answers the provider return. A success sets the
// rotated session cookie and answers ok. A conflict answers 409 and
// changes nothing, so the retry carries the state with the switch
// choice. Any other refusal answers 401 without saying which guard
// tripped, and every answer travels no-store.
func (s *Service) handleProviderCallback(w http.ResponseWriter, r *http.Request, p Provider) {
	user, sessionID, ok := signInSession(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	if query.Get("error") != "" {
		_ = wire.WriteError(w, http.StatusUnauthorized, wire.CodeInvalidCode, "the provider sign-in was refused", nil)
		return
	}
	outcome, err := s.CompleteSignIn(r.Context(), p, sessionID, user.ID,
		query.Get("state"), query.Get("code"), query.Get("choice"))
	if err != nil {
		switch {
		case errors.Is(err, ErrGuestDataConflict):
			_ = wire.WriteError(w, http.StatusConflict, wire.CodeGuestDataConflict, "this device holds guest data the account would leave behind", nil)
		case errors.Is(err, ErrInvalid):
			_ = wire.WriteError(w, http.StatusBadRequest, wire.CodeInvalidRequest, "this callback carries no usable answer", nil)
		default:
			s.log.Warn("identity: complete provider sign-in", "error", err)
			_ = wire.WriteError(w, http.StatusUnauthorized, wire.CodeInvalidCode, "the provider sign-in was refused", nil)
		}
		return
	}
	s.SetSessionCookie(w, outcome.SessionID)
	writeSignInJSON(w, http.StatusOK, signInOKJSON{OK: true})
}
