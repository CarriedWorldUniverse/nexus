// Lineage vocabulary for derived "hand" identities (roundtable P2,
// NEX-571).
//
// A hand is a fresh-context instance of an aspect, spawned by that
// aspect mid-turn for background fan-out. The broker enforces one live
// session per name, and the parent stays awake while its hands run —
// so hands register under DERIVED names: `<base>.<word>` where <word>
// is a kindred word leased from the base aspect's hand-name pool
// (operator naming decision, 2026-06-11: no numbers — shadow → umbra,
// gloam, shade…; plumb → bob, fathom…). `<base>.hand-N` is the
// overflow fallback when a pool is exhausted.
//
// The derived name is an audit lineage tag, not a separate persona:
// persona/config lookups for a derived name serve the BASE aspect's
// bundle, and a hand's effective scope is never wider than its parent's
// (the cairn transitive-permission model).
//
// Lineage is encoded by the `.` separator: aspect names themselves use
// hyphens (DNS-label style, e.g. `maren-art`) and never contain a dot,
// so any name with a dot is a derived hand and the base is the segment
// before the first dot. This keeps lineage parsing a string-split and
// is independent of the suffix vocabulary.
//
// v1 derived names are minted broker-locally (a broker-signed session
// JWT for the hand, see aspects.MintDerivedSession); when herald-rooted
// boot lands, DeriveAgentKey takes over the minting and this naming
// scheme stays the contract.

package aspects

import (
	"fmt"
	"strings"
)

// AspectHandNames is the built-in per-aspect kindred-word pool. The
// operator amendment (2026-06-11) fixes these starting sets; operators
// extend them via config (the Runner merges Config-supplied pools over
// these defaults). Order is the lease preference order.
var AspectHandNames = map[string][]string{
	"shadow": {"umbra", "gloam", "shade", "dusk", "silhouette", "penumbra", "murk", "tenebra"},
	"plumb":  {"bob", "fathom", "sound", "level", "datum", "line"},
	"anvil":  {"horn", "hardy", "temper", "face", "strike", "pritchel"},
	"keel":   {"ballast", "skeg", "stem", "draft", "hull"},
	"maren":  {"brine", "spume", "swell", "foam", "pearl"},
	"harrow": {"tine", "furrow", "loam", "glebe"},
}

// HandNamePool returns the kindred-word lease order for base. Unknown
// aspects (no built-in pool) get an empty pool — callers fall straight
// through to the `hand-N` overflow naming.
func HandNamePool(base string) []string {
	return AspectHandNames[base]
}

// IsDerivedName reports whether name is a derived hand identity —
// `<base>.<suffix>`. The discriminator is the dot: aspect names use
// hyphens and never contain one, so a dotted name is always a hand.
// The base and the suffix must both be non-empty (`.umbra` and
// `shadow.` are not lineages).
func IsDerivedName(name string) bool {
	i := strings.IndexByte(name, '.')
	return i > 0 && i < len(name)-1
}

// BaseName returns the base aspect a derived name belongs to — the
// segment before the first dot. Names that aren't derived are returned
// unchanged, so callers can use it unconditionally as "the identity
// whose persona/config applies".
func BaseName(name string) string {
	if i := strings.IndexByte(name, '.'); i > 0 && i < len(name)-1 {
		return name[:i]
	}
	return name
}

// DerivedName composes a hand identity for base from a leased suffix
// (a kindred word, or a `hand-N` overflow token). It is the inverse of
// BaseName.
func DerivedName(base, suffix string) string {
	return fmt.Sprintf("%s.%s", base, suffix)
}

// OverflowHandName composes the `<base>.hand-N` overflow identity used
// only when base's kindred-word pool is exhausted (cap 4 concurrent
// makes this unreachable in practice for the built-in pools).
func OverflowHandName(base string, n int) string {
	return fmt.Sprintf("%s.hand-%d", base, n)
}
