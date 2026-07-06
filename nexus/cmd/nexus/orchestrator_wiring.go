package main

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/docregister"
	"github.com/CarriedWorldUniverse/nexus/nexus/orchestrator"
	"github.com/CarriedWorldUniverse/nexus/nexus/workgraph"
	"google.golang.org/grpc"
)

// li1-orchestrator-wiring: this file wires M1's tested-in-isolation
// orchestrator/workgraph/docregister packages into a running broker
// (build-specs/li1-orchestrator-wiring.md). Every subsystem here is
// env-gated and fail-soft: absent env, none of it is constructed and the
// broker behaves exactly as it did before this file existed. A
// construction error (bad ledger addr, missing cert, disabled flag) is
// logged and the subsystem is skipped — the broker still serves
// chat/dispatch. See README.md ("Live-integration wiring") for the full
// env-var reference and the live-verify path.

// buildDocRegister constructs the M1 Unit 2 document register from the
// environment. Dark by default: DOCREGISTER_ENABLE=1 (or a non-empty
// DOCREGISTER_CAIRN_DIR) is required, and DOCREGISTER_CAIRN_DIR must name a
// git working-copy directory the process can commit into (see
// docregister.GitCairnContent). Any missing prerequisite logs and returns
// nil — Config.DocRegister stays nil, endpoints dormant, exactly as before
// this wiring existed.
//
// Env:
//
//	DOCREGISTER_ENABLE=1       explicit opt-in (also implied by CAIRN_DIR below)
//	DOCREGISTER_CAIRN_DIR      git working-copy dir the register commits doc MD into. Required.
//	DOCREGISTER_CAIRN_AUTHOR   optional "Name <email>" for the git commit --author
func buildDocRegister(logger *slog.Logger, db *sql.DB) *docregister.Register {
	cairnDir := os.Getenv("DOCREGISTER_CAIRN_DIR")
	enabled := os.Getenv("DOCREGISTER_ENABLE") == "1" || cairnDir != ""
	if !enabled {
		return nil
	}
	if cairnDir == "" {
		logger.Warn("docregister DISABLED — DOCREGISTER_ENABLE=1 set but DOCREGISTER_CAIRN_DIR is empty")
		return nil
	}
	if info, err := os.Stat(cairnDir); err != nil || !info.IsDir() {
		logger.Warn("docregister DISABLED — DOCREGISTER_CAIRN_DIR not a directory", "dir", cairnDir, "err", err)
		return nil
	}
	reg := &docregister.Register{
		Store: docregister.NewSQLStore(db),
		Content: &docregister.GitCairnContent{
			RepoDir: cairnDir,
			Author:  os.Getenv("DOCREGISTER_CAIRN_AUTHOR"),
		},
	}
	logger.Info("docregister ENABLED", "cairn_dir", cairnDir)
	return reg
}

// buildWorkgraphClient dials the sovereign ledger and constructs a
// workgraph.Client when WORKGRAPH_LEDGER_ADDR is set. Dark by default —
// unset means the orchestrator is never started (there's nothing for it to
// drain). Any dial/config failure logs and returns nil so the broker still
// boots and serves chat/dispatch without the orchestrator.
//
// Env:
//
//	WORKGRAPH_LEDGER_ADDR   sovereign ledger gRPC address. Unset → orchestrator not started.
//	WORKGRAPH_ORG           cwb-org presented to the ledger (default workgraph.DefaultOrg)
//	WORKGRAPH_SUBJECT       cwb-subject presented to the ledger (default "nexus-orchestrator")
//	WORKGRAPH_PROJECT       ledger project key work items live under (default workgraph.DefaultProject)
//	WORKGRAPH_TLS_CERT/_KEY/_CA   mTLS material (see workgraph.DialCreds)
//	WORKGRAPH_DEV_INSECURE=1     dial without mTLS (local dev only)
func buildWorkgraphClient(logger *slog.Logger) *workgraph.Client {
	addr := os.Getenv("WORKGRAPH_LEDGER_ADDR")
	if addr == "" {
		return nil
	}
	dialCreds, err := workgraph.DialCreds()
	if err != nil {
		logger.Warn("orchestrator DISABLED — workgraph TLS config error", "err", err)
		return nil
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(dialCreds))
	if err != nil {
		logger.Warn("orchestrator DISABLED — workgraph dial failed", "addr", addr, "err", err)
		return nil
	}
	org := envOrDefault("WORKGRAPH_ORG", workgraph.DefaultOrg)
	subject := envOrDefault("WORKGRAPH_SUBJECT", "nexus-orchestrator")
	project := envOrDefault("WORKGRAPH_PROJECT", workgraph.DefaultProject)
	client := workgraph.New(conn, org, subject, project)
	// Best-effort: idempotently ensure the org/project/workflow this
	// orchestrator's work items live under. A failure here (e.g. the
	// ledger isn't reachable yet, or the org already exists with a
	// conflicting owner) is logged but non-fatal — DrainOnce's own
	// ListReady/Transition calls will surface the same error loudly and
	// repeatedly if the project truly isn't usable, which is a clearer
	// signal than failing broker boot over it. Bounded so an unreachable
	// ledger can never hang broker startup.
	ensureCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.EnsureProject(ensureCtx); err != nil {
		logger.Warn("workgraph: EnsureProject failed (continuing — orchestrator drain may error until this resolves)",
			"org", org, "project", project, "err", err)
	}
	logger.Info("workgraph client ENABLED", "addr", addr, "org", org, "project", project)
	return client
}

