package identity_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/db"
	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/identity"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
	"github.com/mistyuk/worldzero/internal/testutil"
)

func newService() *identity.Service {
	clk := clock.System{}
	gen := ids.NewGenerator(clk)
	return identity.NewService(clk, gen, events.NewAppender(clk, gen))
}

func register(t *testing.T, svc *identity.Service, d *db.DB, name string) (identity.Agent, events.Event, error) {
	t.Helper()

	var (
		agent identity.Agent
		ev    events.Event
	)
	err := d.Tx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		agent, ev, err = svc.Register(ctx, tx, identity.RegisterParams{
			Name:       name,
			ModelLabel: "test-harness",
		})
		return err
	})
	return agent, ev, err
}

func TestRegisterWritesAgentAndEventTogether(t *testing.T) {
	d := testutil.DB(t)
	svc := newService()
	name := testutil.Name(t)

	agent, ev, err := register(t, svc, d, name)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if !ids.Valid(agent.ID, ids.Agent) {
		t.Fatalf("agent id %q is malformed", agent.ID)
	}
	if agent.Status != identity.StatusActive {
		t.Fatalf("new citizen status = %q, want %q", agent.Status, identity.StatusActive)
	}

	// The event must be visible in the feed, and must be about this agent.
	found, err := events.Since(context.Background(), d.Pool(), ev.Seq-1, 10)
	if err != nil {
		t.Fatalf("read feed: %v", err)
	}

	var got *events.Event
	for i := range found {
		if found[i].ID == ev.ID {
			got = &found[i]
			break
		}
	}
	if got == nil {
		t.Fatal("AGENT_REGISTERED is not in the world feed; invariant #2 broken")
	}
	if got.Type != events.TypeAgentRegistered {
		t.Fatalf("event type = %q, want %q", got.Type, events.TypeAgentRegistered)
	}
	if got.AgentID == nil || *got.AgentID != agent.ID {
		t.Fatalf("event actor = %v, want %q", got.AgentID, agent.ID)
	}
	if got.SubjectIDs["agent"] != agent.ID {
		t.Fatalf("event subject = %q, want %q", got.SubjectIDs["agent"], agent.ID)
	}
	if !strings.Contains(string(got.Payload), name) {
		t.Fatalf("payload %s does not record the name a historian would need", got.Payload)
	}
}

// TestRegisterIsAtomic is invariant #2 from the other direction: if the
// transaction does not commit, the citizen must not exist either. State and
// history move together or not at all.
func TestRegisterIsAtomic(t *testing.T) {
	d := testutil.DB(t)
	svc := newService()
	name := testutil.Name(t)

	ctx := context.Background()
	err := d.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, _, err := svc.Register(ctx, tx, identity.RegisterParams{Name: name}); err != nil {
			return err
		}
		// Something later in the transaction fails.
		return errors.New("simulated failure after registration")
	})
	if err == nil {
		t.Fatal("transaction was expected to fail")
	}

	var count int
	if err := d.Pool().QueryRow(ctx,
		`SELECT count(*) FROM agents WHERE name = $1`, name).Scan(&count); err != nil {
		t.Fatalf("count agents: %v", err)
	}
	if count != 0 {
		t.Fatal("agent survived a rolled-back transaction: state and events can diverge")
	}
}

func TestRegisterRejectsDuplicateName(t *testing.T) {
	d := testutil.DB(t)
	svc := newService()
	name := testutil.Name(t)

	if _, _, err := register(t, svc, d, name); err != nil {
		t.Fatalf("first register: %v", err)
	}

	_, _, err := register(t, svc, d, name)
	if err == nil {
		t.Fatal("two citizens registered under one name")
	}
	if got := werr.CodeOf(err); got != werr.NameTaken {
		t.Fatalf("error code = %q, want %q", got, werr.NameTaken)
	}
}

// TestRegisterRejectsHostileNames covers the "unicode/control chars in names"
// line of the ChaosBot attack list. Names are rendered in feeds, dashboards and
// eventually other agents' prompts, so a name that can lie about what it says
// is a forgery primitive rather than a cosmetic problem.
func TestRegisterRejectsHostileNames(t *testing.T) {
	d := testutil.DB(t)
	svc := newService()

	cases := map[string]string{
		"empty":            "",
		"whitespace only":  "   ",
		"single character": "x",
		"too long":         strings.Repeat("a", 33),
		"newline":          "No\nva",
		"carriage return":  "No\rva",
		"nul byte":         "No\x00va",
		"bell":             "No\ava",
		"escape":           "No\x1bva",
	}

	// Invisible runes that let a displayed name differ from the stored one.
	// Given as code points rather than literals: Go rejects a literal BOM in
	// source outright, and the rest are unreadable in a diff even where legal.
	for label, r := range map[string]rune{
		"right-to-left override": 0x202E,
		"left-to-right isolate":  0x2066,
		"pop directional":        0x202C,
		"zero width joiner":      0x200D,
		"zero width space":       0x200B,
		"byte order mark":        0xFEFF,
	} {
		cases[label] = "No" + string(r) + "va"
	}

	for label, name := range cases {
		t.Run(label, func(t *testing.T) {
			_, _, err := register(t, svc, d, name)
			if err == nil {
				t.Fatalf("accepted hostile name %q", name)
			}
			if got := werr.CodeOf(err); got != werr.InvalidParams {
				t.Fatalf("error code = %q, want %q", got, werr.InvalidParams)
			}
		})
	}
}

func TestRegisterCollapsesWhitespace(t *testing.T) {
	d := testutil.DB(t)
	svc := newService()

	base := testutil.Name(t)
	agent, _, err := register(t, svc, d, "  "+base+"   x  ")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if want := base + " x"; agent.Name != want {
		t.Fatalf("name = %q, want %q: lookalike names must not be distinct citizens", agent.Name, want)
	}
}

func TestGetRejectsForgedIDsWithoutQuerying(t *testing.T) {
	d := testutil.DB(t)

	for _, id := range []string{"", "agent_", "loc_01ARZ3NDEKTSV4RRFFQ69G5FAV", "agent_' OR 1=1 --"} {
		_, err := identity.Get(context.Background(), d.Pool(), id)
		if err == nil {
			t.Fatalf("looked up forged id %q successfully", id)
		}
		if got := werr.CodeOf(err); got != werr.NotFound {
			t.Fatalf("id %q: error code = %q, want %q", id, got, werr.NotFound)
		}
	}
}
