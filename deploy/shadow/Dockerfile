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

ENTRYPOINT ["/usr/local/bin/shadow-entrypoint.sh"]
