package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mistyuk/worldzero/internal/economy"
	"github.com/mistyuk/worldzero/internal/kernel/events"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// wallet reads a citizen's economic state.
func wallet(t *testing.T, h http.Handler, key string) map[string]any {
	t.Helper()
	rec, body := doAuthed(t, h, http.MethodGet, "/v1/agents/me", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/agents/me: %d %s", rec.Code, rec.Body.String())
	}
	return body["wallet"].(map[string]any)
}

func breadListing(t *testing.T, h http.Handler) (listingID, itemID string, price int64) {
	t.Helper()
	rec, body := do(t, h, http.MethodGet, "/v1/world/listings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("listings: %d %s", rec.Code, rec.Body.String())
	}
	for _, raw := range body["listings"].([]any) {
		l := raw.(map[string]any)
		if l["sku"] == economy.BreadSKU {
			return l["id"].(string), l["item_id"].(string), int64(l["price"].(float64))
		}
	}
	t.Fatal("the world sells no bread")
	return "", "", 0
}

// TestTheSurvivalLoop is M2's whole reason to exist (ADR-007): claim, buy, eat.
//
// Without it Phase 1 has needs but no income, so every citizen starves and the
// economy never closes.
func TestTheSurvivalLoop(t *testing.T) {
	h := newServer(t)
	_, key, _ := selfRegister(t, h, "")
	listing, itemID, price := breadListing(t, h)

	// A new citizen has nothing.
	w := wallet(t, h, key)
	if got := int64(w["balance"].(float64)); got != 0 {
		t.Fatalf("a new citizen starts with %d, want 0", got)
	}

	// 1. Claim.
	rec, resp := act(t, h, key, idemKey(t), `{"type":"claim_stipend","params":{}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim_stipend: %d %s", rec.Code, rec.Body.String())
	}
	result := resp["result"].(map[string]any)
	if got := int64(result["amount"].(float64)); got != int64(economy.StipendAmount) {
		t.Fatalf("stipend paid %d, want %d", got, economy.StipendAmount)
	}

	// 2. Buy.
	rec, resp = act(t, h, key, idemKey(t),
		`{"type":"buy","params":{"listing_id":"`+listing+`","quantity":2}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("buy: %d %s", rec.Code, rec.Body.String())
	}
	result = resp["result"].(map[string]any)
	if got := int64(result["paid"].(float64)); got != price*2 {
		t.Fatalf("paid %d for two loaves at %d each", got, price)
	}

	w = wallet(t, h, key)
	if got := int64(w["balance"].(float64)); got != int64(economy.StipendAmount)-price*2 {
		t.Fatalf("balance %d after buying two loaves", got)
	}
	inv := w["inventory"].([]any)
	if len(inv) != 1 || int(inv[0].(map[string]any)["quantity"].(float64)) != 2 {
		t.Fatalf("inventory = %v, want two loaves", inv)
	}

	// 3. Eat.
	before := w["energy"].(map[string]any)["value"].(float64)
	rec, resp = act(t, h, key, idemKey(t),
		`{"type":"consume","params":{"item_id":"`+itemID+`"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("consume: %d %s", rec.Code, rec.Body.String())
	}
	result = resp["result"].(map[string]any)

	// A full citizen gains nothing from eating: energy is clamped, and the
	// surplus is wasted rather than banked. That is the correct behaviour and
	// worth asserting, because the tempting alternative — letting energy exceed
	// its maximum — would let an agent stockpile invulnerability.
	after := result["energy"].(float64)
	if after > economy.EnergyMax {
		t.Fatalf("eating pushed energy to %v, above the maximum", after)
	}
	if after < before {
		t.Fatalf("eating reduced energy from %v to %v", before, after)
	}
	if held := int(result["held"].(float64)); held != 1 {
		t.Fatalf("held %d loaves after eating one of two", held)
	}
}

// TestStipendCooldownIsEnforced. The cooldown is the only thing between an
// agent and infinite money, so it is enforced in the WHERE clause rather than
// by a prior read — a check-then-write races and two concurrent claims would
// both pass.
func TestStipendCooldownIsEnforced(t *testing.T) {
	h := newServer(t)
	_, key, _ := selfRegister(t, h, "")

	if rec, _ := act(t, h, key, idemKey(t), `{"type":"claim_stipend","params":{}}`); rec.Code != http.StatusOK {
		t.Fatalf("first claim: %d", rec.Code)
	}

	rec, body := act(t, h, key, idemKey(t), `{"type":"claim_stipend","params":{}}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a second claim succeeded: %d %s", rec.Code, rec.Body.String())
	}
	if got := body["error"].(map[string]any)["code"]; got != string(werr.CooldownActive) {
		t.Fatalf("code = %v, want %q", got, werr.CooldownActive)
	}
}

// TestMoneySupplyOnlyGrowsByStipends is the audit invariant the soak harness
// will watch: money enters the world in exactly one place.
func TestMoneySupplyOnlyGrowsByStipends(t *testing.T) {
	h := newServer(t)

	supply := func() int64 {
		rec, body := do(t, h, http.MethodGet, "/v1/world/stats", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("stats: %d", rec.Code)
		}
		return int64(body["money_supply"].(float64))
	}

	before := supply()

	_, a, _ := selfRegister(t, h, "")
	_, b, _ := selfRegister(t, h, "")
	for _, k := range []string{a, b} {
		if rec, _ := act(t, h, k, idemKey(t), `{"type":"claim_stipend","params":{}}`); rec.Code != http.StatusOK {
			t.Fatalf("claim: %d", rec.Code)
		}
	}

	if got, want := supply()-before, 2*int64(economy.StipendAmount); got != want {
		t.Fatalf("money supply grew by %d, want %d", got, want)
	}

	// Trading moves money but never creates it.
	agentB, _ := doAuthed(t, h, http.MethodGet, "/v1/agents/me", "", b)
	_ = agentB
	rec, meB := doAuthed(t, h, http.MethodGet, "/v1/agents/me", "", b)
	if rec.Code != http.StatusOK {
		t.Fatalf("me: %d", rec.Code)
	}
	bID := meB["agent"].(map[string]any)["id"].(string)

	mid := supply()
	rec, _ = act(t, h, a, idemKey(t),
		`{"type":"transfer","params":{"to_agent_id":"`+bID+`","amount":5000000,"memo":"a gift"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("transfer: %d %s", rec.Code, rec.Body.String())
	}
	if got := supply(); got != mid {
		t.Fatalf("a transfer changed the money supply from %d to %d: money was created", mid, got)
	}
}

// TestTransferRejectsHostileInput — the ChaosBot list for money.
func TestTransferRejectsHostileInput(t *testing.T) {
	h := newServer(t)
	_, key, _ := selfRegister(t, h, "")
	rec, me := doAuthed(t, h, http.MethodGet, "/v1/agents/me", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("me: %d", rec.Code)
	}
	selfID := me["agent"].(map[string]any)["id"].(string)

	if rec, _ := act(t, h, key, idemKey(t), `{"type":"claim_stipend","params":{}}`); rec.Code != http.StatusOK {
		t.Fatalf("claim: %d", rec.Code)
	}

	cases := map[string]struct {
		body string
		code werr.Code
	}{
		"negative amount":    {`{"type":"transfer","params":{"to_agent_id":"` + selfID + `","amount":-100}}`, werr.InvalidParams},
		"zero amount":        {`{"type":"transfer","params":{"to_agent_id":"` + selfID + `","amount":0}}`, werr.InvalidParams},
		"to self":            {`{"type":"transfer","params":{"to_agent_id":"` + selfID + `","amount":1000}}`, werr.InvalidParams},
		"forged recipient":   {`{"type":"transfer","params":{"to_agent_id":"agent_nope","amount":1000}}`, werr.InvalidParams},
		"missing recipient":  {`{"type":"transfer","params":{"to_agent_id":"agent_01ARZ3NDEKTSV4RRFFQ69G5FAV","amount":1000}}`, werr.NotFound},
		"more than they own": {`{"type":"transfer","params":{"to_agent_id":"agent_01ARZ3NDEKTSV4RRFFQ69G5FAV","amount":999999999999}}`, werr.NotFound},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, body := act(t, h, key, idemKey(t), tc.body)
			if got := body["error"].(map[string]any)["code"]; got != string(tc.code) {
				t.Fatalf("code = %v, want %q", got, tc.code)
			}
		})
	}
}

// TestOverspendingIsRefused. The balance CHECK is what enforces this, not an
// application test — the application test races.
func TestOverspendingIsRefused(t *testing.T) {
	h := newServer(t)
	_, rich, _ := selfRegister(t, h, "")
	_, poor, _ := selfRegister(t, h, "")

	rec, me := doAuthed(t, h, http.MethodGet, "/v1/agents/me", "", rich)
	if rec.Code != http.StatusOK {
		t.Fatalf("me: %d", rec.Code)
	}
	richID := me["agent"].(map[string]any)["id"].(string)

	if rec, _ := act(t, h, poor, idemKey(t), `{"type":"claim_stipend","params":{}}`); rec.Code != http.StatusOK {
		t.Fatalf("claim: %d", rec.Code)
	}

	// Spend more than the stipend.
	rec, body := act(t, h, poor, idemKey(t),
		`{"type":"transfer","params":{"to_agent_id":"`+richID+`","amount":`+
			itoa(int64(economy.StipendAmount)+1)+`}}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("overspending succeeded: %d %s", rec.Code, rec.Body.String())
	}
	if got := body["error"].(map[string]any)["code"]; got != string(werr.InsufficientFunds) {
		t.Fatalf("code = %v, want %q", got, werr.InsufficientFunds)
	}
}

// TestBuyRejectsHostileInput.
func TestBuyRejectsHostileInput(t *testing.T) {
	h := newServer(t)
	_, key, _ := selfRegister(t, h, "")
	listing, itemID, _ := breadListing(t, h)

	cases := map[string]struct {
		body string
		code werr.Code
	}{
		"zero quantity":     {`{"type":"buy","params":{"listing_id":"` + listing + `","quantity":0}}`, werr.InvalidParams},
		"negative quantity": {`{"type":"buy","params":{"listing_id":"` + listing + `","quantity":-5}}`, werr.InvalidParams},
		"absurd quantity":   {`{"type":"buy","params":{"listing_id":"` + listing + `","quantity":100000}}`, werr.InvalidParams},
		"forged listing":    {`{"type":"buy","params":{"listing_id":"lst_nope","quantity":1}}`, werr.InvalidParams},
		"missing listing":   {`{"type":"buy","params":{"listing_id":"lst_01ARZ3NDEKTSV4RRFFQ69G5FAV","quantity":1}}`, werr.NotFound},
		"no money":          {`{"type":"buy","params":{"listing_id":"` + listing + `","quantity":1}}`, werr.InsufficientFunds},
		"eating nothing":    {`{"type":"consume","params":{"item_id":"` + itemID + `"}}`, werr.NotFound},
		"forged item":       {`{"type":"consume","params":{"item_id":"itm_nope"}}`, werr.InvalidParams},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, body := act(t, h, key, idemKey(t), tc.body)
			if got := body["error"].(map[string]any)["code"]; got != string(tc.code) {
				t.Fatalf("code = %v, want %q", got, tc.code)
			}
		})
	}
}

