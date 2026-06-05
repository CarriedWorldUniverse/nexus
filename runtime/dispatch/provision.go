package dispatch

import (
	"context"
	"fmt"
	"os/exec"
)

// Provision ensures per-dispatch prerequisites exist before the Job runs.
func (c *Controller) Provision(ctx context.Context, b Brief, taskID string) error {
	if err := c.K8s.EnsureKeyfileSecret(ctx, b.Agent); err != nil {
		return fmt.Errorf("provision: keyfile secret for %s missing: %w", b.Agent, err)
	}
	if b.Repo != "" {
		cmd := exec.CommandContext(ctx, "cw", "credential", "issue-git-permission",
			"--aspect", b.Agent, "--repo", b.Repo)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("provision: git-cred grant: %w (%s)", err, out)
		}
	}
	return c.K8s.PutBriefConfigMap(ctx, taskID, b.Task)
}
