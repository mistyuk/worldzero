// Package users owns human accounts.
//
// Humans do not play (VISION §1). A user account exists to own citizens, hold
// credentials, and log into a dashboard to watch — never to act in the world.
// That separation is enforced in internal/kernel/auth: a session or user key can
// never legally hold an agent scope, so a human is structurally incapable of
// acting as its own citizen rather than merely discouraged from it.
package users

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

type User struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// Password limits. The maximum exists because argon2 cost is bounded by
// MaxConcurrent but input length is not: a megabyte password is a free way to
// make one hash expensive.
const (
	MinPasswordLen = 12
	MaxPasswordLen = 256
	MaxEmailLen    = 254 // RFC 5321 practical maximum
)

type Service struct {
	clk    clock.Clock
	gen    *ids.Generator
	params Params
}

func NewService(clk clock.Clock, gen *ids.Generator) *Service {
	return &Service{clk: clk, gen: gen, params: Default}
}

type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Create registers a human account.
func (s *Service) Create(ctx context.Context, tx pgx.Tx, email, password string) (User, error) {
	addr, norm, err := normalizeEmail(email)
	if err != nil {
		return User{}, err
	}
	if err := checkPassword(password); err != nil {
		return User{}, err
	}

	hash, err := HashPassword(password, s.params)
	if err != nil {
		return User{}, werr.Wrap(werr.Internal, "could not create account", err)
	}

	now := s.clk.Real() // account lifecycle is real time (ADR-018)
	u := User{
		ID:        s.gen.New(ids.User),
		Email:     addr,
		Status:    StatusActive,
		CreatedAt: now,
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO users (id, email, email_norm, password_hash, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
	`, u.ID, u.Email, norm, hash, u.Status, now)
	if err != nil {
		if isUniqueViolation(err) {
			// Deliberately the same shape of answer as success would give the
			// caller nothing: this DOES leak that the address is taken, which is
			// unavoidable for a signup form that has to tell you to log in
			// instead. Login is where enumeration is actually prevented.
			return User{}, werr.New(werr.NameTaken, "an account with that email already exists")
		}
		return User{}, werr.Wrap(werr.Internal, "could not create account", err)
	}
	return u, nil
}

// Authenticate checks an email and password.
//
// Every failure — unknown address, wrong password, disabled account — returns
// one identical error AND costs the same time. The dummy verify is not
// decoration: argon2id takes tens of milliseconds, so skipping it on an unknown
// address makes the timing difference enormous and trivially measurable over a
// network. A slow hash makes an enumeration oracle louder, not quieter.
func (s *Service) Authenticate(ctx context.Context, q Querier, email, password string) (User, error) {
	fail := func() (User, error) {
		return User{}, werr.New(werr.Unauthenticated, "invalid email or password")
	}

	_, norm, err := normalizeEmail(email)
	if err != nil {
		DummyVerify(password)
		return fail()
	}
	if len(password) == 0 || len(password) > MaxPasswordLen {
		DummyVerify(password)
		return fail()
	}

	var (
		u    User
		hash *string
	)
	err = q.QueryRow(ctx, `
		SELECT id, email, status, created_at, password_hash
		FROM users WHERE email_norm = $1
	`, norm).Scan(&u.ID, &u.Email, &u.Status, &u.CreatedAt, &hash)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		DummyVerify(password)
		return fail()
	case err != nil:
		// A database failure is not an authentication failure. Saying otherwise
		// tells every human their password is wrong during a brief outage.
		return User{}, werr.Wrap(werr.Internal, "could not sign in", err)
	}

	if hash == nil {
		// No password login configured (system accounts). Still burn the time.
		DummyVerify(password)
		return fail()
	}

	ok, err := VerifyPassword(password, *hash)
	if err != nil {
		return User{}, werr.Wrap(werr.Internal, "could not sign in", err)
	}
	if !ok {
		return fail()
	}
	if u.Status != StatusActive {
		return fail()
	}
	return u, nil
}

// Get returns a user by id.
func Get(ctx context.Context, q Querier, id string) (User, error) {
	if !ids.Valid(id, ids.User) {
		return User{}, werr.New(werr.NotFound, "no such user")
	}
	var u User
	err := q.QueryRow(ctx, `
		SELECT id, email, status, created_at FROM users WHERE id = $1
	`, id).Scan(&u.ID, &u.Email, &u.Status, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, werr.New(werr.NotFound, "no such user")
	}
	if err != nil {
		return User{}, werr.Wrap(werr.Internal, "could not load user", err)
	}
	return u, nil
}

// normalizeEmail returns the address as typed and the form used for uniqueness.
//
// The whole address is lowercased. RFC 5321 says the local part is technically
// case-sensitive, but no mail provider in practice treats it that way, and the
// alternative — Alice@x.com and alice@x.com as two accounts, indistinguishable
// in every interface — is a support burden and an impersonation vector.
func normalizeEmail(raw string) (addr, norm string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > MaxEmailLen {
		return "", "", werr.New(werr.InvalidParams, "email is required and must be under 254 characters")
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", "", werr.New(werr.InvalidParams, "email may not contain control or formatting characters")
		}
	}

	parsed, perr := mail.ParseAddress(trimmed)
	if perr != nil || parsed.Address != trimmed {
		// Requiring the parse to round-trip rejects "Name <a@b.c>" forms, which
		// would otherwise let two spellings reach one mailbox.
		return "", "", werr.New(werr.InvalidParams, "that does not look like an email address")
	}

	return trimmed, strings.ToLower(trimmed), nil
}

// checkPassword enforces length only.
//
// Composition rules ("one uppercase, one digit, one symbol") measurably reduce
// entropy by funnelling people into predictable patterns, which is why NIST
// stopped recommending them. Length is what matters; the argon2 cost handles the
// rest.
func checkPassword(p string) error {
	if n := len([]rune(p)); n < MinPasswordLen || n > MaxPasswordLen {
		return werr.New(werr.InvalidParams,
			fmt.Sprintf("password must be between %d and %d characters", MinPasswordLen, MaxPasswordLen))
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return werr.New(werr.InvalidParams, "password may not contain control characters")
		}
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
