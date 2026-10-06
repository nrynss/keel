package fetch

import "fmt"

// Codes carried by Error, one per distinct refusal or failure a fetch can
// end with. An application handler that answers HTTP copies the code of the
// error into its own error envelope, so a client branches on one stable
// identifier instead of on a sentence.
const (
	// CodeBlockedAddress marks a fetch whose dialled address the policy
	// refused. The address is the one resolution produced, never the name
	// the link carried.
	CodeBlockedAddress = "fetch_blocked_address"
	// CodeBadScheme marks a link whose scheme the policy refuses. Only
	// https is allowed by default, and http beside it when Config.AllowHTTP
	// is set.
	CodeBadScheme = "fetch_bad_scheme"
	// CodeBadPort marks a link whose port the policy refuses.
	CodeBadPort = "fetch_bad_port"
	// CodeBadURL marks a link that does not parse, names no host, or
	// carries a port that is not a number.
	CodeBadURL = "fetch_bad_url"
	// CodeTooManyRedirects marks a fetch that followed more redirects than
	// Config.MaxRedirects allows.
	CodeTooManyRedirects = "fetch_too_many_redirects"
	// CodeTooLarge marks a body that exceeds Config.MaxBytes. The body is
	// refused whole, never truncated.
	CodeTooLarge = "fetch_too_large"
	// CodeBadType marks a content type the allowlist refuses, in the
	// Content-Type header, in the sniffed bytes, or in both.
	CodeBadType = "fetch_bad_type"
	// CodeTimeout marks a fetch that outlived Config.Timeout or waited past
	// Config.HeaderTimeout for a response.
	CodeTimeout = "fetch_timeout"
	// CodeBadStatus marks a final response whose status is outside 2xx.
	CodeBadStatus = "fetch_bad_status"
	// CodeUnreachable marks a fetch that failed below the policy, such as a
	// name that does not resolve, a refused connection, or a TLS fault.
	CodeUnreachable = "fetch_unreachable"
)

// Error is a fetch that was refused or failed, carrying the stable code an
// application maps into its error envelope. Get returns one for every
// condition the package itself names, wrapped or not, and an application
// reaches it with errors.As.
type Error struct {
	// Code is one of the Code constants of this package.
	Code string
	// Message is one lowercase sentence about the failure. A client may
	// show it, and must never branch on it.
	Message string
	// err is the cause below the refusal, if any. Unwrap exposes it.
	err error
}

// Error renders the code and the message as one line for a log.
func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap returns the cause the error carries, so errors.Is and errors.As
// reach past it into the transport's own wrappers. A refusal with no cause
// below it returns nil.
func (e *Error) Unwrap() error {
	return e.err
}

// refusal builds a policy refusal with no cause below it.
func refusal(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// failure builds a failure that wraps the cause the transport reported.
func failure(code, message string, err error) *Error {
	return &Error{Code: code, Message: message, err: err}
}
