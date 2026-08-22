package messaging

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/action"
	"github.com/mistyuk/worldzero/internal/kernel/auth"
	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// Verbs registers speech.
func Verbs(r *action.Registry, clk clock.Clock, gen *ids.Generator) {
	action.Register(r, action.Verb[SendMessageParams]{
		Type:  "send_message",
		Scope: auth.ScopeMessagesSend,
		Emits: []string{events.TypeMessageSent},
		Limit: action.BucketSpeak,
		Exec:  sendMessage(clk, gen),
	})

	action.Register(r, action.Verb[SayParams]{
		Type:  "say",
		Scope: auth.ScopeMessagesSend,
		Emits: []string{events.TypeLocationSay},
		Limit: action.BucketSpeak,
		Exec:  say(clk, gen),
	})
}

// ------------------------------------------------------------ send_message --

type SendMessageParams struct {
	ToAgentID string `json:"to_agent_id"`
	Body      string `json:"body"`
}

func (p SendMessageParams) Validate() error {
	if !ids.Valid(p.ToAgentID, ids.Agent) {
		return werr.New(werr.InvalidParams, "to_agent_id must be a valid agent id")
	}
	if _, err := NormalizeBody(p.Body, MaxDirectBody); err != nil {
		return err
	}
	return nil
}

type SendResult struct {
	MessageID string `json:"message_id"`
	To        string `json:"to_agent_id"`
}

func sendMessage(clk clock.Clock, gen *ids.Generator) func(context.Context, pgx.Tx, action.Actor, SendMessageParams) (action.Outcome, error) {
	return func(ctx context.Context, tx pgx.Tx, a action.Actor, p SendMessageParams) (action.Outcome, error) {
		if p.ToAgentID == a.ID {
			return action.Outcome{}, werr.New(werr.InvalidParams, "you cannot message yourself")
		}

		body, err := NormalizeBody(p.Body, MaxDirectBody)
		if err != nil {
			return action.Outcome{}, err
		}

		var status string
		err = tx.QueryRow(ctx, `SELECT status FROM agents WHERE id = $1`, p.ToAgentID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return action.Outcome{}, werr.New(werr.NotFound, "no such agent")
		}
		if err != nil {
			return action.Outcome{}, werr.Wrap(werr.Internal, "could not find the recipient", err)
		}
		if status == "suspended" {
			return action.Outcome{}, werr.New(werr.Forbidden, "that agent is suspended")
		}

		id := gen.New(ids.Message)
		if _, err := tx.Exec(ctx, `
			INSERT INTO messages (id, from_agent_id, to_agent_id, body, created_at)
			VALUES ($1, $2, $3, $4, $5)
		`, id, a.ID, p.ToAgentID, body, clk.Now()); err != nil {
			return action.Outcome{}, werr.Wrap(werr.Internal, "could not send the message", err)
		}

		return action.Outcome{
			Result: SendResult{MessageID: id, To: p.ToAgentID},
			Events: []events.New{{
				Type: events.TypeMessageSent,
				// Both parties are subjects, so the recipient's feed tells it
				// there is mail. Note what the event does NOT carry: the body,
				// and no location.
				//
				// The body is omitted because events.Since is a PUBLIC firehose.
				// Putting a message body in an event would make every private
				// conversation in the world readable by anyone who polls — which
				// the first ChaosBot to try it would discover immediately.
				// The event says that mail exists; reading it requires being the
				// recipient.
				SubjectIDs: map[string]string{
					"agent": a.ID, "to_agent": p.ToAgentID, "message": id,
				},
				Payload: map[string]any{"length": len([]rune(body))},
			}},
		}, nil
	}
}

// --------------------------------------------------------------------- say --

type SayParams struct {
	Body string `json:"body"`
}

func (p SayParams) Validate() error {
	_, err := NormalizeBody(p.Body, MaxSayBody)
	return err
}

type SayResult struct {
	MessageID  string `json:"message_id"`
	LocationID string `json:"location_id"`
	Heard      int    `json:"heard_by"`
}

// say is speech in a room: anyone present can read it, and so can anyone who
// comes later.
//
// Unlike a direct message, the body IS public, so it goes in the event. A room
// is a public place — that is what makes it different from a private message,
// and it is what lets a world have gossip, journalism and history rather than
// only what each agent personally witnessed.
func say(clk clock.Clock, gen *ids.Generator) func(context.Context, pgx.Tx, action.Actor, SayParams) (action.Outcome, error) {
	return func(ctx context.Context, tx pgx.Tx, a action.Actor, p SayParams) (action.Outcome, error) {
		if a.LocationID == nil {
			return action.Outcome{}, werr.New(werr.InvalidParams,
				"you are nowhere, so there is nobody to hear you")
		}

		body, err := NormalizeBody(p.Body, MaxSayBody)
		if err != nil {
			return action.Outcome{}, err
		}

		var heard int
		if err := tx.QueryRow(ctx,
			`SELECT occupancy FROM locations WHERE id = $1`, *a.LocationID).Scan(&heard); err != nil {
			return action.Outcome{}, werr.Wrap(werr.Internal, "could not read the room", err)
		}

		id := gen.New(ids.Message)
		if _, err := tx.Exec(ctx, `
			INSERT INTO messages (id, from_agent_id, location_id, body, created_at)
			VALUES ($1, $2, $3, $4, $5)
		`, id, a.ID, *a.LocationID, body, clk.Now()); err != nil {
			return action.Outcome{}, werr.Wrap(werr.Internal, "could not speak", err)
		}

		return action.Outcome{
			Result: SayResult{MessageID: id, LocationID: *a.LocationID, Heard: heard - 1},
			Events: []events.New{{
				Type: events.TypeLocationSay,
				SubjectIDs: map[string]string{
					"agent": a.ID, "location": *a.LocationID, "message": id,
				},
				// The body is here on purpose: this was said in public, so it
				// belongs in the public record and in the nearby feed of anyone
				// standing in the room.
				Payload: map[string]any{"body": body},
			}},
		}, nil
	}
}
