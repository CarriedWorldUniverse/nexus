#!/bin/sh
# Bring up a headless Secret Service (session dbus + gnome-keyring, unlocked
# from KEYRING_PASS), then exec the always-on agentfunnel. agy reads/writes its
# OAuth session token via this Secret Service; the keyring store lives on a PVC
# at $HOME/.local/share/keyrings, so a one-time `agy` login persists across
# restarts. First boot creates the login keyring with KEYRING_PASS; later boots
# unlock it with the same passphrase.
set -e

: "${KEYRING_PASS:?KEYRING_PASS required}"
: "${HOME:=/root}"
mkdir -p "$HOME/.local/share/keyrings"

# FIXED session-bus address so `kubectl exec` (the one-time agy login) reaches
# the SAME gnome-keyring as this agentfunnel — a random dbus-launch address
# would put the login token in a different, unreachable keyring.
export DBUS_SESSION_BUS_ADDRESS="unix:path=/tmp/maren-session-bus"
rm -f /tmp/maren-session-bus
dbus-daemon --session --address="$DBUS_SESSION_BUS_ADDRESS" --fork
printf 'export DBUS_SESSION_BUS_ADDRESS=%s\n' "$DBUS_SESSION_BUS_ADDRESS" > /tmp/maren-dbus-env

# Initialise/unlock the login keyring, then start the secrets component. The
# password is fed on stdin; --login is idempotent (creates on first run,
# unlocks thereafter).
printf '%s' "$KEYRING_PASS" | gnome-keyring-daemon --daemonize --login >/dev/null 2>&1 || true
eval "$(printf '%s' "$KEYRING_PASS" | gnome-keyring-daemon --start --components=secrets 2>/dev/null)" || true
export GNOME_KEYRING_CONTROL

echo "maren-entrypoint: Secret Service up ($DBUS_SESSION_BUS_ADDRESS); starting agentfunnel" >&2
exec /usr/local/bin/agentfunnel "$@"
