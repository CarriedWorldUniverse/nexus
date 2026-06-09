# Always-on maren artist pod = the builder image + a headless Secret Service.
#
# agy (Antigravity CLI) authenticates via the OS keyring (Linux Secret Service)
# — there is no API-key/env path. So the always-on pod runs gnome-keyring under
# a session dbus, unlocked at boot from KEYRING_PASS, with its store on a PVC.
# A one-time interactive `agy` login persists the OAuth token in that keyring,
# surviving turns and restarts. The agentfunnel then runs always-on as maren.
FROM localhost/nexus-builder:dev

RUN apt-get update && apt-get install -y --no-install-recommends \
      gnome-keyring dbus-x11 libsecret-1-0 \
 && rm -rf /var/lib/apt/lists/*

COPY maren-entrypoint.sh /usr/local/bin/maren-entrypoint.sh
COPY maren-agy-login.sh /usr/local/bin/maren-agy-login
RUN chmod 0755 /usr/local/bin/maren-entrypoint.sh /usr/local/bin/maren-agy-login

ENTRYPOINT ["/usr/local/bin/maren-entrypoint.sh"]
