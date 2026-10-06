package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/nrynss/keel/erase"
	"github.com/nrynss/keel/job"
	"github.com/nrynss/keel/wire"
)

// Account deletion for signed-in users.
//
// Deleting needs a fresh sign-in code, typed while it is still live. The
// code check reuses the sign-in verification, so one rule decides every
// code, and the verification consumes the code it checked. The job then
// fans the erasure out over the app-registered targets and removes the
// identity rows, the code rows, every session row and the user row last.
// No stored address survives.
//
// The work runs as one job under DeletionKindName, so a long erasure
// outlasts its request and a restart resumes what is left. Register the
// kinds Kinds returns before the runner opens, because a kind that
// declares no resume work never resumes after a restart.

// DeletionKindName is the job kind account deletion runs under. Register
// the kinds Kinds returns before the runner opens, so a restart resumes
// an interrupted deletion from its snapshot.
const DeletionKindName = "identity-delete"

// deletionMaxAttempts is the tries one deletion may make across
// restarts. Every step repeats safely, so every restart may resume the
// work.
const deletionMaxAttempts = 5

// deletionSnapshotVersion guards the snapshot encoding. Only version one
// exists.
const deletionSnapshotVersion = 1

// deletionWatchTimeout bounds the wait on one inner erasure job, and
// deletionWatchTick spaces the polls.
const (
	deletionWatchTimeout = time.Minute
	deletionWatchTick    = 20 * time.Millisecond
)

// deletionMaxBody caps the deletion request body.
const deletionMaxBody = 1 << 20

// deletionSnapshot is the ledger one deletion progress report carries. A
// run that resumes reads it from the interrupted record, so the same
// user deletes again and already-erased targets confirm as gone.
type deletionSnapshot struct {
	// Version guards the encoding. Only version one exists.
	Version int `json:"version"`
	// User is the deleted user id. Every delete scopes to it.
	User string `json:"user"`
	// RowsDone marks a user with no code, session, identity or user row
	// left.
	RowsDone bool `json:"rows_done,omitempty"`
}