// buildOrchestrator constructs the M1 Unit 6 standing orchestrator when
// ORCHESTRATOR_ENABLE=1 AND a workgraph client is available (buildWorkgraphClient
// returned non-nil). Fail-soft: any missing prerequisite returns nil and the
// caller skips wiring OnJobDoneHook / starting the drain loop entirely — the
// broker still serves chat/dispatch, just without the orchestrator.
//
// Env:
//
//	ORCHESTRATOR_ENABLE=1        explicit opt-in. Unset/not "1" → orchestrator not started.
//	ORCHESTRATOR_ROLES           comma-separated role labels the pool serves
//	                             (default "builder,builder-complex,tester,reviewer,security-reviewer")
//	ORCHESTRATOR_STALE_AFTER     ReapStale heartbeat-staleness threshold, a
//	                             time.ParseDuration string (default: orchestrator's
//	                             own 5m default; invalid values are ignored with a warning)
//	ORCHESTRATOR_ROLE_BRAINS     role->brain overrides (role-tier-brains, 2026-07-06;
//	                             reasoning-EFFORT knob added 2026-07-06): comma-separated
//	                             "role=provider:model" or "role=provider:model:effort"
//	                             entries, e.g. "builder-complex=claude-code:claude-sonnet-4-6"
//	                             or "builder-complex=claude-api:claude-opus-4-6:high" — see
//	                             parseRoleBrains. The effort field is optional (2-field
//	                             "role=provider:model" back-compat kept) and must be one of
//	                             low|medium|high; a bad value is warned and ignored (the
//	                             provider/model override still applies). Unset/empty →
//	                             no overrides, every role dispatches with the leased
//	                             personality's own aspects-row provider/model (unchanged
//	                             pre-role-tier-brains behavior).
func buildOrchestrator(logger *slog.Logger, wg *workgraph.Client, dispatcher orchestrator.Dispatcher, workerStatus orchestrator.WorkerStatusStore) *orchestrator.Orchestrator {
	if os.Getenv("ORCHESTRATOR_ENABLE") != "1" {
		return nil
	}
	if wg == nil {
		logger.Warn("orchestrator DISABLED — ORCHESTRATOR_ENABLE=1 but no workgraph client (set WORKGRAPH_LEDGER_ADDR)")
		return nil
	}
	if dispatcher == nil {
		logger.Warn("orchestrator DISABLED — ORCHESTRATOR_ENABLE=1 but dispatch Runner is not wired (no in-cluster k8s client)")
		return nil
	}
	roles := parseCSVOrDefault(os.Getenv("ORCHESTRATOR_ROLES"),
		[]string{"builder", "builder-complex", "tester", "reviewer", "security-reviewer"})
	orch := &orchestrator.Orchestrator{
		Graph:        wg,
		Dispatcher:   dispatcher,
		WorkerStatus: workerStatus,
		Alerter:      orchestrator.LogAlerter{Log: logger},
		Roles:        roles,
	}
	if v := os.Getenv("ORCHESTRATOR_STALE_AFTER"); v != "" {
		if d, derr := time.ParseDuration(v); derr == nil && d > 0 {
			orch.StaleAfter = d
		} else {
			logger.Warn("ORCHESTRATOR_STALE_AFTER invalid — using orchestrator default", "value", v, "err", derr)
		}
	}
	// Role-brain overrides (role-tier-brains): a Resolver is wired only when
	// at least one entry parses — an empty map would make o.resolve's
	// (via RoleBrainResolver) return ("","") for every role anyway, but
	// leaving Resolver nil in that case keeps DrainOnce's "no Resolver"
	// zero-overhead path exactly as it was before this feature, matching
	// how every other optional wiring in this file behaves (see
	// ensurePoolPersonalities/HandProvider being fully opt-in above).
	if brains := parseRoleBrains(os.Getenv("ORCHESTRATOR_ROLE_BRAINS"), logger); len(brains) > 0 {
		orch.Resolver = orchestrator.RoleBrainResolver{Brains: brains}
		logger.Info("orchestrator: role brains configured", "brains", brains)
	}
	logger.Info("orchestrator ENABLED", "roles", roles)
	return orch
}

