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
	// Effort is the role's reasoning-EFFORT knob (low|medium|high, empty =
	// provider default) — the reasoning-EFFORT knob (2026-07-06): lets a
	// complex-tier builder run the claude-api provider at a chosen
	// extended-thinking budget (see runtime/cmd/agentfunnel/main.go's
	// effortToBudgetTokens for the low/medium/high -> budget_tokens table).
	// Threaded the same way as Provider/Model: RoleBrain.Effort ->
	// dispatch.PoolItem.Effort (pool.go) -> dispatch.Brief.Effort (brief.go)
	// -> CW_EFFORT job env (jobspec.go) -> agentfunnel applies it to the
	// claude-api provider's TurnRequest.ThinkingBudgetTokens; a no-op
	// (logged) on claude-code/openai/other providers, which have no
	// request-side thinking-budget knob. Empty = no override.
	Effort string
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
func (r RoleBrainResolver) Resolve(role string) (rolePrompt string, skillAllowlist []string, policy *funnel.ToolPolicy, provider string, model string, effort string) {
	b := r.Brains[role]
	return "", nil, nil, b.Provider, b.Model, b.Effort
}
