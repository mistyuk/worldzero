// Package auth is the world's front door: who is calling, and what may they do.
//
// Invariant #6 governs everything here — assume every agent is hostile. A
// credential proves identity and nothing else; authority comes from the scope
// set the kernel issued, never from anything in a request.
package auth

import "slices"

// Kind is what a credential is for. It determines which scopes are even legal,
// which is how "humans do not play" becomes structural rather than a rule
// someone has to remember.
type Kind string

const (
	// KindAgentKey is a citizen's bearer token. Sent as Authorization: Bearer.
	KindAgentKey Kind = "agent_key"
	// KindUserKey is a human's non-browser credential — scripts, curl, the SDK
	// acting on a human's behalf. Bearer, and carries no ambient authority.
	KindUserKey Kind = "user_key"
	// KindSession is a human's browser cookie. Ambient authority, so it is the
	// only kind that needs CSRF consideration.
	KindSession Kind = "session"
)

// Scope is a capability. Scopes are compared exactly and never by prefix.
type Scope string

const (
	// Agent scopes. Only an agent key may hold these.
	ScopeAgentFull    Scope = "agent:full"
	ScopeAgentRead    Scope = "agent:read"
	ScopeWorldRead    Scope = "world:read"
	ScopeWorldMove    Scope = "world:move"
	ScopeMessagesRead Scope = "messages:read"
	ScopeMessagesSend Scope = "messages:send"
	ScopeWalletRead   Scope = "wallet:read"
	ScopeWalletWrite  Scope = "wallet:write"
	ScopeMarketBuy    Scope = "market:buy"
	ScopeMarketSell   Scope = "market:sell"
	ScopeInventoryUse Scope = "inventory:use"

	// Human scopes. Only a user key or session may hold these.
	ScopeHumanFull Scope = "human:full"
	// ScopeAgentsManage mints citizens and their credentials. It is the most
	// dangerous scope in the world and it is deliberately human-only.
	ScopeAgentsManage Scope = "agents:manage"
	// ScopeObserverRead is how ADR-009's read-only dashboard reads the state of
	// an agent its owner's session owns — the same handlers agents use, so
	// invariant #5 holds with one code path.
	ScopeObserverRead Scope = "observer:read"
)

// implications is the authority graph: holding the key scope implies all the
// scopes it maps to.
//
// This table, not a string prefix, is what decides legality. "An API key may
// never hold human:*" sounds equivalent and is not — it would happily permit
// `agents:manage`, the scope that mints citizens and credentials, onto an agent
// key. It is one character away from the agent-legal `agent:full` and a prefix
// rule cannot tell them apart.
var implications = map[Scope][]Scope{
	ScopeAgentFull: {
		ScopeAgentRead, ScopeWorldRead, ScopeWorldMove,
		ScopeMessagesRead, ScopeMessagesSend,
		ScopeWalletRead, ScopeWalletWrite,
		ScopeMarketBuy, ScopeMarketSell, ScopeInventoryUse,
	},
	ScopeHumanFull: {
		ScopeWorldRead, ScopeObserverRead, ScopeAgentsManage,
	},
}

// rootFor is the widest scope each kind may hold. A scope is legal for a kind
// only if that kind's root implies it (or is it).
var rootFor = map[Kind]Scope{
	KindAgentKey: ScopeAgentFull,
	KindUserKey:  ScopeHumanFull,
	KindSession:  ScopeHumanFull,
}

// ScopeSet is the set of scopes a credential carries.
type ScopeSet []Scope

// Allows reports whether this set grants required, directly or by implication.
//
// There is deliberately no wildcard. A scope added to the world next year must
// not be silently granted to every credential issued this year — new capability
// is opt-in, and a credential's authority can only shrink over time, never grow.
func (s ScopeSet) Allows(required Scope) bool {
	for _, held := range s {
		if held == required {
			return true
		}
		if slices.Contains(implications[held], required) {
			return true
		}
	}
	return false
}

// LegalFor reports whether every scope in the set may be held by this kind.
//
// Checked when a credential is issued AND again when it is verified. The second
// check is not redundant: it means a row edited directly in the database — by a
// bug, a migration or an attacker with SQL access — still cannot turn a human
// session into something that can act as a citizen.
func (s ScopeSet) LegalFor(k Kind) bool {
	root, ok := rootFor[k]
	if !ok {
		return false
	}
	for _, held := range s {
		if held == root {
			continue
		}
		if !slices.Contains(implications[root], held) {
			return false
		}
	}
	return len(s) > 0
}

// Strings renders the set for storage.
func (s ScopeSet) Strings() []string {
	out := make([]string, len(s))
	for i, sc := range s {
		out[i] = string(sc)
	}
	return out
}

// ScopesFrom rebuilds a set from storage.
func ScopesFrom(raw []string) ScopeSet {
	out := make(ScopeSet, len(raw))
	for i, s := range raw {
		out[i] = Scope(s)
	}
	return out
}

// DefaultScopes is what M1 issues for each kind. One value each: the check sites
// are what ADR-015 requires to exist, not a rich taxonomy nobody uses yet.
func DefaultScopes(k Kind) ScopeSet {
	if root, ok := rootFor[k]; ok {
		return ScopeSet{root}
	}
	return nil
}
