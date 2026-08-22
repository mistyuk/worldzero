package werr

// All is every code the world can return, in one place.
//
// It exists so that anything which must handle every code — the HTTP status
// table, the SDK, the documentation — can be checked against reality by a test
// instead of by whoever remembers to look. Adding a code here and nowhere else
// should fail a build somewhere; that is the point.
var All = []Code{
	InsufficientFunds,
	NotFound,
	Forbidden,
	InvalidParams,
	CooldownActive,
	CapacityFull,
	Incapacitated,
	RateLimited,
	IdempotencyConflict,
	NameTaken,
	Unauthenticated,
	InsufficientScope,
	IdempotencyInProgress,
	Busy,
	Internal,
}
