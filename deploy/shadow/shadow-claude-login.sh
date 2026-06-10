#!/bin/sh
# One-time (or re-auth) claude-code login for the always-on shadow pod:
#   kubectl exec -it deploy/shadow-aspect -n nexus -- shadow-claude-login
# Runs `claude setup-token` — prints an OAuth URL; complete it in a browser
# and paste the code back. The long-lived credential lands in the PVC-backed
# $HOME/.claude, surviving pod restarts. Any args are passed through to
# `claude` instead (e.g. shadow-claude-login --version).
if [ $# -gt 0 ]; then exec claude "$@"; fi
exec claude setup-token