// parseRoleBrains parses ORCHESTRATOR_ROLE_BRAINS, an operator-configured
// role->brain override list: comma-separated "role=provider:model" or
// "role=provider:model:effort" entries (e.g.
// "builder-complex=claude-code:claude-sonnet-4-6" or
// "builder-complex=claude-api:claude-opus-4-6:high"). A malformed entry
// (missing "=", missing ":", or an empty role/provider) is skipped with a
// warning rather than aborting the whole parse — one bad entry never takes
// down every other role's override. Model may be empty ("role=provider:" or
// "role=provider") — that role then overrides only the provider, leaving the
// leased personality's own model (or the provider's own default) in place.
// The effort field (reasoning-EFFORT knob, 2026-07-06) is optional — a
// plain 2-field "role=provider:model" entry is unaffected (back-compat) —
// and must be one of low|medium|high (validEffort); an unrecognized value
// is warned and ignored (dropped, NOT the whole entry — the provider/model
// override still applies) rather than aborting the parse.
// Unset/empty input returns an empty map: every role then resolves
// ("","","") for provider/model/effort via RoleBrainResolver, identical to
// no Resolver being wired at all (see buildOrchestrator). Deliberately no
// built-in default role->brain mapping is hardcoded here — the env is the
// only source, per the build decision that this unit ships mechanism, not a
// specific model pin (see docs/network/ROLE-MODEL.md "Role tiers" for a
// worked example).
func parseRoleBrains(raw string, logger *slog.Logger) map[string]orchestrator.RoleBrain {
	brains := map[string]orchestrator.RoleBrain{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return brains
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		role, spec, ok := strings.Cut(entry, "=")
		role, spec = strings.TrimSpace(role), strings.TrimSpace(spec)
		if !ok || role == "" || spec == "" {
			logger.Warn("ORCHESTRATOR_ROLE_BRAINS: skipping malformed entry (want role=provider:model[:effort])", "entry", entry)
			continue
		}
		provider, rest, hasRest := strings.Cut(spec, ":")
		provider = strings.TrimSpace(provider)
		var model, effort string
		if hasRest {
			if m, e, hasEffort := strings.Cut(rest, ":"); hasEffort {
				model, effort = strings.TrimSpace(m), strings.TrimSpace(e)
			} else {
				model = strings.TrimSpace(rest)
			}
		}
		if provider == "" {
			logger.Warn("ORCHESTRATOR_ROLE_BRAINS: skipping malformed entry (want role=provider:model[:effort])", "entry", entry)
			continue
		}
		if !isKnownWorkerRole(role) {
			logger.Warn("ORCHESTRATOR_ROLE_BRAINS: role is not a registered pool role — configuring it anyway (it will simply never be leased)", "role", role)
		}
		if effort != "" && !validEffort(effort) {
			logger.Warn("ORCHESTRATOR_ROLE_BRAINS: unrecognized effort value — ignoring effort, keeping provider/model override (want low|medium|high)", "entry", entry, "effort", effort)
			effort = ""
		}
		brains[role] = orchestrator.RoleBrain{Provider: provider, Model: model, Effort: effort}
	}
	return brains
}

// validEffort reports whether s is one of the reasoning-EFFORT knob's
// recognized values (low|medium|high) — see
// runtime/cmd/agentfunnel/main.go's effortToBudgetTokens for the table
// each maps to.
func validEffort(s string) bool {
	switch s {
	case "low", "medium", "high":
		return true
	default:
		return false
	}
}

// orchestratorDrainInterval reads ORCHESTRATOR_DRAIN_INTERVAL (a
// time.ParseDuration string), defaulting to 30s when unset or invalid.
func orchestratorDrainInterval(logger *slog.Logger) time.Duration {
	const defaultInterval = 30 * time.Second
	v := os.Getenv("ORCHESTRATOR_DRAIN_INTERVAL")
	if v == "" {
		return defaultInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		logger.Warn("ORCHESTRATOR_DRAIN_INTERVAL invalid — using default", "value", v, "default", defaultInterval, "err", err)
		return defaultInterval
	}
	return d
}

// drainer is the narrow slice of *orchestrator.Orchestrator's API the drain
// loop needs — narrowed to an interface so tests can supply a fake instead
// of a live orchestrator wired to a real workgraph/dispatch pair.
// *orchestrator.Orchestrator satisfies this structurally via DrainOnce.
type drainer interface {
	DrainOnce(ctx context.Context) (orchestrator.DrainReport, error)
}

