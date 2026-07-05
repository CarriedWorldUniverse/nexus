package cfgreconcile

import (
	"context"
	"encoding/json"
	"log/slog"
)

// FrontierAuthPath is the almanac key for the frontier (claude-code) OAuth
// SecureParameter delivery pointer (PHASE2-DESIGN §6, §7 build spec Part B).
// Its value is a JSON doc {"secret_name":"...","secret_key":"..."} naming
// the k8s Secret (and key within it) that actually carries
// CLAUDE_CODE_OAUTH_TOKEN. almanac is the SOURCE OF TRUTH for WHICH secret to
// trust; the Secret itself remains the only thing ever mounted/env-injected
// into a dispatch Job — see runtime/dispatch.FrontierAuthConfig.
const FrontierAuthPath = "cwb/nexus/frontier-auth"

// frontierAuthDoc is the almanac document shape for FrontierAuthPath.
type frontierAuthDoc struct {
	SecretName string `json:"secret_name"`
	SecretKey  string `json:"secret_key"`
}

// FrontierAuthSetter is the narrow surface on the live frontier-auth secret
// pointer. Satisfied by *runtime/dispatch.FrontierAuthConfig.
type FrontierAuthSetter interface {
	Set(name, key string) (changed bool)
}

// FrontierAuth reconciles the frontier-auth secret pointer (§6) from almanac
// into the live dispatch config. almanac dark, the key absent, or a
// malformed doc are all a clean no-op — dispatch.FrontierAuthConfig keeps
// its claude-oauth default (see that type's doc for why this is safe).
type FrontierAuth struct {
	r      Reader
	setter FrontierAuthSetter
	log    *slog.Logger
}

// NewFrontierAuth builds the frontier-auth reconciler.
func NewFrontierAuth(r Reader, setter FrontierAuthSetter, log *slog.Logger) *FrontierAuth {
	return &FrontierAuth{r: r, setter: setter, log: log}
}

func (*FrontierAuth) Name() string { return "frontier-auth" }

// ReconcileOnce reads FrontierAuthPath and write-throughs any change into the
// live pointer. No doc / absent key → no-op (pointer kept). An almanac read
// error aborts the pass (pointer kept at last-known — boots-standalone).
func (rc *FrontierAuth) ReconcileOnce(ctx context.Context) (int, error) {
	raw, ok, err := rc.r.Value(ctx, FrontierAuthPath)
	if err != nil {
		return 0, err
	}
	if !ok || raw == "" {
		return 0, nil // almanac doesn't manage it yet → keep the default
	}
	var d frontierAuthDoc
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		rc.log.Warn("cfgreconcile: skip malformed frontier-auth doc", "err", err)
		return 0, nil
	}
	if rc.setter.Set(d.SecretName, d.SecretKey) {
		rc.log.Info("cfgreconcile: frontier-auth secret pointer synced from almanac",
			"secret_name", d.SecretName, "secret_key", d.SecretKey)
		return 1, nil
	}
	return 0, nil
}
