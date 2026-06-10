#!/bin/sh
# One-time (or re-auth) claude-code login for the always-on shadow pod:
#   kubectl exec -it deploy/shadow-aspect -n nexus -- shadow-claude-login
# Runs `claude setup-token` — prints an OAuth URL; complete it in a browser
# and paste the code back. NOTE: setup-token PRINTS a long-lived token (it
# does NOT log the CLI in). Pipe that token into the shadow-claude-auth
# secret (key: token); the deployment feeds it to claude-code via the
# CLAUDE_CODE_OAUTH_TOKEN env. Any args pass through to `claude` instead.
if [ $# -gt 0 ]; then exec claude "$@"; fi
exec claude setup-token
