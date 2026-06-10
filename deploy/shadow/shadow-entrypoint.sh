#!/bin/sh
# Always-on shadow: ensure the PVC-backed ~/.claude exists, then exec the
# funnel. claude-code reads its OAuth credentials + session files from
# $HOME/.claude; the one-time shadow-claude-login populates it and the PVC
# keeps both auth and the consistent session across restarts.
set -e
: "${HOME:=/root}"
mkdir -p "$HOME/.claude"
echo "shadow-entrypoint: HOME=$HOME (.claude PVC-backed); starting agentfunnel" >&2
exec /usr/local/bin/agentfunnel "$@"
