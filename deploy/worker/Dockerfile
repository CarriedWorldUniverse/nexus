# Builder-agent worker image (NEX-436). One shared image; identity +
# provider differ per-run via the mounted keyfile + binding, not per-image.
# NO secrets baked in — git/provider creds come from the M1 custodian seam
# (cw credential helper / kind=provider) at runtime.
#
# Build context = the nexus repo root; the prebuilt nexus binaries + cw are
# expected alongside this Dockerfile (see build.sh, which builds them on the
# host first — the host has the git auth for cw's private modules).
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates curl gnupg xz-utils && rm -rf /var/lib/apt/lists/*

# Go toolchain — builders compile Go work.
RUN curl -fsSL https://go.dev/dl/go1.26.3.linux-amd64.tar.gz -o /tmp/go.tgz \
 && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
ENV PATH=/usr/local/go/bin:/usr/local/bin:/root/go/bin:/root/.npm-global/bin:$PATH

# Node (for claude-code + the codex installer).
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
 && apt-get install -y --no-install-recommends nodejs && rm -rf /var/lib/apt/lists/*

# Prebuilt nexus binaries + cw (built on the host by build.sh, copied into
# the context). agentfunnel runs the builder; the nexus-*-mcp servers + cw
# are the aspect's tool surface.
COPY agentfunnel nexus-issue-mcp nexus-jira-mcp nexus-comms-mcp cw /usr/local/bin/

# Provider CLI: codex (standalone, installed via npm — fetches the linux
# binary into the standalone layout). claude-code can be added the same way
# (npm i -g @anthropic-ai/claude-code) when claude-backed builders are wanted.
RUN npm config set prefix /root/.npm-global && npm i -g @openai/codex && codex --version

# cw is git's credential helper: clone/push auth flows through the M1 custodian
# seam, never a baked secret (NEX-437). cw's git-helper reads CW_TOKEN (the
# session JWT, exported by agentfunnel in -builder mode) + CW_SEAM_URL (Job env).
RUN git config --system credential.helper '!cw credential git-helper'

WORKDIR /work
ENTRYPOINT ["/usr/local/bin/agentfunnel"]
