package dispatch

import (
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// acceptanceGateEnvKeys are the ACCEPTANCE-GATE-HARDENING knobs the worker
// (agentfunnel) reads — the gate runs in the worker, not the broker, and a
// k8s Job does not inherit the broker pod's env, so BuildJob forwards any
// that are set on the broker. Unset = the worker's code defaults (U2 on at
// floor 1, U1 judge-the-diff on, U3 test-evidence off). This makes the gate
// operator-tunable via the broker deployment env, no per-knob code change.
var acceptanceGateEnvKeys = []string{
	"ACCEPTANCE_MIN_DIFF_LINES",    // Unit 2 floor (default 1; 0 disables)
	"ACCEPTANCE_JUDGE_DIFF",        // Unit 1 (default on; 0 disables)
	"ACCEPTANCE_REQUIRE_TEST_DIFF", // Unit 3 (default off; 1 enables)
	// CW_VCS rides the same broker-env→worker seam: set CW_VCS=cairn on the
	// broker deployment to flip dispatched builders onto the cairn
	// clone-per-run path (BUILDER-CAIRN-MIGRATION.md); unset = git default.
	"CW_VCS",
	// CW_PULL_* configures the cairn pull-checks recorder (runtime/pullchecks)
	// — the builder gates' verdicts recorded as cairn-server pull checks.
	// Dark by default: CW_PULL_SERVER_ADDR unset means the recorder is never
	// built and the gate path makes zero PullService calls. See
	// docs/network/ACCEPTANCE-GATE-HARDENING.md.
	"CW_PULL_SERVER_ADDR",
	"CW_PULL_ORG",
	"CW_PULL_SLUG",
	"CW_PULL_PROJECT",
	// CW_PULL_TARGET overrides the pull-checks target-branch resolution
	// (default: the repo's actual default branch via `gh repo view`, falling
	// back to "main") — set it when a repo's default branch can't be
	// resolved that way or the operator wants a fixed target regardless.
	"CW_PULL_TARGET",
	"CW_PULL_TLS_CERT",
	"CW_PULL_TLS_KEY",
	"CW_PULL_TLS_CA",
	"CW_PULL_DEV_INSECURE",
}

type JobConfig struct {
	Image         string
	Namespace     string
	NodeIP        string
	BrokerHost    string
	BriefTimeout  string
	IdleTimeout   string
	GitCredName   string
	ActivityDir   string
	LynxAIBaseURL string
	LynxAIKey     string
	// BrokerCAFile, when non-empty, is the IN-POD path at which the
	// broker's TLS CA is mounted for hand (spawn) Jobs. A hand has no
	// keyfile to pin the broker cert from (ticket builders carry it inside
	// aspect-keyfile-<agent>), so the CA is delivered as a Secret volume
	// and CW_SEAM_CA points agentfunnel's BrokerTLSConfigFromCAFile at it.
	// Empty → no CW_SEAM_CA injected → system trust (the CA-signed / LE
	// broker default). Set from the broker's own cert path in cmd/nexus.
	BrokerCAFile string
	// NexusID is the broker's nexus id, injected as CW_NEXUS_ID on hand
	// Jobs for log correlation / WS-dial parity (informational — a hand's
	// identity is already proven by its session JWT). Empty → not injected.
	NexusID string
	// OllamaBaseURL / OllamaKeepAlive propagate the ollama provider
	// endpoint into Jobs whose provider is ollama-flavoured (NEX-610).
	// An ollama-provider parent's deployment carries OLLAMA_BASE_URL
	// (e.g. the in-cluster gemma service); its hands run the same
	// provider but a fresh Job spec, which without these dials the
	// agentfunnel default localhost:11434 and fails. Set from cmd/nexus
	// env (CW_OLLAMA_BASE_URL / CW_OLLAMA_KEEP_ALIVE, falling back to
	// the broker's own OLLAMA_* env), same seam as BrokerCAFile/NexusID.
	// Empty → not injected → agentfunnel default.
	OllamaBaseURL   string
	OllamaKeepAlive string

	// ImageTagPin, when non-nil, is called once per BuildJob invocation to
	// resolve the §7 CLI-version knob (PHASE2-DESIGN §7): a full image ref
	// (e.g. "localhost/nexus-runner:cli-2.1.3") that REPLACES cfg.Image for
	// this dispatch. Return "" for "no pin" — cfg.Image (the boot-configured
	// default, "latest built") is used unchanged. Called at BUILD time (not
	// read once at JobConfig-construction), so an operator pin/clear takes
	// effect on the next dispatch without a broker restart, given a
	// live-backed resolver — see cmd/nexus/main.go (CW_BUILDER_IMAGE_PIN_FILE)
	// and README "CLI version knob". nil = no knob = today's fixed cfg.Image
	// behavior, unchanged. PullNever (below) means the resolved tag must
	// already be pre-loaded on the node — see Part C (CI image rebuild).
	ImageTagPin func() string

	// FrontierAuthFunc, when non-nil, is called once per BuildJob invocation
	// to resolve the k8s Secret (name, key) delivering the frontier
	// (claude-code) OAuth token (PHASE2-DESIGN §6). Injected as
	// CLAUDE_CODE_OAUTH_TOKEN on claude-code-provider dispatch Jobs only.
	// almanac is the source of truth for WHICH secret to trust (see
	// nexus/cfgreconcile.FrontierAuth / runtime/dispatch.FrontierAuthConfig);
	// the Secret itself is ALWAYS the actual delivery mechanism into the Job
	// env — almanac only redirects the pointer. Returning ("", _) or a nil
	// FrontierAuthFunc means no injection (today's behavior for anyone who
	// hasn't wired this — e.g. every existing test's zero-value JobConfig).
	FrontierAuthFunc func() (secretName, secretKey string)
}

const (
	// brokerCAVolumeName / brokerCAMountDir deliver the broker's TLS CA to
	// hand Jobs via the cluster Secret nexus-broker-ca (provisioned
	// out-of-band, like aspect-keyfile-<agent> and codex-auth). The CA file
	// lands at brokerCAMountDir/<key>; cmd/nexus sets JobConfig.BrokerCAFile
	// to that full in-pod path and CW_SEAM_CA mirrors it.
	brokerCAVolumeName = "broker-ca"
	brokerCASecretName = "nexus-broker-ca"
	brokerCAMountDir   = "/etc/nexus-ca"
	brokerCASecretKey  = "ca.crt"
)

// HandBrokerCAPath is the in-pod path at which a hand Job finds the broker
// TLS CA (the nexus-broker-ca Secret mounted at brokerCAMountDir). Set
// JobConfig.BrokerCAFile to this when the broker uses a self-signed /
// internal-CA cert so hands can pin it via CW_SEAM_CA.
func HandBrokerCAPath() string { return brokerCAMountDir + "/" + brokerCASecretKey }

// HandBrokerCASecretName is the cluster Secret (provisioned out-of-band,
// like aspect-keyfile-<agent>) whose ca.crt key holds the broker TLS CA
// delivered to hand Jobs.
func HandBrokerCASecretName() string { return brokerCASecretName }

func int32p(v int32) *int32   { return &v }
func int64ptr(v int64) *int64 { return &v }

const (
	builderJobTTLSeconds = 5 * 60
	maxJobNameLength     = 63
	runSuffixHexLength   = 20
	maxRunSuffixLength   = 24
	homePVCNamePrefix    = "aspect-home-"
	homeRepoMountPath    = "/var/lib/nexus/home.git"
	homeWorkMountPath    = "/agent-home"
	sharedReposPVCName   = "nexus-builder-repos"
	sharedReposMountPath = "/src"
)

// BuildJob mirrors deploy/worker/job.yaml for one dispatch brief.
func BuildJob(b Brief, cfg JobConfig, taskID string, provider string) *batchv1.Job {
	if cfg.BriefTimeout == "" {
		cfg.BriefTimeout = "30m"
	}
	if cfg.IdleTimeout == "" {
		cfg.IdleTimeout = "2m"
	}
	if provider == "" {
		provider = "codex-cli"
	}
	// Provider aliases mirror broker.supportedProviders (NEX-610): the
	// aspects.provider column holds whatever the operator set ("codex",
	// "ollama-local", "agy", …), and hand briefs inherit that raw value
	// via Runner.HandProvider — matching only the canonical id silently
	// dropped the codex-auth mount for provider="codex" parents.
	codexProvider := provider == "codex-cli" || provider == "codex" || provider == "codexcli"
	antigravityProvider := provider == "antigravity-cli" || provider == "antigravity" || provider == "agy"
	ollamaProvider := provider == "ollama" || provider == "ollama-local"
	// claude-code is the frontier CLI: it self-authenticates via
	// CLAUDE_CODE_OAUTH_TOKEN (from `claude setup-token`), never a keyfile
	// env-var credential like codex/antigravity's mounted secrets — see
	// the FrontierAuthFunc injection below (§6).
	claudeCodeProvider := provider == "claude-code" || provider == "claudecode" ||
		provider == "claude" || provider == "claude-api"
	// §7 CLI-version knob: ImageTagPin overrides cfg.Image for this dispatch
	// when set to a non-empty value; nil/empty = cfg.Image unchanged (the
	// boot-configured "latest built" default).
	image := cfg.Image
	if cfg.ImageTagPin != nil {
		if pin := cfg.ImageTagPin(); pin != "" {
			image = pin
		}
	}
	// The Job runs AS the named agent: keyfile = aspect-keyfile-<agent>, and
	// the Job name + labels carry the agent + run id. A hand brief
	// (SpawnParent set, NEX-571) runs as the DERIVED identity instead:
	// no keyfile exists for it, so the broker-minted session JWT is
	// injected as env in place of the keyfile volume, and the lineage
	// label lets observers group hands under their base aspect.
	spawn := b.SpawnParent != ""
	keyfileAspect := b.Agent
	labels := map[string]string{
		"app":                   "nexus-builder",
		"nexus.dispatch/agent":  b.Agent,
		"nexus.dispatch/ticket": b.Ticket,
		"nexus.dispatch/run-id": b.RunID,
	}
	if spawn {
		labels["nexus.dispatch/lineage"] = dnsLabelPart(b.SpawnParent)
	}
	if b.WorkItemID != "" {
		labels["nexus.dispatch/work-item"] = dnsLabelPart(b.WorkItemID)
	}
	if b.Personality != "" {
		labels["nexus.dispatch/personality"] = dnsLabelPart(b.Personality)
	}
	annotations := map[string]string{}
	if b.Thread != "" {
		annotations["nexus.dispatch/thread"] = b.Thread
	}
	env := []corev1.EnvVar{
		{Name: "CW_SEAM_URL", Value: "https://" + cfg.BrokerHost + ":7888"},
		{Name: "GOCACHE", Value: "/cache/go"},
		{Name: "CW_DISPATCH_RUN_ID", Value: b.RunID},
		{Name: "CW_DISPATCH_PARENT_RUN_ID", Value: b.ParentRunID},
		{Name: "CW_IDLE_TIMEOUT", Value: cfg.IdleTimeout},
		// CW_IMAGE_TAG carries this Job's own (possibly §7-pinned) image ref
		// into the pod so the M1 Unit 5 worker.status heartbeat can report
		// `image_tag` without re-deriving it — this is the resolved `image`
		// (cfg.Image, or ImageTagPin's override when set), closing the loop
		// described in the §7 build spec.
		{Name: "CW_IMAGE_TAG", Value: image},
		{Name: "CW_AGENT_HOME_REPO", Value: homeRepoMountPath},
		{Name: "CW_AGENT_HOME_WORKDIR", Value: homeWorkMountPath},
		{Name: "CW_SHARED_REPOS_DIR", Value: sharedReposMountPath},
		// The codex-auth init container writes /root/.codex; the builder
		// entrypoint moves HOME to the per-agent home worktree, so pin
		// CODEX_HOME to the auth location, HOME-independent.
		{Name: "CODEX_HOME", Value: "/root/.codex"},
	}
	// Role-at-spawn metadata (M1 Unit 3). WorkItemID/Personality are
	// informational (log correlation / accountability, mirroring
	// CW_NEXUS_ID). SkillAllowlist scopes nexus-skills-mcp's
	// search_skills/get_skill surface to this spawn's role — the
	// skill-gating primitive (ROLE-MODEL §9). All empty by default →
	// no env injected → unchanged behavior.
	if b.Role != "" {
		// CW_ROLE carries the role LABEL (M1 Unit 4's pool leases stamp
		// this — see pool.go) into the worker process so the M1 Unit 5
		// worker.status heartbeat can populate its `role` field without
		// re-plumbing a new brief-file/flag path. Informational, same
		// posture as CW_WORK_ITEM_ID/CW_PERSONALITY below.
		env = append(env, corev1.EnvVar{Name: "CW_ROLE", Value: b.Role})
	}
	if b.WorkItemID != "" {
		env = append(env, corev1.EnvVar{Name: "CW_WORK_ITEM_ID", Value: b.WorkItemID})
	}
	if b.Personality != "" {
		env = append(env, corev1.EnvVar{Name: "CW_PERSONALITY", Value: b.Personality})
	}
	// CW_PROVIDER/CW_MODEL (role-tier-brains, 2026-07-06): a dispatch-time
	// Brief.Provider/Brief.Model override for the worker's boot-resolved
	// provider/model. agentfunnel prefers these over the broker resolve/
	// validate response's Provider/Model when present, falling back to the
	// resolve response otherwise (main.go) — the mechanism a role-brain
	// override (dispatch.PoolItem.Provider/Model, see pool.go) needs to
	// actually change what BRAIN the worker process talks to, not just
	// which auth the Job mounts (which `provider` — the BuildJob parameter,
	// already effective — governs). Empty = no override, exactly as before
	// these env vars existed.
	if b.Provider != "" {
		env = append(env, corev1.EnvVar{Name: "CW_PROVIDER", Value: b.Provider})
	}
	if b.Model != "" {
		env = append(env, corev1.EnvVar{Name: "CW_MODEL", Value: b.Model})
	}
	// CW_EFFORT (reasoning-EFFORT knob, 2026-07-06): a dispatch-time
	// Brief.Effort override — low|medium|high — for the worker's
	// extended-thinking budget on the claude-api provider path.
	// agentfunnel maps this via a fixed effort->budget_tokens table onto
	// bridle.TurnRequest.ThinkingBudgetTokens; every other provider (no
	// request-side thinking-budget knob) logs a one-line no-op note and
	// otherwise ignores it. Empty = no override, exactly as before this
	// env var existed.
	if b.Effort != "" {
		env = append(env, corev1.EnvVar{Name: "CW_EFFORT", Value: b.Effort})
	}
	if len(b.SkillAllowlist) > 0 {
		env = append(env, corev1.EnvVar{Name: "CW_SKILL_ALLOWLIST", Value: strings.Join(b.SkillAllowlist, ",")})
	}
	if spawn {
		// Derived hand credential (NEX-571 Task B/C): the broker-signed
		// session JWT for `<parent>.sub-N`, injected beside where ticket
		// dispatches mount aspect-keyfile-<agent>. Same trust domain as
		// the keyfile Secret (cluster-internal Job spec), per the locked
		// v1 env-injection decision; herald DeriveAgentKey replaces this
		// when herald-rooted boot lands.
		env = append(env,
			corev1.EnvVar{Name: "CW_SESSION_JWT", Value: b.SessionJWT},
			corev1.EnvVar{Name: "CW_ASPECT_NAME", Value: b.Agent},
			corev1.EnvVar{Name: "CW_SPAWN_PARENT", Value: b.SpawnParent},
		)
		// CW_SEAM_CA: pin the broker's self-signed/internal-CA cert so the
		// hand's /api/aspect/resolve over TLS verifies. The CA file is
		// delivered by the nexus-broker-ca Secret volume mounted below at
		// brokerCAMountDir; CW_SEAM_CA = its in-pod path. Empty BrokerCAFile
		// → omit → system trust (CA-signed / LE broker), unchanged behavior.
		if cfg.BrokerCAFile != "" {
			env = append(env, corev1.EnvVar{Name: "CW_SEAM_CA", Value: cfg.BrokerCAFile})
		}
		// CW_NEXUS_ID: informational (log correlation / WS-dial parity); the
		// hand's identity is proven by its JWT, so a miss is non-fatal.
		if cfg.NexusID != "" {
			env = append(env, corev1.EnvVar{Name: "CW_NEXUS_ID", Value: cfg.NexusID})
		}
	}
	// Ollama provider endpoint (NEX-610): without OLLAMA_BASE_URL the
	// agentfunnel in the Job dials localhost:11434 — dead inside a pod.
	// Injected for any ollama-provider Job (hand or ticket dispatch);
	// empty config values are omitted so non-ollama clusters see no
	// behaviour change.
	if ollamaProvider {
		if cfg.OllamaBaseURL != "" {
			env = append(env, corev1.EnvVar{Name: "OLLAMA_BASE_URL", Value: cfg.OllamaBaseURL})
		}
		if cfg.OllamaKeepAlive != "" {
			env = append(env, corev1.EnvVar{Name: "OLLAMA_KEEP_ALIVE", Value: cfg.OllamaKeepAlive})
		}
	}
	if cfg.LynxAIBaseURL != "" {
		env = append(env, corev1.EnvVar{Name: "LYNXAI_BASE_URL", Value: cfg.LynxAIBaseURL})
	}
	if cfg.LynxAIKey != "" {
		env = append(env, corev1.EnvVar{Name: "LYNXAI_KEY", Value: cfg.LynxAIKey})
	}
	// Frontier auth (§6): inject CLAUDE_CODE_OAUTH_TOKEN via secretKeyRef
	// for claude-code-provider dispatches only — the orchestrator drain Job
	// and reviewer/security pods (frontier seats) all dispatch with this
	// provider. Delivered via a k8s Secret either way (almanac, when
	// configured, only redirects WHICH secret/key — see FrontierAuthFunc
	// doc above). No FrontierAuthFunc / empty name → no injection, so a
	// zero-value JobConfig (every pre-unit-7 test/deployment) reproduces
	// today's exact env list.
	if claudeCodeProvider && cfg.FrontierAuthFunc != nil {
		if secretName, secretKey := cfg.FrontierAuthFunc(); secretName != "" && secretKey != "" {
			env = append(env, corev1.EnvVar{
				Name: "CLAUDE_CODE_OAUTH_TOKEN",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  secretKey,
					},
				},
			})
		}
	}
	// Sandbox marker for the claude-code CLI: the worker container runs as
	// root, and claude-code refuses `--dangerously-skip-permissions` under
	// root ("cannot be used with root/sudo privileges") — which is exactly
	// how the claudecode provider invokes it headless. IS_SANDBOX=1 tells
	// the CLI it is in a contained env and allows the bypass (confirmed live
	// 2026-07-06 in the pool worker: the same invocation that exit-1'd as
	// root replies normally with this set). The pod IS the sandbox: no
	// interactive user, ephemeral, network-scoped. Set for the claude-code
	// tier only — Ornith (openai provider) never spawns the CLI.
	if claudeCodeProvider {
		env = append(env, corev1.EnvVar{Name: "IS_SANDBOX", Value: "1"})
	}
	// Forward the acceptance-gate knobs from the broker's env to the worker
	// (see acceptanceGateEnvKeys) — only those explicitly set, so an unset
	// broker env leaves the worker on its safe code defaults.
	for _, k := range acceptanceGateEnvKeys {
		if v := os.Getenv(k); v != "" {
			env = append(env, corev1.EnvVar{Name: k, Value: v})
		}
	}
	volumeMounts := []corev1.VolumeMount{
		{Name: "work", MountPath: "/work"},
		{Name: "cache", MountPath: "/cache"},
		{Name: "home-repo", MountPath: homeRepoMountPath},
		{Name: "home-work", MountPath: homeWorkMountPath},
		{Name: "shared-repos", MountPath: sharedReposMountPath},
		// Brief ConfigMap mounts as its OWN directory — must NOT be a
		// file inside /etc/nexus (the keyfile Secret's mount point), or
		// the OCI runtime fails with "not a directory" (NEX-437).
		{Name: "brief", MountPath: "/etc/dispatch", ReadOnly: true},
	}
	volumes := []corev1.Volume{
		{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "cache", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "nexus-builder-work"}}},
		{Name: "home-repo", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: HomePVCName(b.Agent)}}},
		{Name: "home-work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "shared-repos", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: SharedReposPVCName()}}},
		{Name: "brief", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "brief-" + taskID}}}},
	}
	if !spawn {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "keyfile", MountPath: "/etc/nexus", ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: "keyfile", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "aspect-keyfile-" + keyfileAspect}}})
	}
	if spawn && cfg.BrokerCAFile != "" {
		// Hands have no keyfile-pinned broker cert; mount the broker CA so
		// CW_SEAM_CA (set above) resolves to a real file in the pod. The
		// nexus-broker-ca Secret is provisioned out-of-band, like the
		// keyfile Secret. Read-only — the hand only needs to read the PEM.
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: brokerCAVolumeName, MountPath: brokerCAMountDir, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: brokerCAVolumeName, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: brokerCASecretName}}})
	}
	var initContainers []corev1.Container
	if codexProvider {
		initContainers = append(initContainers, corev1.Container{
			Name:            "codex-auth",
			Image:           image,
			ImagePullPolicy: corev1.PullNever,
			Command:         []string{"sh", "-c", "mkdir -p /root/.codex && cp /codex-secret/auth.json /root/.codex/auth.json && chmod 600 /root/.codex/auth.json"},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "codex-home", MountPath: "/root/.codex"},
				{Name: "codex-secret", MountPath: "/codex-secret", ReadOnly: true},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "codex-home", MountPath: "/root/.codex"})
		volumes = append(volumes,
			corev1.Volume{Name: "codex-secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "codex-auth"}}},
			corev1.Volume{Name: "codex-home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		)
	}
	if antigravityProvider {
		// agy reads OAuth creds from $HOME/.gemini. The builder moves HOME to
		// the per-agent worktree at runtime, so we mount the secret read-only
		// here and the agentfunnel copies it into $HOME/.gemini after the move
		// (stageAntigravityCreds) — writable, so agy can refresh the token.
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "antigravity-secret", MountPath: "/antigravity-secret", ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: "antigravity-secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "antigravity-auth"}}})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        builderJobName(b.Agent, b.RunID, taskID),
			Namespace:   cfg.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32p(0),
			ActiveDeadlineSeconds:   int64ptr(activeDeadlineSeconds(cfg.BriefTimeout)),
			TTLSecondsAfterFinished: int32p(builderJobTTLSeconds),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					HostAliases: []corev1.HostAlias{{
						IP:        cfg.NodeIP,
						Hostnames: []string{cfg.BrokerHost},
					}},
					InitContainers: initContainers,
					Containers: []corev1.Container{{
						Name:            "builder",
						Image:           image,
						ImagePullPolicy: corev1.PullNever,
						Args:            builderArgs(b, cfg, spawn),
						Env:             env,
						VolumeMounts:    volumeMounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

// briefDir is the mount path of the "brief" ConfigMap volume (declared in
// BuildJob's volumeMounts). Role-at-spawn overlay files (role.md,
// policy.json) land here as extra keys of the SAME ConfigMap — the
// ConfigMap volume mounts every Data key as a file, so no new volume is
// needed. See briefConfigMapData in runner.go.
const (
	briefDir                = "/etc/dispatch"
	briefRoleFileName       = "role.md"
	briefPolicyFragmentName = "policy.json"
	briefAcceptanceFileName = "acceptance.md"
)

// builderArgs assembles the agentfunnel command line. Ticket dispatches
// authenticate with the mounted keyfile; hand briefs (spawn) carry no
// keyfile — their identity is the CW_SESSION_JWT env injected above.
func builderArgs(b Brief, cfg JobConfig, spawn bool) []string {
	var args []string
	if !spawn {
		args = append(args, "-k", "/etc/nexus/keyfile.json")
	}
	args = append(args,
		"-builder",
		"-brief-file", briefDir+"/brief.md",
		"-reply-topic", b.Thread,
		"-builder-timeout", cfg.BriefTimeout,
		"-builder-idle-timeout", cfg.IdleTimeout,
		"-repo", b.Repo,
		"-ticket", b.Ticket,
		"-branch", b.Branch,
	)
	// Role-at-spawn overlay (M1 Unit 3): only passed when the brief
	// carries them, so an empty RolePrompt/PolicyFragment reproduces
	// today's exact agentfunnel invocation (no -role-file/-policy-fragment-file).
	if b.RolePrompt != "" {
		args = append(args, "-role-file", briefDir+"/"+briefRoleFileName)
	}
	if b.PolicyFragment != nil {
		args = append(args, "-policy-fragment-file", briefDir+"/"+briefPolicyFragmentName)
	}
	if b.AcceptanceCriteria != "" {
		args = append(args, "-acceptance-file", briefDir+"/"+briefAcceptanceFileName)
	}
	return args
}

func activeDeadlineSeconds(timeout string) int64 {
	d, err := time.ParseDuration(timeout)
	if err != nil || d <= 0 {
		d = 30 * time.Minute
	}
	return int64(d.Seconds())
}

func HomePVCName(agent string) string {
	safeAgent := dnsLabelPart(agent)
	if safeAgent == "" {
		safeAgent = "agent"
	}
	maxAgentLen := maxJobNameLength - len(homePVCNamePrefix)
	if len(safeAgent) > maxAgentLen {
		safeAgent = strings.Trim(safeAgent[:maxAgentLen], "-")
	}
	if safeAgent == "" {
		safeAgent = "agent"
	}
	return homePVCNamePrefix + safeAgent
}

func SharedReposPVCName() string {
	return sharedReposPVCName
}

func builderJobName(agent, runID, taskID string) string {
	suffix := runNameSuffix(runID, taskID)
	prefix := "builder-"
	separator := "-"
	agentMax := maxJobNameLength - len(prefix) - len(separator) - len(suffix)
	safeAgent := dnsLabelPart(agent)
	if len(safeAgent) > agentMax {
		safeAgent = strings.Trim(safeAgent[:agentMax], "-")
	}
	if safeAgent == "" {
		safeAgent = "agent"
	}
	return prefix + safeAgent + separator + suffix
}

func runNameSuffix(runID, taskID string) string {
	if strings.HasPrefix(runID, "run-") {
		hex := strings.NewReplacer("-", "").Replace(strings.TrimPrefix(runID, "run-"))
		if len(hex) > runSuffixHexLength {
			hex = hex[:runSuffixHexLength]
		}
		if hex = dnsLabelPart(hex); hex != "" {
			return "run-" + hex
		}
	}
	if runID != "" {
		if suffix := dnsLabelPart(runID, maxRunSuffixLength); suffix != "" {
			return suffix
		}
	}
	if suffix := dnsLabelPart(taskID, maxRunSuffixLength); suffix != "" {
		return suffix
	}
	return "run"
}

func dnsLabelPart(s string, maxLen ...int) string {
	s = strings.ToLower(s)
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(maxLen) > 0 && maxLen[0] > 0 && len(out) > maxLen[0] {
		out = strings.Trim(out[:maxLen[0]], "-")
	}
	return out
}
