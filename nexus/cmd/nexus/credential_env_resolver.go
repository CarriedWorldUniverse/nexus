// Adapter between nexus/credentials.Store and funnel.ProviderEnvResolver.
// Wires #218's per-aspect default credential into every TurnRequest's
// ProviderEnv so providers receive the right API_KEY + BASE_URL for
// the aspect's configured upstream — without a per-call configuration
// dance.

package main

import (
	"context"
	"errors"

	"github.com/CarriedWorldUniverse/bridle"

	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
)

// credentialEnvResolver implements funnel.ProviderEnvResolver by
// consulting the credentials store's per-aspect default. Selects
// Anthropic vs OpenAI shape from the funnel's configured Provider
// (claude-family → Anthropic shape; openai-family → OpenAI shape).
//
// Nil store = no-op resolver (always returns ok=false). Lets dev/
// test wiring skip the credential store entirely without conditional
// plumbing at the funnel.Config site.
type credentialEnvResolver struct {
	store    *credentials.Store
	provider bridle.ProviderID
}

func newCredentialEnvResolver(store *credentials.Store, provider bridle.ProviderID) funnel.ProviderEnvResolver {
	return &credentialEnvResolver{store: store, provider: provider}
}

// Resolve consults the store for the aspect's default credential of
// the shape the provider needs, then returns its env overlay. kind
// (main | compact | filter) is captured for future routing — today
// every kind uses the same default; once we land per-call overrides
// (e.g. "filter uses DeepSeek-Anthropic-shape credential") this is
// where the dispatch lives.
func (r *credentialEnvResolver) Resolve(ctx context.Context, aspectID, kind string) (map[string]string, bool, error) {
	if r == nil || r.store == nil {
		return nil, false, nil
	}
	shape := credentialShapeForProvider(r.provider)
	if shape == "" {
		return nil, false, nil
	}
	_, env, err := r.store.ResolveDefaultForAspect(ctx, aspectID, shape)
	if err != nil {
		if errors.Is(err, credentials.ErrNoDefault) {
			// Aspect didn't configure a default; provider keeps its own
			// auth (subscription claudecode, process-env keys).
			return nil, false, nil
		}
		return nil, false, err
	}
	return env, true, nil
}

// credentialShapeForProvider maps a bridle ProviderID to the
// credential api_shape that provider's wire format speaks. Returns
// empty string for providers that don't take credential-store auth
// (ollama-local, etc) — caller treats empty as "skip resolution."
func credentialShapeForProvider(p bridle.ProviderID) credentials.APIShape {
	switch p {
	case "claude-code", "claude-api", "bedrock":
		return credentials.ShapeAnthropic
	case "openai-api", "openai":
		return credentials.ShapeOpenAI
	default:
		return ""
	}
}
