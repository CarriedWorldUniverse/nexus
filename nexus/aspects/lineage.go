// Lineage vocabulary for derived "hand" identities (roundtable P2,
// NEX-571).
//
// A hand is a fresh-context instance of an aspect, spawned by that
// aspect mid-turn for background fan-out. The broker enforces one live
// session per name, and the parent stays awake while its hands run —
// so hands register under DERIVED names: `<base>.sub-N`. The derived
// name is an audit lineage tag, not a separate persona: persona/config
// lookups for a derived name serve the BASE aspect's bundle, and a
// hand's effective scope is never wider than its parent's (the cairn
// transitive-permission model).
//
// v1 derived names are minted broker-locally (a broker-signed session
// JWT for the sub, see aspects.MintDerivedSession); when herald-rooted
// boot lands, DeriveAgentKey takes over the minting and this naming
// scheme stays the contract.

package aspects

import (
	"fmt"
	"regexp"
)

// derivedNameRe matches `<base>.sub-N`. The base group is greedy, so a
// hypothetical "a.sub-1.sub-2" resolves to base "a.sub-1" — one level
// of stripping per call; v1 forbids sub-of-sub at the spawn surface so
// nesting never occurs in practice.
var derivedNameRe = regexp.MustCompile(`^(.+)\.sub-([0-9]+)$`)

// IsDerivedName reports whether name is a derived hand identity
// (`<base>.sub-N`).
func IsDerivedName(name string) bool {
	return derivedNameRe.MatchString(name)
}

// BaseName returns the base aspect a derived name belongs to. Names
// that aren't derived are returned unchanged, so callers can use it
// unconditionally as "the identity whose persona/config applies".
func BaseName(name string) string {
	if m := derivedNameRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return name
}

// DerivedName composes the hand identity for base's n-th hand slot.
func DerivedName(base string, n int) string {
	return fmt.Sprintf("%s.sub-%d", base, n)
}