// runDrainLoop is the orchestrator's cadence-driven wake trigger
// (PHASE2-DESIGN §2 "wake triggers: ticker cadence — the fallback safety
// net"). It calls o.DrainOnce every interval until ctx is cancelled, logging
// each pass's DrainReport summary. The OnJobDone event wake (wired
// separately via orch.OnJobDoneHook()) covers the fast, event-triggered
// path; this ticker is the belt-and-suspenders sweep that still runs work
// even if a JobDone event is ever missed.
func runDrainLoop(ctx context.Context, o drainer, interval time.Duration, logger *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			report, err := o.DrainOnce(ctx)
			if err != nil {
				logger.Error("orchestrator: drain pass failed", "err", err)
				continue
			}
			logger.Info("orchestrator: drain pass",
				"dispatched", report.Dispatched,
				"skipped", report.Skipped,
				"reaped", report.Reaped,
				"held", report.Held,
				"hold_reason", report.HoldReason,
				"errors", report.Errors,
			)
		}
	}
}

// parseCSVOrDefault splits a comma-separated list, trimming whitespace and
// dropping empty entries; an empty/whitespace-only input returns def
// unchanged (the caller's built-in default), never an empty slice.
func parseCSVOrDefault(raw string, def []string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// ensurePoolPersonalities self-provisions the pool's personality aspects and
// their shared provider (Ornith) credential. A pool worker is named
// `<personality>-<role>` (aspects/lineage.go) and resolves its persona/config/
// credentials to the PERSONALITY (aspects.PersonalityOf) — so the personality
// must be a real, non-retired aspects-store row carrying the provider default.
// The personalities are ordinary single-word aspects lent to the pool; on a
// fresh store they are inserted, and every boot their default provider
// credential is (re)pointed at the shared Ornith brain. Idempotent.
//
// Env:
//
//	POOL_PERSONALITIES     comma-separated personality roster (default aspects.WorkerPersonalities;
//	                       `shadow` is never a worker — it is the orchestrator)
//	POOL_PROVIDER/_MODEL   the shared worker brain (default openai-shape / "ornith")
//	POOL_PROVIDER_BASE_URL the Ornith endpoint; when unset, no credential is
//	                       provisioned and workers inherit their process env
//	POOL_PROVIDER_KEY      the provider key (default "dummy" — Ornith is keyless)
func ensurePoolPersonalities(ctx context.Context, store aspects.Store, credStore *credentials.Store, logger *slog.Logger) {
	if store == nil {
		return
	}
	personalities := parseCSVOrDefault(os.Getenv("POOL_PERSONALITIES"), aspects.WorkerPersonalities)
	provider := envOr("POOL_PROVIDER", "openai")
	model := envOr("POOL_MODEL", "ornith")

	// The shared worker-brain credential (Ornith). One credential, allowed
	// for every personality, set as each personality's default below. Only
	// when POOL_PROVIDER_BASE_URL is set (else workers inherit process env).
	baseURL := os.Getenv("POOL_PROVIDER_BASE_URL")
	shape := "openai"
	if provider == "claude-api" || provider == "anthropic" || provider == "claude" {
		shape = "anthropic"
	}
	const credName = "ornith-provider"
	credReady := false
	if credStore != nil && baseURL != "" {
		if err := credStore.Set(ctx, credentials.UpsertParams{
			Name:           credName,
			Description:    "shared pool-worker brain (self-provisioned at boot)",
			Kind:           credentials.KindProvider,
			Bundle:         map[string]any{"api_shape": shape, "base_url": baseURL, "key": envOr("POOL_PROVIDER_KEY", "dummy"), "default_model": model},
			AllowedAspects: []string{"*"},
			Mode:           credentials.ModeFetch,
		}); err != nil {
			logger.Warn("orchestrator: ensurePoolPersonalities: upsert shared provider credential failed", "err", err)
		} else {
			credReady = true
		}
	}

	provisioned := make([]string, 0, len(personalities))
	for _, p := range personalities {
		if a, err := store.Get(ctx, p); err != nil || a == nil || a.Status == aspects.StatusRetired {
			if err := store.Insert(ctx, aspects.Aspect{
				Name: p, Status: aspects.StatusActive, Provider: provider, Model: model,
			}); err != nil {
				logger.Warn("orchestrator: ensurePoolPersonalities: insert personality aspect failed", "personality", p, "err", err)
				continue
			}
			logger.Info("orchestrator: provisioned personality aspect row", "personality", p, "provider", provider, "model", model)
		}
		if credReady {
			if err := credStore.SetAspectDefault(ctx, p, shape, credName); err != nil {
				logger.Warn("orchestrator: ensurePoolPersonalities: set personality default provider credential failed", "personality", p, "err", err)
				continue
			}
		}
		provisioned = append(provisioned, p)
	}
	logger.Info("orchestrator: ensured pool personalities", "personalities", provisioned, "shape", shape, "base_url", baseURL, "model", model, "cred", credReady)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
