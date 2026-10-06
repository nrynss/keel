package wire

// The sign-in and account refusal codes. A screen branches on the code
// and never on the message, so the wording around one code can change
// without a client release.

// CodeInvalidRequest answers a malformed body or a value the route
// cannot honour.
const CodeInvalidRequest = "invalid_request"

// CodeInvalidCode answers a sign-in or deletion code the server refuses.
// The code is unknown, requested by another session, expired, used, or
// past its attempts. One code covers every case, so the answer never
// names an account.
const CodeInvalidCode = "invalid_code"

// CodeGuestDataConflict answers a sign-in that would strand the guest's
// app data on this device. Nothing changes, and the caller repeats the
// request with the switch choice to move anyway.
const CodeGuestDataConflict = "guest_data_conflict"

// CodeSendLimited answers a code request the send ceilings refuse. Known
// and unknown addresses share it, so the answer never names an account.
const CodeSendLimited = "send_limited"

// CodeInternal answers a dependency fault.
const CodeInternal = "internal_error"

// CodeNotFound answers a resource the request may not know about, with
// the same shape as any other absence.
const CodeNotFound = "not_found"
