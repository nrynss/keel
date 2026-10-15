// Package revision keeps every version of a document an editor works on.
//
// A document is opaque JSON bytes with an owner, such as a timeline, a script
// or a cue list. Each commit becomes a numbered revision. Numbers start at 1
// and rise by one per commit, so a document's head is its highest number.
//
// A commit names the revision it was made on, its parent. It succeeds only
// when the parent is still the head, so a stale tab cannot overwrite a newer
// commit. A refusal is a *ConflictError that carries the head, so the client
// can rebase its edit and try again. WriteConflict answers it over HTTP in
// the wire envelope with the stable code CodeConflict.
//
// An automated step never commits. It proposes, and a proposal is stored
// beside the history without being a revision. Accept turns an open proposal
// into a commit under the same conflict rule, and Reject closes it. Nothing
// proposed applies until a person accepts it.
//
// A pin names a revision as an approved take. Prune drops old revisions to
// bound a history, and it never drops a pinned revision or the head. Erasure
// is the one path that removes a pinned revision: ForgetOwner removes every
// document of an owner, and an erase.Target maps its unknown-owner answer to
// erase.ErrGone.
//
// An application separates its users by owner. Documents lists one owner's
// documents, and ForgetOwner erases them; the store keeps no other namespace.
// History, Proposals and Documents return one page at a time, keyset paged so
// a commit or a prune between pages never repeats a row. A Page or OwnerPage
// carries the limit, DefaultLimit when unset and never above MaxLimit, and
// the cursor the previous page returned.
//
// The store does not authorize. A method that names a document or a
// proposal id acts on it whoever asks, so a handler checks Document.Owner,
// or the Document field of a proposal, against the caller first.
//
// Merging two edits, diffing bodies and real-time collaboration are out of
// scope. The sqlitestore package provides the store over SQLite.
package revision

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/nrynss/keel/wire"
)

// CodeConflict is the wire envelope code a refused commit or accept answers
// with. A browser client branches on it to rebase onto the head the detail
// names.
const CodeConflict = "revision_conflict"

// Sentinel errors. Every failure the store reports wraps one of these, or a
// *ConflictError, so callers branch with errors.Is and errors.As.
var (
	// ErrInvalid reports unusable input, such as an empty document id, an
	// empty author, or a body that is not valid JSON.
	ErrInvalid = errors.New("revision: invalid input")

	// ErrExists reports a Create for a document id the store already holds.
	ErrExists = errors.New("revision: document already exists")

	// ErrUnknownDocument reports a document id the store holds no record
	// for. ForgetOwner reports it for an owner with no documents.
	ErrUnknownDocument = errors.New("revision: unknown document")

	// ErrUnknownRevision reports a revision number the document does not
	// hold, because it was never committed or it was pruned.
	ErrUnknownRevision = errors.New("revision: unknown revision")

	// ErrUnknownProposal reports a proposal id the store holds no record
	// for.
	ErrUnknownProposal = errors.New("revision: unknown proposal")

	// ErrClosed reports an Accept or Reject of a proposal that was already
	// accepted or rejected.
	ErrClosed = errors.New("revision: proposal already closed")

	// ErrPinTaken reports a Pin whose name already marks another revision
	// of the document. Unpin it first, so an approval never moves silently.
	ErrPinTaken = errors.New("revision: pin name marks another revision")

	// ErrUnknownPin reports an Unpin of a name the document holds no pin
	// under.
	ErrUnknownPin = errors.New("revision: unknown pin")

	// ErrConflict is matched by every *ConflictError.
	ErrConflict = errors.New("revision: parent is not the head")
)

// ConflictError reports a commit or accept whose parent is not the head. Head
// is the revision the client must rebase onto. It matches ErrConflict.
type ConflictError struct {
	// Document is the document the commit was for.
	Document string
	// Parent is the revision the refused commit was made on.
	Parent int64
	// Head is the document's current head.
	Head int64
}

