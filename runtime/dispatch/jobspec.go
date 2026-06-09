package dispatch

import (
	"strings"

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
	GitCredName   string
	ActivityDir   string
	LynxAIBaseURL string
	LynxAIKey     string
}

func int32p(v int32) *int32 { return &v }

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
	if provider == "" {
		provider = "codex-cli"
	}
	codexProvider := provider == "codex-cli"
	antigravityProvider := provider == "antigravity-cli"
	// The Job runs AS the named agent: keyfile = aspect-keyfile-<agent>, and
	// the Job name + labels carry the agent + run id.
	keyfileAspect := b.Agent
	labels := map[string]string{
		"app":                   "nexus-builder",
		"nexus.dispatch/agent":  b.Agent,
		"nexus.dispatch/ticket": b.Ticket,
		"nexus.dispatch/run-id": b.RunID,
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
		{Name: "CW_AGENT_HOME_REPO", Value: homeRepoMountPath},
		{Name: "CW_AGENT_HOME_WORKDIR", Value: homeWorkMountPath},
		{Name: "CW_SHARED_REPOS_DIR", Value: sharedReposMountPath},
		// The codex-auth init container writes /root/.codex; the builder
		// entrypoint moves HOME to the per-agent home worktree, so pin
		// CODEX_HOME to the auth location, HOME-independent.
		{Name: "CODEX_HOME", Value: "/root/.codex"},
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
		{Name: "keyfile", MountPath: "/etc/nexus", ReadOnly: true},
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
		{Name: "keyfile", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "aspect-keyfile-" + keyfileAspect}}},
		{Name: "brief", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "brief-" + taskID}}}},
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
						Args: []string{
							"-k", "/etc/nexus/keyfile.json",
							"-builder",
							"-brief-file", "/etc/dispatch/brief.md",
							"-reply-topic", b.Thread,
							"-builder-timeout", cfg.BriefTimeout,
							"-repo", b.Repo,
							"-ticket", b.Ticket,
							"-branch", b.Branch,
						},
						Env:          env,
						VolumeMounts: volumeMounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
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
