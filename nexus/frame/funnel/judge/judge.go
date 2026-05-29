// Package judge builds the cheap-model output filter (hard rules + a
// cheap-tier LLM classifier) shared by both nexus runtimes: the
// in-process Frame (nexus/cmd/nexus) and the out-of-process agentfunnel
// (runtime/cmd/agentfunnel). Before NEX-365 #2 each runtime carried its
// own near-identical copy of this logic — the provider switch, the
// native/bare judge-provider decorators, the model-default + bare-tier
// expansion, and the CheapModelFilter assembly — which drifted (the
// NEX-365 #3 cross-provider fix had to be written twice). This package is
// the single source of truth; the runtimes resolve their inputs (judge
// provider name / model / credential env, from aspect.json or the broker)
// and hand them to BuildFilter.
//
// It lives under funnel rather than in funnel itself so the funnel package
// stays provider-constructor-free (it works in bridle.Provider/Harness
// abstractions); this package is where the concrete bridle provider
// constructors are allowed.
package judge

import (
	"log/slog"
	"sort"
	"strings"

	bridle "github.com/CarriedWorldUniverse/bridle"
	claudeprovider "github.com/CarriedWorldUniverse/bridle/provider/claude"
	claudecodeprovider "github.com/CarriedWorldUniverse/bridle/provider/claudecode"
	openaiprovider "github.com/CarriedWorldUniverse/bridle/provider/openai"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
)

// Spec is the resolved input to BuildFilter. The runtimes differ in WHERE
// these values come from (Frame: aspect.json + credentials.Store;
// agentfunnel: the broker over WS) but the construction from here on is
// identical.
type Spec struct {
	// Label prefixes log lines so operators can tell which runtime built
	// the judge ("frame funnel" / "agentfunnel").
	Label string

	// MainProvider/MainProviderID/MainModel describe the aspect's primary
	// provider. The judge inherits this provider when JudgeProviderName is
	// empty, and falls back to MainModel as the judge model for non-Claude
	// providers (no haiku tier exists off-Claude).
	MainProvider   bridle.Provider
	MainProviderID bridle.ProviderID
	MainModel      string

	// JudgeProviderName, when non-empty, routes the judge to a standalone
	// provider family (claude-api / claude-code / openai) distinct from the
	// aspect's primary — the NEX-365 #3 cross-provider judging lever. Empty
	// = inherit MainProvider.
	JudgeProviderName string

	// JudgeModel overrides the per-flavor default ("haiku" for Claude,
	// MainModel otherwise). Bare claude tiers are expanded to full ids for
	// the native SDK path (see funnel.ExpandBareClaudeTier).
	JudgeModel string

	// JudgeEnv is the resolved judge-credential env overlay
	// (ANTHROPIC_API_KEY/ANTHROPIC_BASE_URL, or OPENAI_* for OpenAI-shape).
	// It serves two roles: it pins native providers at CONSTRUCTION (the
	// native SDK reads creds at construction, not per-turn) AND it is passed
	// as CheapModelFilter.ProviderEnv for the claude-code subprocess path
	// (which honours per-turn env). nil = inherit ambient process env.
	JudgeEnv map[string]string

	// AspectHome is forwarded to CheapModelFilter. The Frame sets it; the
	// agentfunnel leaves it empty.
	AspectHome string

	ObsHook funnel.ObservabilityHook
	Logger  *slog.Logger
}

// BuildFilter constructs the output filter for an aspect: a HardRulesFilter
// wrapping a CheapModelFilter (the cheap-tier LLM judge). It downgrades to
// a bare HardRulesFilter (no cheap judge, never silent always-post) when
// the judge provider can't be built or no judge model can be resolved.
func BuildFilter(spec Spec) funnel.OutputFilter {
	log := spec.Logger

	// Resolve which provider the judge runs on. Override → a standalone
	// provider for the named family (cred-pinned from JudgeEnv). Inherit →
	// the aspect's own provider, then pin a native one to the judge
	// credential's endpoint if one was resolved.
	var jp bridle.Provider
	var jid bridle.ProviderID
	if name := strings.TrimSpace(spec.JudgeProviderName); name != "" {
		p, id, ok := BuildProvider(name, spec.JudgeEnv, log)
		if !ok {
			log.Warn(spec.Label+": filter=cheap requested but judge provider unbuildable; downgrading to hard",
				"judge_provider", name)
			return funnel.HardRulesFilter{}
		}
		jp, jid = p, id
	} else {
		// Inherit the aspect's own provider, then pin a native one to the
		// judge credential's endpoint if one was resolved. (Real callers
		// always pass a non-nil MainProvider; we don't guard nil here so
		// behaviour matches the pre-NEX-365-#2 builders, whose inherit path
		// never produced a nil provider.)
		jp, jid = spec.MainProvider, spec.MainProviderID
		jp = NativeJudgeProvider(jp, jid, spec.JudgeEnv)
	}

	// Model: explicit override, else per-flavor default (haiku for Claude —
	// the CLI's own default tier for claude-code; MainModel otherwise).
	model := strings.TrimSpace(spec.JudgeModel)
	if model == "" {
		if IsClaudeFlavor(jid) {
			model = "haiku"
		} else {
			model = spec.MainModel
		}
	}
	// NEX-369: claude-code's CLI accepts a bare tier ("haiku"), but the
	// native Anthropic SDK (claude-api) needs a full model id — a bare
	// "haiku" 404s → the judge degrades + fails open. Expand bare tiers to
	// full ids on the native path; claude-code keeps the CLI shorthand.
	model = funnel.ExpandBareClaudeTier(model, jid)
	if model == "" {
		log.Warn(spec.Label+": no judge model resolvable for provider; filter=hard, no cheap-judge",
			"provider", jid)
		return funnel.HardRulesFilter{}
	}

	log.Info(spec.Label+": filter=cheap (hard rules + cheap-model judge)",
		"judge_provider", jid, "judge_model", model, "judge_env_keys", envKeyNames(spec.JudgeEnv))
	return funnel.HardRulesFilter{
		Inner: &funnel.CheapModelFilter{
			Harness:           bridle.NewHarness(BareJudgeProvider(jp, jid)),
			Provider:          jid,
			Model:             model,
			AspectHome:        spec.AspectHome,
			Logger:            log,
			ObservabilityHook: spec.ObsHook,
			ProviderEnv:       spec.JudgeEnv,
		},
	}
}

