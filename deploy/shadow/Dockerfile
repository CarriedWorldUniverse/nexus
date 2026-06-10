# Always-on shadow pod = the builder image + claude-code.
#
# Shadow is the orchestrator: full claude-code harness in the pod, running one
# consistent session via the funnel (claude-code HAS session jsonl, so the
# funnel's continuity + compaction machinery applies — unlike the agy pod).
# Auth + sessions live file-based under ~/.claude, which the deployment backs
# with a PVC (the maren lesson: persist the REAL state dir). One-time
# interactive login via shadow-claude-login (kubectl exec).
FROM localhost/nexus-builder:dev

RUN npm config set prefix /root/.npm-global \
 && npm i -g @anthropic-ai/claude-code \
 && ln -sf /root/.npm-global/bin/claude /usr/local/bin/claude \
 && claude --version

COPY shadow-entrypoint.sh /usr/local/bin/shadow-entrypoint.sh
COPY shadow-claude-login.sh /usr/local/bin/shadow-claude-login
RUN chmod 0755 /usr/local/bin/shadow-entrypoint.sh /usr/local/bin/shadow-claude-login

# Operational context for the claude-code session. The funnel runs claude
# with cmd.Dir = AspectHome = the funnel's cwd, which is the builder image's
# WORKDIR /work — claude-code auto-reads CLAUDE.md there at session start,
# and CLAUDE.md points at RUNBOOK.md. Image-baked (git-versioned, survives
# pod restarts, no broker schema involved); /work has no volume mount in the
# shadow deployment, so these stay exactly as built.
COPY CLAUDE.md RUNBOOK.md /work/

ENTRYPOINT ["/usr/local/bin/shadow-entrypoint.sh"]
