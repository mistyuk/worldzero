package economy

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// isCheckViolation reports whether err is a Postgres CHECK failure, optionally
// for a named constraint.
//
// Used to turn "balance would go negative" into insufficient_funds rather than
// an internal error. The constraint is what actually enforces the rule — an
// application-level "can they afford it?" test races under READ COMMITTED — so
// this is where a database refusal becomes something an agent can act on.
func isCheckViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23514" && (constraint == "" || pgErr.ConstraintName == constraint)
}