// BuildProvider instantiates a standalone cheap-judge provider for the
// named family. For native Anthropic/OpenAI shapes it pins the key + base
// URL from env at construction (the native SDKs read creds at construction,
// not per-turn) so e.g. a Claude aspect can be judged by a DeepSeek
// Anthropic-shape endpoint. claude-code is left to BareJudgeProvider +
// ProviderEnv (the subprocess honours per-turn env). Returns ok=false on an
// unrecognised name so the caller can downgrade gracefully.
func BuildProvider(name string, env map[string]string, log *slog.Logger) (bridle.Provider, bridle.ProviderID, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "claude-api", "claude":
		key, base := env["ANTHROPIC_API_KEY"], env["ANTHROPIC_BASE_URL"]
		if key != "" || base != "" {
			return claudeprovider.NewWithBaseURL(key, base), bridle.ProviderClaude, true
		}
		return claudeprovider.New(""), bridle.ProviderClaude, true
	case "claude-code", "claudecode":
		return claudecodeprovider.New(), bridle.ProviderClaudeCode, true
	case "openai":
		return openaiprovider.NewWithBaseURL(env["OPENAI_API_KEY"], env["OPENAI_BASE_URL"]),
			bridle.ProviderID("openai"), true
	default:
		log.Warn("judge: provider unrecognised", "provider", name)
		return nil, "", false
	}
}

// NativeJudgeProvider rebuilds a native Anthropic-shape judge provider from
// the resolved judge-credential env so a judge credential actually
// redirects the judge — even on the inherit path with no explicit
// JudgeProviderName. The native SDK reads creds at CONSTRUCTION (not
// per-turn), so CheapModelFilter.ProviderEnv (honoured only by claude-code
// subprocesses) can't redirect it. Without this a native judge inherits the
// aspect's ambient ANTHROPIC_* env and judges on the MAIN endpoint,
// defeating a separate cheap-judge credential. NEX-365 #3.
//
// Returns the inbound provider unchanged for non-native ids (claude-code
// keeps its BareJudgeProvider subprocess path) or when no judge env was
// resolved (preserving pre-NEX-365 behaviour).
func NativeJudgeProvider(p bridle.Provider, id bridle.ProviderID, env map[string]string) bridle.Provider {
	if id != bridle.ProviderClaude && id != "claude" {
		return p
	}
	if len(env) == 0 {
		return p
	}
	key, base := env["ANTHROPIC_API_KEY"], env["ANTHROPIC_BASE_URL"]
	if key == "" && base == "" {
		return p
	}
	return claudeprovider.NewWithBaseURL(key, base)
}

// BareJudgeProvider returns the provider the cheap-judge harness should use.
// For claude-code the aspect's provider has full CLI surface (hooks, LSP,
// plugin sync, CLAUDE.md auto-discovery, keychain, attribution) — fine for
// the deliberation loop, wasteful and contaminating for a short-lived
// classifier subprocess. We construct a fresh claudecode.Provider with
// Bare=true so the judge spawns a minimal CLI: no hooks, no plugin sync, no
// auto-discovery, no memory writes.
//
// --bare is API-key-only: it disables subscription auth and reads only
// ANTHROPIC_API_KEY (pair with a judge credential via ProviderEnv so the
// bare subprocess gets an explicit key pointing at a cheap classifier
// endpoint). Per task #196 — kills the "judge ran as a 9-step agent"
// failure mode where the cheap-judge subprocess auto-discovered CLAUDE.md
// and did real work instead of saying "yes"/"no".
//
// For non-claudecode judges (claude-api, openai) returns the inbound
// provider unchanged — Bare is a CLI-only knob.
func BareJudgeProvider(p bridle.Provider, id bridle.ProviderID) bridle.Provider {
	switch id {
	case "claude-code", "claudecode":
		jp := claudecodeprovider.New()
		jp.Bare = true
		return jp
	}
	return p
}

// IsClaudeFlavor reports whether providerID is one of the Claude providers.
// Used for picking the haiku default judge model. Accepts both the
// canonical IDs ("claude-api", "claude-code") and the aspect.json /
// validation-response aliases ("claude", "claudecode").
func IsClaudeFlavor(id bridle.ProviderID) bool {
	switch id {
	case "claude-api", "claude-code", "claude", "claudecode":
		return true
	}
	return false
}

// envKeyNames returns the env map's keys (sorted) for logging — never the
// values, which carry the API key.
func envKeyNames(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k := range env {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
