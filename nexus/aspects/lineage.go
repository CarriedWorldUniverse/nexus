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

// Pool-worker identity grammar (2026-07-05 naming decision, supersedes the
// `<parent>.sub-N` scheme). An orchestrator worker is named
// `<personality>-<role>` LITERALLY — e.g. `anvil-builder`, `maren-painter`,
// `plumb-security-reviewer`. The personality is an existing single-word aspect
// lent to the pool (its persona/config/credentials are what the worker
// resolves to); the role is the job, drawn from a closed vocabulary and
// supplied at dispatch via the Brief. `sub` existed only so the real named
// aspect could sit in chat alongside a hand — we no longer co-run it, so the
// worker simply IS the personality doing a role.
//
// The split is unambiguous — and never misreads an ordinary hyphenated aspect
// name (`maren-art`) as a worker — because BOTH halves must be known: the
// suffix a registered WorkerRole, the prefix a registered WorkerPersonality.
var (
	// WorkerRoles is the closed role set. SplitWorker matches the LONGEST
	// suffix, so `security-reviewer` is recognised ahead of `reviewer`, and
	// `builder-complex` ahead of `builder` (e.g. `anvil-builder-complex` →
	// ("anvil", "builder-complex"), never a false split against `builder`).
	//
	// `builder-complex` is the heavy-tier builder role (2026-07-06 role-tier
	// decision): same job as `builder`, different BRAIN (provider/model) per
	// the role→brain config (see nexus/orchestrator's role resolver). Tier is
	// a role property, not a personality property — any personality may take
	// either builder role.
	WorkerRoles = []string{"security-reviewer", "builder-complex", "builder", "tester", "reviewer", "painter", "modeller"}

	// WorkerPersonalities is the pool personality set — existing single-word
	// aspects. `shadow` is deliberately ABSENT: it is the orchestrator, not a
	// worker. Any personality may take any role. Operators extend this set
	// via config (POOL_PERSONALITIES), same as the hand-name pools above.
	WorkerPersonalities = []string{"anvil", "plumb", "keel", "maren", "harrow"}
)

func inSet(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// SplitWorker splits a pool-worker identity `<personality>-<role>` into its
// personality and role. ok is false unless the suffix is a known WorkerRole
// AND the remaining prefix is a known WorkerPersonality, so arbitrary aspect
// names are never misclassified. The longest role suffix wins, so
// `anvil-security-reviewer` → ("anvil", "security-reviewer").
func SplitWorker(name string) (personality, role string, ok bool) {
	best := ""
	for _, r := range WorkerRoles {
		suf := "-" + r
		if strings.HasSuffix(name, suf) && len(r) > len(best) {
			if p := name[:len(name)-len(suf)]; inSet(WorkerPersonalities, p) {
				best, personality, role, ok = r, p, r, true
			}
		}
	}
	return personality, role, ok
}

// IsWorkerName reports whether name is a pool-worker identity.
func IsWorkerName(name string) bool { _, _, ok := SplitWorker(name); return ok }

// WorkerName composes a pool-worker identity from a personality and a role
// (`anvil` + `builder` → `anvil-builder`). It is the inverse of SplitWorker.
func WorkerName(personality, role string) string {
	return personality + "-" + role
}

// PersonalityOf returns the identity whose persona/config/credentials a name
// resolves to: the personality for a pool worker `<personality>-<role>`, the
// base aspect for a dotted hand, or the name itself. It is the resolver every
// serving seam should use for "which aspect's bundle applies".
func PersonalityOf(name string) string {
	if p, _, ok := SplitWorker(name); ok {
		return p
	}
	return BaseName(name)
}

// IsDerivedName reports whether name is a derived hand identity —
// `<base>.<suffix>`. The discriminator is the dot: aspect names use
// hyphens and never contain one, so a dotted name is always a hand.
// The base and the suffix must both be non-empty (`.umbra` and
// `shadow.` are not lineages).
func IsDerivedName(name string) bool {
	if IsWorkerName(name) { // pool worker `<personality>-<role>` is a hand of its personality
		return true
	}
	i := strings.IndexByte(name, '.')
	return i > 0 && i < len(name)-1
}

// BaseName returns the base aspect a derived name belongs to — the
// segment before the first dot. Names that aren't derived are returned
// unchanged, so callers can use it unconditionally as "the identity
// whose persona/config applies".
func BaseName(name string) string {
	if p, _, ok := SplitWorker(name); ok { // pool worker → its personality
		return p
	}
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
