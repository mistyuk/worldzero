package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mistyuk/worldzero/internal/kernel/auth"
	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/db"
	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// Dispatcher executes actions.
type Dispatcher struct {
	registry *Registry
	db       *db.DB
	events   *events.Appender
	limiter  *Limiter
	clk      clock.Clock
	gen      *ids.Generator
}

func NewDispatcher(r *Registry, database *db.DB, ev *events.Appender,
	lim *Limiter, clk clock.Clock, gen *ids.Generator) *Dispatcher {
	return &Dispatcher{registry: r, db: database, events: ev, limiter: lim, clk: clk, gen: gen}
}

// Timeouts, in real time. A statement that hangs must become a coded error
// rather than a held connection.
const (
	lockTimeout      = "2s"
	statementTimeout = "10s"
)

// Dispatch runs one action.
//
// The order is the whole design, so it is worth stating plainly. Outside any
// transaction: authorise, validate the key, look for a stored replay, meter.
// Inside one transaction: lock the actor, reserve the idempotency key, run the
// verb, append its events, store the response.
//
// Two orderings are load-bearing:
//
//   - The replay check comes BEFORE the rate limiter. An agent that was throttled
//     after its action already committed must still be able to learn what
//     happened; charging physics budget to read back a past result would wedge it.
//   - The scope check comes BEFORE the key is reserved. Otherwise a request
//     refused for lack of a capability still consumes the key, and the client is
//     permanently stuck the first time a narrow credential is used — which would
//     defeat the point of having scopes at all.
func (d *Dispatcher) Dispatch(ctx context.Context, p auth.Principal, req Request) (Response, error) {
	if err := ValidateKey(req.IdempotencyKey); err != nil {
		return Response{}, err
	}

	// An unknown verb is rejected before metering: otherwise every invented verb
	// string would mint a fresh budget for the caller.
	handler, ok := d.registry.Lookup(req.Type)
	if !ok {
		return Response{}, werr.New(werr.InvalidParams, "no such action type")
	}
	if !p.Allows(handler.scope()) {
		return Response{}, werr.New(werr.InsufficientScope,
			"this credential does not carry the "+string(handler.scope())+" capability")
	}

	// Replay probe: one primary-key read, no transaction.
	if resp, found, err := d.replay(ctx, p.AgentID, req); err != nil {
		return Response{}, err
	} else if found {
		return resp, nil
	}

	if err := d.limiter.Take(ctx, d.db, p.AgentID, handler.bucket(), d.clk.Real()); err != nil {
		return Response{}, err
	}

	resp, err := d.execute(ctx, p, req, handler)

	switch {
	case err == nil && resp.Replayed:
		// A duplicate is not a second action. The limiter had to be taken
		// optimistically, because whether a request is a replay is only knowable
		// once the key is reserved — so give the unit back now that we know.
		d.limiter.Refund(ctx, d.db, p.AgentID, handler.bucket(), d.clk.Real())

	case err != nil && refusedBeforeActing(err):
		// A request the world refused never became an action. Charging it the
		// verb's own budget would mean an agent that mistypes a parameter three
		// times cannot then do the thing it meant to do — and the agents here
		// are programs, so getting a parameter wrong is how they LEARN the
		// shape of the world.
		//
		// It is not free, though: the cost moves to the misc bucket, which has a
		// deliberately wide burst so the first several mistakes each return the
		// error that names the problem, and a narrow sustained rate so a loop
		// of malformed requests still throttles. Diagnosable for the honest,
		// bounded for the hostile.
		d.limiter.Refund(ctx, d.db, p.AgentID, handler.bucket(), d.clk.Real())
		_ = d.limiter.Take(ctx, d.db, p.AgentID, BucketMisc, d.clk.Real())
	}
	return resp, err
}

