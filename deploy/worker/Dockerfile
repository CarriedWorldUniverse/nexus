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
      git ca-certificates curl gnupg xz-utils \
      ripgrep jq fd-find tree make ffmpeg \
   && rm -rf /var/lib/apt/lists/*

# Go toolchain — builders compile Go work.
RUN curl -fsSL https://go.dev/dl/go1.26.3.linux-amd64.tar.gz -o /tmp/go.tgz \
 && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
ENV PATH=/usr/local/go/bin:/usr/local/bin:/root/go/bin:/root/.npm-global/bin:$PATH

# Node (for claude-code + the codex installer).
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
 && apt-get install -y --no-install-recommends nodejs && rm -rf /var/lib/apt/lists/*

# gh (PR creation in github mode — cw setup-git github authenticates it) and
# helm (render/lint charts — hosting-platform work).
RUN GH_VERSION=2.62.0 \
 && curl -fsSL https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz -o /tmp/gh.tgz \
 && tar -C /tmp -xzf /tmp/gh.tgz \
 && install /tmp/gh_${GH_VERSION}_linux_amd64/bin/gh /usr/local/bin/gh \
 && rm -rf /tmp/gh* \
 && HELM_VERSION=3.16.3 \
 && curl -fsSL https://get.helm.sh/helm-v${HELM_VERSION}-linux-amd64.tar.gz -o /tmp/helm.tgz \
 && tar -C /tmp -xzf /tmp/helm.tgz \
 && install /tmp/linux-amd64/helm /usr/local/bin/helm \
 && rm -rf /tmp/helm* /tmp/linux-amd64 \
 && gh --version && helm version --short

# kubectl — cluster introspection for always-on infra aspects (e.g. keel as the
# error-alert handler). Access is gated by RBAC/ServiceAccount, not the binary,
# so it is inert for dispatch builders (default SA, no perms). Matches the k3s
# server minor (v1.35).
RUN KUBECTL_VERSION=v1.35.5 \
 && curl -fsSL https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl -o /usr/local/bin/kubectl \
 && chmod +x /usr/local/bin/kubectl && kubectl version --client

# nano - lightweight editor for hand-editing config inside the pod (e.g. croft).
RUN apt-get update && apt-get install -y --no-install-recommends nano && rm -rf /var/lib/apt/lists/*

# Chrome — the browser head (NEX tier-1 head for croft; usable by any worker).
# Playwright MCP expects the stable channel at /opt/google/chrome/chrome; the
# deb pulls the full headless-capable dependency set. No GPU needed (swiftshader).
RUN curl -fsSL https://dl.google.com/linux/linux_signing_key.pub | gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg \
 && echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] http://dl.google.com/linux/chrome/deb/ stable main" > /etc/apt/sources.list.d/google-chrome.list \
 && apt-get update && apt-get install -y --no-install-recommends google-chrome-stable fonts-liberation \
 && rm -rf /var/lib/apt/lists/* && google-chrome --version

# kasmVNC + minimal desktop — tier-2 head for croft: web-delivered X desktop
# over the tailnet (croft startup.sh starts the server only where ~/.vnc is
# provisioned on the PVC; inert for pool workers). Deb pinned, bookworm build.
RUN curl -fsSL -o /tmp/kasmvnc.deb https://github.com/kasmtech/KasmVNC/releases/download/v1.4.0/kasmvncserver_bookworm_1.4.0_amd64.deb \
 && apt-get update && apt-get install -y --no-install-recommends /tmp/kasmvnc.deb openbox xterm \
 && rm -rf /var/lib/apt/lists/* /tmp/kasmvnc.deb \
 && dpkg -s kasmvncserver | grep ^Version

# Prebuilt nexus binaries + cw + cairn (staged into the context by build.sh /
# CI, copied into the image). agentfunnel runs the builder; the nexus-*-mcp
# servers + cw are the aspect's tool surface; cairn is the CW_VCS=cairn builder
# VCS (inert when CW_VCS!=cairn). cairn is COPY'd like the other binaries rather
# than fetched in a RUN step: one staging path (build.sh/CI) owns all binary
# versions, and the image build needs no network fetch for it.
COPY agentfunnel nexus-issue-mcp nexus-jira-mcp nexus-comms-mcp nexus-vision-mcp cw cairn /usr/local/bin/

# Provider CLI: codex (standalone, installed via npm — fetches the linux
# binary into the standalone layout).
RUN npm config set prefix /root/.npm-global && npm i -g @openai/codex && codex --version

# Provider CLI: claude-code (M1 Unit 7, PHASE2-DESIGN §6/§7 — frontier auth +
# CLI version knob). CLAUDE_CODE_VERSION pins the exact release this image
# carries; the CI workflow (.github/workflows/runner-images.yml) sets it per
# build and tags the resulting image runner:cli-<ver>, so a pod NEVER
# self-updates mid-job (auto-updater disabled — see README "CLI version
# knob"). Default "latest" only for ad-hoc/dev builds (build.sh); every
# CI-built image passes an explicit version.
ARG CLAUDE_CODE_VERSION=latest
RUN npm i -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
 && claude --version \
 && (claude config set -g autoUpdaterStatus disabled || true)  # -g dropped in newer CLIs; autoupdater is irrelevant in a one-shot Job image

# Provider CLI: Antigravity (agy) — a dependency-free Go binary; the installer
# drops it into /root/.local/bin. The binary install is auth-free; agy's runtime
# auth (a Gemini credential) is mounted at dispatch, separate from this image.
RUN curl -fsSL https://antigravity.google/cli/install.sh | bash && /root/.local/bin/agy --version

# The agent (codex) spawns login shells (sh -lc), which re-source /etc/profile
# and RESET PATH to the distro default — dropping /usr/local/go/bin and
# /root/.npm-global/bin, so `go` and `codex` aren't found and the agent searches.
# Fix both ways: symlink the off-PATH tools into /usr/local/bin (on PATH for
# every shell), and re-add the dirs via /etc/profile.d for login shells.
RUN ln -sf /usr/local/go/bin/go /usr/local/bin/go \
 && ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt \
 && ln -sf /root/.npm-global/bin/codex /usr/local/bin/codex \
 && ln -sf /root/.local/bin/agy /usr/local/bin/agy \
 && ln -sf /usr/bin/fdfind /usr/local/bin/fd \
 && printf 'export PATH=/usr/local/go/bin:/root/.npm-global/bin:/root/.local/bin:/root/go/bin:$PATH\n' > /etc/profile.d/cwb-path.sh \
 && chmod 0644 /etc/profile.d/cwb-path.sh

# cw is git's credential helper: clone/push auth flows through the M1 custodian
# seam, never a baked secret (NEX-437). cw's git-helper reads CW_TOKEN (the
# session JWT, exported by agentfunnel in -builder mode) + CW_SEAM_URL (Job env).
RUN git config --system credential.helper '!cw credential git-helper'

WORKDIR /work
ENTRYPOINT ["/usr/local/bin/agentfunnel"]
