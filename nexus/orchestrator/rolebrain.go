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

// RoleBrainResolver is a RoleResolver that resolves the role->brain mapping
// (Provider/Model/Effort) and the role->skill-allowlist mapping — the two
// pieces of the role-at-spawn overlay this resolver ships concrete,
// production-wired implementations for. RolePrompt/PolicyFragment stay
// unresolved ("", nil) from this resolver, exactly like a nil
// Orchestrator.Resolver — see README.md "Role resolution (out of scope, by
// design)": that gap is unchanged by this type; only the brain and
// skill-allowlist fields are resolved here.
//
// Brains is keyed by role label (e.g. "builder-complex"); a role absent from
// the map (or with an empty Provider) resolves to ("", "") for
// provider/model, which dispatchOne/SubmitPoolItem/resolveProvider all treat
// as "no role-brain override". Skills is likewise keyed by role label; a
// role absent from Skills resolves to a nil allowlist, which
// nexus-skills-mcp treats as "all skills" (the ungated back-compat default)
// — so an unconfigured role behaves exactly as before this field existed.
// nexus/cmd/nexus/orchestrator_wiring.go populates Brains from
// ORCHESTRATOR_ROLE_BRAINS and Skills from ORCHESTRATOR_ROLE_SKILLS at boot.
type RoleBrainResolver struct {
	Brains map[string]RoleBrain
	// Skills maps a role label to its skill allowlist — the exact skill
	// names (from the .agents/skills store) a spawn of this role may
	// discover/load via nexus-skills-mcp. Empty/absent = nil allowlist =
	// all skills (ungated). Scoping this per role is the context-hygiene
	// lever (operator directive, 2026-07-07): a spawn only sees the skills
	// its work plausibly needs, so search_skills is trimmed and get_skill
	// is hard-denied outside the set (agentskills.FilterAllowlist/AllowedName).
	Skills map[string][]string
}

// Resolve implements RoleResolver. See the type doc for what it does and
// does not resolve.
func (r RoleBrainResolver) Resolve(role string) (rolePrompt string, skillAllowlist []string, policy *funnel.ToolPolicy, provider string, model string, effort string) {
	b := r.Brains[role]
	return "", r.Skills[role], nil, b.Provider, b.Model, b.Effort
}
