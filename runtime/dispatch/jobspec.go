package dispatch

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type JobConfig struct {
	Image        string
	Namespace    string
	NodeIP       string
	BrokerHost   string
	BriefTimeout string
	GitCredName  string
}

func int32p(v int32) *int32 { return &v }

// BuildJob mirrors deploy/worker/job.yaml for one dispatch brief.
func BuildJob(b Brief, cfg JobConfig, taskID string) *batchv1.Job {
	if cfg.BriefTimeout == "" {
		cfg.BriefTimeout = "30m"
	}
	labels := map[string]string{
		"app":                   "nexus-builder",
		"nexus.dispatch/agent":  b.Agent,
		"nexus.dispatch/ticket": b.Ticket,
	}
	annotations := map[string]string{}
	if b.Thread != "" {
		annotations["nexus.dispatch/thread"] = b.Thread
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "builder-" + b.Agent + "-" + taskID,
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
					InitContainers: []corev1.Container{{
						Name:            "codex-auth",
						Image:           cfg.Image,
						ImagePullPolicy: corev1.PullNever,
						Command:         []string{"sh", "-c", "mkdir -p /root/.codex && cp /codex-secret/auth.json /root/.codex/auth.json && chmod 600 /root/.codex/auth.json"},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "codex-home", MountPath: "/root/.codex"},
							{Name: "codex-secret", MountPath: "/codex-secret", ReadOnly: true},
						},
					}},
					Containers: []corev1.Container{{
						Name:            "builder",
						Image:           cfg.Image,
						ImagePullPolicy: corev1.PullNever,
						Args: []string{
							"-k", "/etc/nexus/keyfile.json",
							"-builder",
							"-brief-file", "/etc/dispatch/brief.md",
							"-builder-timeout", cfg.BriefTimeout,
						},
						Env: []corev1.EnvVar{
							{Name: "CW_SEAM_URL", Value: "https://" + cfg.BrokerHost + ":7888"},
							{Name: "GOCACHE", Value: "/cache/go"},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "work", MountPath: "/work"},
							{Name: "cache", MountPath: "/cache"},
							{Name: "keyfile", MountPath: "/etc/nexus", ReadOnly: true},
							// Brief ConfigMap mounts as its OWN directory — must NOT be a
							// file inside /etc/nexus (the keyfile Secret's mount point), or
							// the OCI runtime fails with "not a directory" (NEX-437).
							{Name: "brief", MountPath: "/etc/dispatch", ReadOnly: true},
							{Name: "codex-home", MountPath: "/root/.codex"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "work", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "nexus-builder-work"}}},
						{Name: "cache", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "nexus-builder-work"}}},
						{Name: "keyfile", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "aspect-keyfile-" + b.Agent}}},
						{Name: "codex-secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "codex-auth"}}},
						{Name: "brief", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "brief-" + taskID}}}},
						{Name: "codex-home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
}
