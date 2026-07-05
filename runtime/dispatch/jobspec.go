package dispatch

import (
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
		// CW_IMAGE_TAG carries this Job's own image ref into the pod so
		// the M1 Unit 5 worker.status heartbeat can report `image_tag`
		// without re-deriving it. Best-effort/informational — the §7 CI
		// version-knob work (a distinct build unit) is what makes this
		// value meaningfully track a pinned CLI version; today it's
		// whatever cfg.Image already resolves to.
		{Name: "CW_IMAGE_TAG", Value: cfg.Image},
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
			Image:           cfg.Image,
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
						Image:           cfg.Image,
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
