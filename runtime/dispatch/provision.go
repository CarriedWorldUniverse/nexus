package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
)

var execCommandContext = exec.CommandContext

// Provision ensures per-dispatch prerequisites exist before the Job runs.
func (c *Controller) Provision(ctx context.Context, b Brief, taskID string) error {
	if err := c.K8s.EnsureKeyfileSecret(ctx, b.Agent); err != nil {
		return fmt.Errorf("provision: keyfile secret for %s missing: %w", b.Agent, err)
	}
	if c.Cfg.GitCredName != "" {
		cmd := execCommandContext(ctx, "cw", "credential", "issue-git-permission",
			"--aspect", b.Agent, "--name", c.Cfg.GitCredName)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("provision: git-cred grant: %w (%s)", err, out)
		}
	} else {
		slog.Info("dispatch: skipping git credential grant; git credential name not configured",
			"aspect", b.Agent, "repo", b.Repo)
	}
	return c.K8s.PutBriefConfigMap(ctx, taskID, b.Task)
}