// Error describes the conflict.
func (e *ConflictError) Error() string {
	return fmt.Sprintf("revision: document %s: parent %d is not the head %d", e.Document, e.Parent, e.Head)
}

// Is reports whether target is ErrConflict.
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// conflictDetail is the detail object WriteConflict sends.
type conflictDetail struct {
	Document string `json:"document"`
	Parent   int64  `json:"parent"`
	Head     int64  `json:"head"`
}

// WriteConflict answers a refused commit with a 409 in the wire envelope. The
// code is CodeConflict and the detail carries document, parent and head, so a
// browser client can fetch the head and rebase. It returns the error
// wire.WriteError returns. A nil c answers 500 with wire.CodeInternal, because
// a handler that lost its conflict has nothing to rebase onto.
func WriteConflict(w http.ResponseWriter, c *ConflictError) error {
	if c == nil {
		return wire.WriteError(w, http.StatusInternalServerError, wire.CodeInternal, "the conflict could not be reported", nil)
	}
	return wire.WriteError(w, http.StatusConflict, CodeConflict,
		"the document changed since this edit was made",
		conflictDetail{Document: c.Document, Parent: c.Parent, Head: c.Head})
}

// DefaultLimit is the page size a listing uses when the page names none.
const DefaultLimit = 50

// MaxLimit is the largest page size a listing accepts. A larger Limit
// reports ErrInvalid rather than a silently shorter page.
const MaxLimit = 500

// Page selects one page of a listing numbered newest first, History and
// Proposals. Paging is keyset: the next page starts below the last number
// returned, so a commit or a prune between two pages never shifts a row onto
// two pages or skips one that still exists.
type Page struct {
	// Limit is the most rows the page holds. Zero means DefaultLimit. A
	// negative value or one above MaxLimit reports ErrInvalid.
	Limit int
	// Before returns only rows numbered below it. Zero starts at the
	// newest row. A negative value reports ErrInvalid. Pass the next
	// cursor a listing returned to read the page after it.
	Before int64
}

// OwnerPage selects one page of an owner's documents, ordered by id
// ascending. Paging is keyset on the id, so a document created or erased
// between two pages never repeats a row. A document created with an id
// before the cursor lands on an earlier page and is not seen.
type OwnerPage struct {
	// Limit is the most documents the page holds. Zero means DefaultLimit.
	// A negative value or one above MaxLimit reports ErrInvalid.
	Limit int
	// After returns only ids that sort after it. Empty starts at the
	// first id. Pass the next cursor Documents returned to read on.
	After string
}

// Document is one document's record.
type Document struct {
	// ID identifies the document.
	ID string
	// Owner is the account the document belongs to. Erasure keys on it.
	Owner string
	// Head is the highest revision number committed.
	Head int64
	// CreatedAt is when the first revision was committed.
	CreatedAt time.Time
}

// Revision is one committed version of a document.
type Revision struct {
	// Document is the document the revision belongs to.
	Document string
	// Number is the revision number, from 1.
	Number int64
	// Parent is the revision the commit was made on, or 0 for the first.
	Parent int64
	// Author is who committed it. For an accepted proposal it is the
	// person who accepted.
	Author string
	// Proposal is the id of the proposal accepted into this revision, or
	// empty for a direct commit.
	Proposal string
	// Body is the document's JSON at this revision. History leaves it nil,
	// and Get fills it.
	Body []byte
	// CommittedAt is when the revision was committed.
	CommittedAt time.Time
}

// Pin names one revision as an approved take.
type Pin struct {
	// Name is the pin's name, unique within the document.
	Name string
	// Revision is the pinned revision number.
	Revision int64
	// PinnedAt is when the pin was set.
	PinnedAt time.Time
}

// ProposalState names where one proposal stands.
type ProposalState string

