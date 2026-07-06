package dispatch

import "sync"

// DefaultFrontierAuthSecretName / DefaultFrontierAuthSecretKey are the M0.3
// claude-oauth Secret coordinates (nexus + croft namespaces — see the §7
// build spec, "the claude-oauth k8s secret already exists"), carrying
// CLAUDE_CODE_OAUTH_TOKEN. This is the fallback FrontierAuthConfig starts at,
// and the value it stays at whenever almanac is dark or hasn't overridden it
// (PHASE2-DESIGN §6: "almanac (source of truth) -> k8s secret as delivery ->
// CLAUDE_CODE_OAUTH_TOKEN in every frontier seat" — the secret IS the
// delivery mechanism either way).
const (
	DefaultFrontierAuthSecretName = "claude-oauth"
	DefaultFrontierAuthSecretKey  = "CLAUDE_CODE_OAUTH_TOKEN"
)

// FrontierAuthConfig is the live, concurrency-safe pointer to the k8s Secret
// (name, key) that JobConfig.FrontierAuthFunc reads at every dispatch to
// inject CLAUDE_CODE_OAUTH_TOKEN into claude-code-provider Jobs (§6).
//
// It starts at the M0.3 defaults above (today's only known secret) and can be
// redirected live by nexus/cfgreconcile.FrontierAuth, which reconciles it
// from almanac's SecureParameter on the existing INC-4a/4b poll loop — same
// pattern as cfgreconcile.WakePolicy driving the broker's live wake-policy
// map. almanac dark (ALMANAC_GRPC_ADDR unset) → FrontierAuthConfig never
// changes from its construction-time defaults, so every claude-code dispatch
// still gets the M0.3 secret with zero extra wiring — that's the "fall back
// to the k8s secret" path from the §7 build spec.
type FrontierAuthConfig struct {
	mu   sync.RWMutex
	name string
	key  string
}

// NewFrontierAuthConfig starts a FrontierAuthConfig at the M0.3 defaults.
func NewFrontierAuthConfig() *FrontierAuthConfig {
	return &FrontierAuthConfig{name: DefaultFrontierAuthSecretName, key: DefaultFrontierAuthSecretKey}
}

// Get returns the current secret name+key — read fresh on every BuildJob
// call via JobConfig.FrontierAuthFunc (never cached at JobConfig-construction
// time), so an almanac-driven Set takes effect on the very next dispatch.
func (f *FrontierAuthConfig) Get() (name, key string) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.name, f.key
}

// Set overrides the secret coordinates (the almanac reconcile write-through
// path — see cfgreconcile.FrontierAuth). An empty name is a no-op: clearing
// an almanac override means removing/blanking the almanac key, which
// naturally stops future reconcile passes from calling Set again, NOT
// regressing FrontierAuthConfig to "no secret" (that would silently break
// every frontier dispatch on a single malformed almanac write). An empty key
// alongside a non-empty name falls back to the default key name. Reports
// whether anything actually changed, mirroring cfgreconcile.WakePolicySetter's
// contract.
func (f *FrontierAuthConfig) Set(name, key string) (changed bool) {
	if name == "" {
		return false
	}
	if key == "" {
		key = DefaultFrontierAuthSecretKey
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.name == name && f.key == key {
		return false
	}
	f.name, f.key = name, key
	return true
}