// refusedBeforeActing reports whether an error means nothing happened.
//
// These are the codes a verb returns from validation or a missing row: the world
// looked at the request and said no, without changing anything. Deliberately NOT
// included are insufficient_funds, cooldown_active and capacity_full — those are
// real attempts at real actions that happened to fail, and an agent that can
// retry them for free is an agent that can probe the world's state at no cost.
func refusedBeforeActing(err error) bool {
	switch werr.CodeOf(err) {
	case werr.InvalidParams, werr.NotFound, werr.InsufficientScope:
		return true
	default:
		return false
	}
}

func (d *Dispatcher) execute(ctx context.Context, p auth.Principal, req Request, h Handler) (Response, error) {
	var resp Response
	var replayed bool

	err := d.db.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`SET LOCAL lock_timeout = '`+lockTimeout+`'; SET LOCAL statement_timeout = '`+statementTimeout+`'`); err != nil {
			return werr.Wrap(werr.Internal, "could not begin the action", err)
		}

		// The actor lock, taken first so every action by one agent serialises
		// against itself while contending with nobody else.
		//
		// FOR NO KEY UPDATE, not FOR UPDATE. Every foreign key referencing
		// agents takes a KEY SHARE lock on the parent row, and KEY SHARE to
		// UPDATE is a mutual upgrade — two concurrent actions by the same agent
		// that each touch a referencing table would deadlock. NO KEY UPDATE
		// coexists with KEY SHARE and still excludes another writer of the row.
		var a Actor
		err := tx.QueryRow(ctx, `
			SELECT id, name, status, location_id FROM agents WHERE id = $1
			FOR NO KEY UPDATE
		`, p.AgentID).Scan(&a.ID, &a.Name, &a.Status, &a.LocationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return werr.New(werr.NotFound, "no such agent")
		}
		if err != nil {
			return classify(err, "could not load the actor")
		}

		if a.Incapacitated() && !h.allowIncapacitated() {
			return werr.New(werr.Incapacitated,
				"you are incapacitated; eat something first")
		}

		// Reserve the key BEFORE running anything.
		//
		// A concurrent duplicate blocks here on our uncommitted insert, then
		// finds zero rows once we commit and replays our stored answer instead
		// of executing. If we abort, its insert succeeds and it executes, which
		// is also correct. This is the entire idempotency mechanism: no lease,
		// no fencing, no stuck-row sweeper.
		actionID := d.gen.New(ids.Action)
		now := d.clk.Real()

		var reserved string
		err = tx.QueryRow(ctx, `
			INSERT INTO actions (id, actor_id, idempotency_key, type, request_hash, status, created_at)
			VALUES ($1, $2, $3, $4, '\x'::bytea, 'pending', $5)
			ON CONFLICT (actor_id, idempotency_key) DO NOTHING
			RETURNING id
		`, actionID, p.AgentID, req.IdempotencyKey, req.Type, now).Scan(&reserved)

		if errors.Is(err, pgx.ErrNoRows) {
			// Someone else owns this key and has committed. Replay theirs.
			stored, found, rerr := d.replayTx(ctx, tx, p.AgentID, req)
			if rerr != nil {
				return rerr
			}
			if !found {
				// Extremely narrow: the winner committed and the row was swept
				// between the two reads. Retrying with the same key is safe and
				// is the only honest answer.
				return werr.New(werr.IdempotencyInProgress,
					"that key is in use; retry the same request")
			}
			resp, replayed = stored, true
			return nil
		}
		if err != nil {
			return classify(err, "could not reserve the action")
		}

		out, fingerprint, err := h.run(ctx, tx, a, req.Params)
		if err != nil {
			return err
		}

		// Events last, per ADR-012: the append holds a global advisory lock
		// until commit, so everything else happens first.
		refs := make([]EventRef, 0, len(out.Events))
		for _, e := range out.Events {
			if e.AgentID == nil {
				id := a.ID
				e.AgentID = &id
			}
			ev, err := d.events.Append(ctx, tx, e)
			if err != nil {
				return werr.Wrap(werr.Internal, "could not record what happened", err)
			}
			refs = append(refs, EventRef{ID: ev.ID, Type: ev.Type, Seq: ev.Seq})
		}

		resp = Response{
			ActionID: actionID,
			Status:   "succeeded",
			Result:   out.Result,
			Events:   refs,
		}

		body, err := json.Marshal(resp)
		if err != nil {
			return werr.Wrap(werr.Internal, "could not record the result", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE actions
			SET status = 'succeeded', http_status = $1, response = $2,
			    request_hash = $3, completed_at = $4
			WHERE id = $5
		`, http.StatusOK, string(body), fingerprint, d.clk.Real(), actionID); err != nil {
			return classify(err, "could not record the result")
		}
		return nil
	})
	if err != nil {
		return Response{}, err
	}

	resp.Replayed = replayed
	return resp, nil
}

// replay looks for a completed action under this key.
func (d *Dispatcher) replay(ctx context.Context, agentID string, req Request) (Response, bool, error) {
	return d.replayFrom(ctx, d.db.Pool(), agentID, req)
}

func (d *Dispatcher) replayTx(ctx context.Context, tx pgx.Tx, agentID string, req Request) (Response, bool, error) {
	return d.replayFrom(ctx, tx, agentID, req)
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (d *Dispatcher) replayFrom(ctx context.Context, q querier, agentID string, req Request) (Response, bool, error) {
	var (
		storedType string
		storedHash []byte
		status     string
		body       string
	)
	err := q.QueryRow(ctx, `
		SELECT type, request_hash, status, response
		FROM actions WHERE actor_id = $1 AND idempotency_key = $2
	`, agentID, req.IdempotencyKey).Scan(&storedType, &storedHash, &status, &body)

	if errors.Is(err, pgx.ErrNoRows) {
		return Response{}, false, nil
	}
	if err != nil {
		return Response{}, false, classify(err, "could not check for a replay")
	}
	if status != "succeeded" {
		// Only reachable inside the winner's own transaction.
		return Response{}, false, nil
	}

	// Same key, different action. Answering with the first action's result would
	// be silently wrong, so this is a conflict rather than a replay.
	if storedType != req.Type {
		return Response{}, false, werr.New(werr.IdempotencyConflict,
			"that idempotency key was used for a different action")
	}
	if h, err := fingerprintOf(d.registry, req); err == nil && len(h) > 0 && !bytes.Equal(h, storedHash) {
		return Response{}, false, werr.New(werr.IdempotencyConflict,
			"that idempotency key was used with different parameters")
	}

	var resp Response
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return Response{}, false, werr.Wrap(werr.Internal, "stored response is unreadable", err)
	}
	resp.Replayed = true
	return resp, true, nil
}

// fingerprintOf recomputes a request's canonical hash without executing it.
func fingerprintOf(r *Registry, req Request) ([]byte, error) {
	h, ok := r.Lookup(req.Type)
	if !ok {
		return nil, werr.New(werr.InvalidParams, "no such action type")
	}
	return h.fingerprint(req.Params)
}

// classify turns Postgres conditions into codes an agent can act on.
//
// A lock timeout is not an internal error: it means someone else holds the row,
// which for an agent means "retry", and telling it "internal" instead would
// teach every SDK to treat a routine condition as a bug.
func classify(err error, msg string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "55P03": // lock_not_available
			return werr.New(werr.IdempotencyInProgress, "that agent is busy; retry shortly")
		case "57014": // query_canceled (statement_timeout)
			return werr.New(werr.Busy, "the action took too long; retry shortly")
		case "40P01": // deadlock_detected
			return werr.New(werr.Busy, "contention; retry shortly")
		case "23514": // check_violation
			return werr.New(werr.CapacityFull, "that would break a rule of the world")
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return werr.New(werr.Busy, "the server is busy; retry shortly")
	}
	return werr.Wrap(werr.Internal, msg, err)
}
