package dispatch

import (
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
	LynxAIBaseURL string
	LynxAIKey     string
}

func int32p(v int32) *int32 { return &v }

// BuildJob mirrors deploy/worker/job.yaml for one dispatch brief.
func BuildJob(b Brief, cfg JobConfig, taskID string, provider string) *batchv1.Job {
	if cfg.BriefTimeout == "" {
		cfg.BriefTimeout = "30m"
	}
	if provider == "" {
		provider = "codex-cli"
	}
	codexProvider := provider == "codex-cli"
	// The Job runs AS the named agent: keyfile = aspect-keyfile-<agent>, and
	// the Job name + labels carry the agent + run id.
	keyfileAspect := b.Agent
	runShort := b.RunID
	if len(runShort) > 8 {
		runShort = runShort[:8]
	}
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
		{Name: "keyfile", MountPath: "/etc/nexus", ReadOnly: true},
		// Brief ConfigMap mounts as its OWN directory — must NOT be a
		// file inside /etc/nexus (the keyfile Secret's mount point), or
		// the OCI runtime fails with "not a directory" (NEX-437).
		{Name: "brief", MountPath: "/etc/dispatch", ReadOnly: true},
	}
	volumes := []corev1.Volume{
		{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "cache", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "nexus-builder-work"}}},
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

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "builder-" + b.Agent + "-" + func() string {
				if runShort != "" {
					return runShort
				}
				return taskID
			}(),
			Namespace:   cfg.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32p(0),
			TTLSecondsAfterFinished: int32p(3600),
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
