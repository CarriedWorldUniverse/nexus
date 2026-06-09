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

# Session bus (gnome-keyring's Secret Service is exposed over it).
eval "$(dbus-launch --sh-syntax)"
export DBUS_SESSION_BUS_ADDRESS DBUS_SESSION_BUS_PID

# Initialise/unlock the login keyring, then start the secrets component. The
# password is fed on stdin; --login is idempotent (creates on first run,
# unlocks thereafter).
printf '%s' "$KEYRING_PASS" | gnome-keyring-daemon --daemonize --login >/dev/null 2>&1 || true
eval "$(printf '%s' "$KEYRING_PASS" | gnome-keyring-daemon --start --components=secrets 2>/dev/null)" || true
export GNOME_KEYRING_CONTROL

echo "maren-entrypoint: Secret Service up (DBUS=$DBUS_SESSION_BUS_ADDRESS); starting agentfunnel" >&2
exec /usr/local/bin/agentfunnel "$@"
