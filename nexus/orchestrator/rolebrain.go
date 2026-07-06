package orchestrator

import "github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"

// RoleBrain is a role's configured Provider/Model override — the "brain"
// that role tier runs, independent of whatever the leased personality's own
// aspects-row provider/model happen to be (operator decision, 2026-07-06:
// complexity tier is a ROLE property, not a personality property — any
// personality may take either builder role, e.g. "builder" or
// "builder-complex"). Empty Provider (and/or Model) means "no override" for
// that field — see RoleBrainResolver.Resolve and runtime/dispatch/pool.go's
// resolveProvider for how an empty override falls through to the leased
// personality's own binding, then to launch's default.
type RoleBrain struct {
	Provider string
	Model    string
}

// RoleBrainResolver is a RoleResolver that resolves ONLY the role->brain
// mapping (Provider/Model) — the one piece of the role-at-spawn overlay
// role-tier-brains actually ships a concrete, production-wired
// implementation for. RolePrompt/SkillAllowlist/PolicyFragment stay
// unresolved ("", nil, nil) from this resolver, exactly like a nil
// Orchestrator.Resolver — see README.md "Role resolution (out of scope, by
// design)": that gap is unchanged by this type; only the brain fields are
// new here.
//
// Brains is keyed by role label (e.g. "builder-complex"); a role absent from
// the map (or with an empty Provider) resolves to ("", "") for
// provider/model, which dispatchOne/SubmitPoolItem/resolveProvider all treat
// as "no role-brain override" — nexus/cmd/nexus/orchestrator_wiring.go
// populates Brains from the ORCHESTRATOR_ROLE_BRAINS env at boot.
type RoleBrainResolver struct {
	Brains map[string]RoleBrain
}

// Resolve implements RoleResolver. See the type doc for what it does and
// does not resolve.
func (r RoleBrainResolver) Resolve(role string) (rolePrompt string, skillAllowlist []string, policy *funnel.ToolPolicy, provider string, model string) {
	b := r.Brains[role]
	return "", nil, nil, b.Provider, b.Model
}
