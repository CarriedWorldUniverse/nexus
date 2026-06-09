#!/bin/sh
# One-time (or re-auth) interactive agy login, run via:
#   kubectl exec -it deploy/maren -n nexus -- maren-agy-login
# It joins the always-on pod's Secret Service so the OAuth token agy obtains is
# stored in the same PVC-backed keyring the running agentfunnel reads. agy
# prints an auth URL; complete it in a browser, paste back the code if asked.
. /tmp/maren-dbus-env
exec agy "$@"