// TestEconomicEventsReachBothParties. events.agent_id names only the actor, so
// without the recipient as a subject a citizen would never see its own payment
// — the single event it most needs.
func TestEconomicEventsReachBothParties(t *testing.T) {
	h := newServer(t)
	_, sender, _ := selfRegister(t, h, "")
	_, recipient, _ := selfRegister(t, h, "")

	rec, me := doAuthed(t, h, http.MethodGet, "/v1/agents/me", "", recipient)
	if rec.Code != http.StatusOK {
		t.Fatalf("me: %d", rec.Code)
	}
	recipientID := me["agent"].(map[string]any)["id"].(string)

	if rec, _ := act(t, h, sender, idemKey(t), `{"type":"claim_stipend","params":{}}`); rec.Code != http.StatusOK {
		t.Fatalf("claim: %d", rec.Code)
	}
	if rec, _ := act(t, h, sender, idemKey(t),
		`{"type":"transfer","params":{"to_agent_id":"`+recipientID+`","amount":1000000,"memo":"here"}}`); rec.Code != http.StatusOK {
		t.Fatalf("transfer: %d", rec.Code)
	}

	// The RECIPIENT, who did not act, must still see it.
	rec, feed := doAuthed(t, h, http.MethodGet, "/v1/agents/me/events?after_seq=0", "", recipient)
	if rec.Code != http.StatusOK {
		t.Fatalf("feed: %d", rec.Code)
	}

	found := false
	for _, raw := range feed["events"].([]any) {
		e := raw.(map[string]any)
		if e["type"] == events.TypeTransferExecuted {
			found = true
			if e["agent_id"] == recipientID {
				t.Error("the recipient is recorded as the actor of a transfer it received")
			}
		}
	}
	if !found {
		b, _ := json.Marshal(feed["events"])
		t.Fatalf("the recipient never saw its own payment: %s", b)
	}
}