// Proposal states. A proposal starts open and ends accepted or rejected.
const (
	// ProposalOpen marks a proposal no one has decided yet.
	ProposalOpen ProposalState = "open"
	// ProposalAccepted marks a proposal that became a revision.
	ProposalAccepted ProposalState = "accepted"
	// ProposalRejected marks a proposal that was closed without applying.
	ProposalRejected ProposalState = "rejected"
)

// Proposal is a suggested edit that is not a revision until accepted.
type Proposal struct {
	// ID identifies the proposal.
	ID string
	// Document is the document the proposal is for.
	Document string
	// Seq numbers the document's proposals from 1 in the order they were
	// made. A number is never reused, even after Prune drops a proposal,
	// and Proposals pages on it.
	Seq int64
	// Parent is the revision the proposal was made on.
	Parent int64
	// Author is who proposed, such as an automated step.
	Author string
	// Body is the proposed JSON.
	Body []byte
	// State is where the proposal stands.
	State ProposalState
	// Revision is the revision an accept committed, or 0.
	Revision int64
	// DecidedBy is who accepted or rejected it, or empty while open.
	DecidedBy string
	// CreatedAt is when it was proposed.
	CreatedAt time.Time
	// DecidedAt is when it was accepted or rejected, or the zero time.
	DecidedAt time.Time
}

// Store keeps documents, their revisions, pins and proposals. The
// sqlitestore package provides one over SQLite. A cancelled context stops a
// write before it commits, so nothing changes.
type Store interface {
	// Create stores a new document owned by owner with body as revision 1.
	Create(ctx context.Context, doc, owner string, body []byte, author string) (Revision, error)
	// Document reads the document's record.
	Document(ctx context.Context, doc string) (Document, error)
	// Documents lists one page of owner's documents by id, and the After
	// cursor of the next page, or "" when this page is the last. This is
	// how an application separates its users: every document has one
	// owner, and the store keeps no namespace beyond it.
	Documents(ctx context.Context, owner string, page OwnerPage) ([]Document, string, error)
	// Commit stores body as the next revision when parent is the head, and
	// otherwise refuses with a *ConflictError carrying the head.
	Commit(ctx context.Context, doc string, parent int64, body []byte, author string) (Revision, error)
	// History lists one page of the document's revisions newest first,
	// without bodies, and the Before cursor of the next page, or 0 when
	// this page is the last.
	History(ctx context.Context, doc string, page Page) ([]Revision, int64, error)
	// Get reads one revision with its body.
	Get(ctx context.Context, doc string, rev int64) (Revision, error)
	// Pin marks rev as an approved take under name.
	Pin(ctx context.Context, doc string, rev int64, name string) (Pin, error)
	// Unpin removes the pin name.
	Unpin(ctx context.Context, doc, name string) error
	// Pins lists the document's pins by name. It is not paged, because a
	// pin is a person's approval and a document holds a handful.
	Pins(ctx context.Context, doc string) ([]Pin, error)
	// Prune drops revisions older than the newest keep, sparing every
	// pinned revision and the head, and reports how many it dropped.
	// Decided proposals made on a dropped revision go with it.
	Prune(ctx context.Context, doc string, keep int) (int, error)
	// Propose stores a proposal made on parent.
	Propose(ctx context.Context, doc string, parent int64, body []byte, author string) (Proposal, error)
	// Proposal reads one proposal.
	Proposal(ctx context.Context, id string) (Proposal, error)
	// Proposals lists one page of the document's proposals newest first,
	// and the Before cursor of the next page, or 0 when this page is the
	// last.
	Proposals(ctx context.Context, doc string, page Page) ([]Proposal, int64, error)
	// Accept commits an open proposal under the commit conflict rule.
	Accept(ctx context.Context, id, author string) (Revision, error)
	// Reject closes an open proposal without applying it.
	Reject(ctx context.Context, id, author string) (Proposal, error)
	// ForgetOwner removes every document of owner with all its rows.
	ForgetOwner(ctx context.Context, owner string) error
}