// BindRunner binds the runner the deletion jobs run on. The app calls it
// once after job.Open, because recovery schedules a resumed deletion
// while the runner opens, ahead of this call. A resumed deletion waits
// for the bind, and a boot that never binds ends the wait through the
// job's context. A later bind replaces an earlier runner, which is how a
// test rebinds a reopened runner.
func (s *Service) BindRunner(r *job.Runner) error {
	if r == nil {
		return fmt.Errorf("%w: runner must not be nil", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runner = r
	select {
	case <-s.bound:
		// The gate is already open from an earlier bind.
	default:
		close(s.bound)
	}
	return nil
}

// Kinds returns every job kind account deletion needs: the deletion kind
// under DeletionKindName and the inner erasure kind under its own name.
// Pass the result into the runner config, because an erasure kind that
// declares no resume work never resumes after a restart. An
// unconfigured deletion returns an empty map.
func (s *Service) Kinds() map[string]job.Kind {
	kinds := make(map[string]job.Kind, 2)
	if s.eraser != nil {
		kinds[DeletionKindName] = job.Kind{
			Idempotent:  true,
			MaxAttempts: deletionMaxAttempts,
			Resume:      s.resumeDeletion,
		}
		kinds[erase.KindName] = s.eraser.Kind()
	}
	return kinds
}

// Delete starts the deletion of userID and returns the job id at once.
// The caller follows the job to done. It first proves the address names
// an identity on this user, then consumes one fresh code through the
// sign-in verification, so a refusal deletes nothing and a refused code
// burns nothing. The job erases every app-registered target, then
// removes the code rows, the session rows, the identity rows and the
// user row last.
func (s *Service) Delete(ctx context.Context, sessionID, userID, address, code string) (string, error) {
	if s.eraser == nil || s.targets == nil {
		return "", fmt.Errorf("%w: deletion needs an eraser and a target source", ErrInvalid)
	}
	runner := s.currentRunner()
	if runner == nil {
		return "", ErrNoRunner
	}
	if sessionID == "" || userID == "" || normalizeAddress(address) == "" || code == "" {
		return "", fmt.Errorf("%w: deletion needs a session, a user, an address and a code", ErrInvalid)
	}
	clean := normalizeAddress(address)
	holder, err := s.store.IdentityHolder(ctx, providerEmail, clean)
	if errors.Is(err, ErrUnknownIdentity) {
		return "", fmt.Errorf("identity: delete account for %s: %w", userID, ErrNotOwner)
	}
	if err != nil {
		return "", fmt.Errorf("identity: delete account for %s: read identity: %w", userID, err)
	}
	if holder != userID {
		return "", fmt.Errorf("identity: delete account for %s: %w", userID, ErrNotOwner)
	}
	// The verification consumes the code it checked, so the same code
	// never authorizes a second deletion. A conflict cannot arise here,
	// because the address is already this user's, and one that somehow
	// did still refuses the deletion.
	outcome, err := s.VerifySignInCode(ctx, sessionID, userID, clean, code, "")
	if err != nil {
		if errors.Is(err, ErrGuestDataConflict) || errors.Is(err, ErrInvalidCode) {
			return "", fmt.Errorf("identity: delete account for %s: %w", userID, ErrInvalidCode)
		}
		return "", fmt.Errorf("identity: delete account for %s: %w", userID, err)
	}
	if outcome.UserID != userID {
		return "", fmt.Errorf("identity: delete account for %s: %w: the sign-in moved devices", userID, ErrInvalidCode)
	}
	snap := deletionSnapshot{Version: deletionSnapshotVersion, User: userID}
	jobID, err := runner.StartKind(ctx, DeletionKindName, func(ctx context.Context, progress func(job.Progress)) ([]byte, error) {
		return s.runDeletion(ctx, progress, snap)
	})
	if err != nil {
		return "", fmt.Errorf("identity: delete account for %s: %w", userID, err)
	}
	return jobID, nil
}

// currentRunner returns the bound runner, or nil while none is bound.
func (s *Service) currentRunner() *job.Runner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runner
}

// resumeDeletion rebuilds the work of a deletion a restart left
// unfinished. It is the job kind resume hook. A record with no snapshot
// fails, because nothing recorded names the deleted user.
func (s *Service) resumeDeletion(rec job.Record) (job.Func, error) {
	snap := deletionSnapshot{}
	if len(rec.Progress.Detail) == 0 {
		return nil, fmt.Errorf("identity: resume deletion %s: %w: unknown snapshot", rec.ID, ErrInvalid)
	}
	if err := json.Unmarshal(rec.Progress.Detail, &snap); err != nil {
		return nil, fmt.Errorf("identity: resume deletion %s: %w", rec.ID, err)
	}
	if snap.Version != deletionSnapshotVersion || snap.User == "" {
		return nil, fmt.Errorf("identity: resume deletion %s: %w: unknown snapshot", rec.ID, ErrInvalid)
	}
	return func(ctx context.Context, progress func(job.Progress)) ([]byte, error) {
		// Recovery schedules the resume while the runner opens, ahead of
		// the app's BindRunner call. The gate holds the work until the
		// bind lands, and a boot that never binds ends the wait through
		// the job's context.
		select {
		case <-s.bound:
		case <-ctx.Done():
			return nil, fmt.Errorf("identity: resume deletion %s: %w", rec.ID, ctx.Err())
		}
		return s.runDeletion(ctx, progress, snap)
	}, nil
}

// runDeletion drives one deletion attempt. Snap names the user an
// earlier step verified. Every step repeats safely, so a resumed
// deletion repeats the erases that never confirmed and the rows that are
// already gone.
func (s *Service) runDeletion(ctx context.Context, progress func(job.Progress), snap deletionSnapshot) ([]byte, error) {
	s.publishDeletion(progress, snap)
	runner := s.currentRunner()
	if runner == nil {
		return nil, fmt.Errorf("identity: delete account for %s: %w", snap.User, ErrNoRunner)
	}
	targets, err := s.targets(ctx, snap.User)
	if err != nil {
		return nil, fmt.Errorf("identity: delete account for %s: list targets: %w", snap.User, err)
	}
	if len(targets) > 0 {
		// The user id is the erasure ref, so the eraser's Source rebuilds
		// the same list when a restart resumes the inner erasure on its
		// own.
		erasureID, err := s.eraser.Start(ctx, runner, snap.User, targets)
		if err != nil {
			return nil, fmt.Errorf("identity: delete account for %s: start erasure: %w", snap.User, err)
		}
		s.publishDeletion(progress, snap)
		if err := s.waitDeletionErasure(ctx, runner, erasureID); err != nil {
			return nil, err
		}
		rep, err := s.eraser.Inspect(ctx, runner, erasureID)
		if err != nil {
			return nil, fmt.Errorf("identity: delete account for %s: inspect erasure: %w", snap.User, err)
		}
		if !rep.Complete() {
			return nil, fmt.Errorf("identity: delete account for %s: erasure still owes %v", snap.User, rep.Stuck)
		}
	}
	if err := s.store.DeleteUserRows(ctx, snap.User); err != nil {
		return nil, fmt.Errorf("identity: delete account for %s: delete rows: %w", snap.User, err)
	}
	snap.RowsDone = true
	s.publishDeletion(progress, snap)
	return nil, nil
}

// waitDeletionErasure polls one inner erasure until its job lands. Done
// means the erasure confirmed every target. Any other terminal means the
// targets still owe deletes, so the deletion fails and a retry resumes
// from its snapshot.
func (s *Service) waitDeletionErasure(ctx context.Context, runner *job.Runner, erasureID string) error {
	deadline := s.now().Add(deletionWatchTimeout)
	for {
		attempts, err := runner.Attempts(ctx, erasureID)
		if err != nil {
			return fmt.Errorf("identity: delete account: watch erasure: %w", err)
		}
		for _, rec := range attempts {
			switch rec.Status {
			case job.StatusDone:
				return nil
			case job.StatusError, job.StatusCancelled, job.StatusInterrupted:
				return fmt.Errorf("identity: delete account: erasure %s ended %s", erasureID, rec.Status)
			}
		}
		if !s.now().Before(deadline) {
			return fmt.Errorf("identity: delete account: erasure %s never landed", erasureID)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("identity: delete account: watch erasure: %w", ctx.Err())
		case <-time.After(deletionWatchTick):
		}
	}
}

// publishDeletion records one deletion ledger snapshot through the job
// progress channel, so the runner streams it and leaves it durable in
// the record.
func (s *Service) publishDeletion(progress func(job.Progress), snap deletionSnapshot) {
	detail, err := json.Marshal(snap)
	if err != nil {
		// The snapshot holds only a number, two strings and a bool, so
		// Marshal cannot fail. A snapshot that cannot encode is dropped,
		// and the next one carries the ledger again.
		return
	}
	progress(job.Progress{Stage: "delete", Detail: detail})
}

// deletionRequestJSON carries the fresh code that authorizes a
// deletion. The code must be live and requested by this same session.
type deletionRequestJSON struct {
	// Address is the address the code went to, before normalization.
	Address string `json:"address"`
	// Code is the typed six digit value.
	Code string `json:"code"`
}

// deletionResponse is the deletion body. JobID is the deletion job the
// caller follows to done over the job stream.
type deletionResponse struct {
	// JobID is the deletion job id.
	JobID string `json:"job_id"`
}

// DeleteHandler serves account deletion on one handler. Mount it behind
// the Middleware at a POST path of the app's choosing. The handler starts
// the deletion and answers with the job id, so the request never waits
// on the erasure. A refused code answers 401 without saying which guard
// tripped. A stranger's address answers the same 404 as an unknown
// account.
func (s *Service) DeleteHandler() http.Handler {
	return http.HandlerFunc(s.handleDelete)
}

// handleDelete answers POST by starting the deletion and returning the
// job id. The deletion revokes every session the user holds, so the
// answer drops the session cookie and the next request mints a fresh
// guest.
func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	user, sessionID, ok := signInSession(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, deletionMaxBody)
	var body deletionRequestJSON
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		_ = wire.WriteError(w, http.StatusBadRequest, wire.CodeInvalidRequest, "this request carries no usable code", nil) // a failed write cannot replace the refusal
		return
	}
	if normalizeAddress(body.Address) == "" || body.Code == "" {
		_ = wire.WriteError(w, http.StatusBadRequest, wire.CodeInvalidRequest, "this request carries no usable code", nil) // a failed write cannot replace the refusal
		return
	}
	jobID, err := s.Delete(r.Context(), sessionID, user.ID, body.Address, body.Code)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidCode):
			_ = wire.WriteError(w, http.StatusUnauthorized, wire.CodeInvalidCode, "this code is expired, used, or never requested on this device", nil) // a failed write cannot replace the refusal
		case errors.Is(err, ErrNotOwner):
			_ = wire.WriteError(w, http.StatusNotFound, wire.CodeNotFound, "that account opens nothing", nil) // a failed write cannot replace the refusal
		case errors.Is(err, ErrNoRunner):
			s.log.Warn("identity: delete account without a bound runner", "error", err)
			_ = wire.WriteError(w, http.StatusInternalServerError, wire.CodeInternal, "the deletion is not wired", nil) // a failed write cannot replace the refusal
		default:
			s.log.Warn("identity: delete account", "error", err)
			_ = wire.WriteError(w, http.StatusInternalServerError, wire.CodeInternal, "that request could not finish", nil) // a failed write cannot replace the refusal
		}
		return
	}
	s.clearSessionCookie(w)
	writeSignInJSON(w, http.StatusAccepted, deletionResponse{JobID: jobID})
}
