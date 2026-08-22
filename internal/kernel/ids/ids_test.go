package ids_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mistyuk/worldzero/internal/kernel/clock"
	"github.com/mistyuk/worldzero/internal/kernel/ids"
)

func TestNewProducesValidPrefixedIDs(t *testing.T) {
	g := ids.NewGenerator(clock.System{})

	id := g.New(ids.Agent)
	if !strings.HasPrefix(id, "agent_") {
		t.Fatalf("id %q lacks its type prefix", id)
	}
	if !ids.Valid(id, ids.Agent) {
		t.Fatalf("generator produced an id its own validator rejects: %q", id)
	}
	if ids.Prefix(id) != ids.Agent {
		t.Fatalf("Prefix(%q) = %q, want %q", id, ids.Prefix(id), ids.Agent)
	}
}

// TestValidRejectsHostileIDs covers the "forged agent IDs" line of the ChaosBot
// attack list. Every one of these should be refused on shape, before any query.
func TestValidRejectsHostileIDs(t *testing.T) {
	g := ids.NewGenerator(clock.System{})
	real := g.New(ids.Agent)
	payload := strings.TrimPrefix(real, "agent_")

	cases := map[string]string{
		"empty":              "",
		"prefix only":        "agent_",
		"no prefix":          payload,
		"wrong prefix":       "loc_" + payload,
		"prefix of a prefix": "agentx_" + payload,
		"sql injection":      "agent_' OR 1=1 --",
		"truncated payload":  "agent_" + payload[:10],
		"padded payload":     "agent_" + payload + "AAAA",
		"lowercase payload":  "agent_" + strings.ToLower(payload),
		"nul byte":           "agent_" + payload + "\x00",
		"newline":            "agent_" + payload + "\n",
		"separator only":     "_",
	}

	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			if ids.Valid(id, ids.Agent) {
				t.Fatalf("accepted hostile id %q", id)
			}
		})
	}
}

func TestNewIsUniqueUnderConcurrency(t *testing.T) {
	g := ids.NewGenerator(clock.System{})

	const workers, each = 8, 500

	var (
		mu   sync.Mutex
		seen = make(map[string]struct{}, workers*each)
		wg   sync.WaitGroup
	)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, 0, each)
			for j := 0; j < each; j++ {
				local = append(local, g.New(ids.Event))
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range local {
				if _, dup := seen[id]; dup {
					t.Errorf("duplicate id %q", id)
				}
				seen[id] = struct{}{}
			}
		}()
	}
	wg.Wait()

	if len(seen) != workers*each {
		t.Fatalf("generated %d unique ids, want %d", len(seen), workers*each)
	}
}

// TestNewUsesWorldClock is what makes IDs sort in world order rather than wall
// order when a simulation runs fast (ADR-014).
func TestNewUsesWorldClock(t *testing.T) {
	past := clock.NewManual(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	future := clock.NewManual(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))

	older := ids.NewGenerator(past).New(ids.Event)
	newer := ids.NewGenerator(future).New(ids.Event)

	if !(older < newer) {
		t.Fatalf("ids do not sort by world time: %q should precede %q", older, newer)
	}
}
