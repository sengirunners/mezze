package orchapi

// OriginChecked is the query parameter (value "1") a member daemon puts on
// the calls only a person makes for themselves (a signed ask answer, a key
// sign-in, removing a key) once it has checked that the caller on its socket
// runs inside no registered agent session (SPEC "Key approvers"). The
// authority reads it on a member's forwarded call: a member too old to add it
// is refused there. It is a query parameter because the team proxy forwards a
// call's query as is but only a fixed set of its headers. A socket caller's
// own value is always dropped. It is a marker, not a boundary: whoever holds
// the member's token can send it.
const OriginChecked = "origin_checked"
