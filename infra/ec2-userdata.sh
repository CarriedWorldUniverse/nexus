#!/bin/bash
# EC2 user-data — nexus-cw instance bootstrap
# Runs once on first boot.
# Provisions: Tailscale, Forgejo v15.0.1, interchange, Litestream v0.3.14
#
# Sensitive values are fetched from SSM Parameter Store at boot using the
# instance role (nexus-cw-instance-role). Operator must put these before launch:
#
#   /nexus-cw/tailscale-auth-key   (SecureString)
#   /nexus-cw/interchange-id       (String)
#   /nexus-cw/interchange-owner-secret  (SecureString)

set -euo pipefail
exec > >(tee /var/log/nexus-bootstrap.log | logger -t nexus-bootstrap) 2>&1

echo "=== nexus-cw bootstrap start $(date -u) ==="

AWS_REGION="ap-southeast-6"
FORGEJO_VERSION="15.0.1"
FORGEJO_URL="https://codeberg.org/forgejo/forgejo/releases/download/v${FORGEJO_VERSION}/forgejo-${FORGEJO_VERSION}-linux-amd64"
LITESTREAM_VERSION="0.3.14"
LITESTREAM_URL="https://github.com/benbjohnson/litestream/releases/download/v${LITESTREAM_VERSION}/litestream-v${LITESTREAM_VERSION}-linux-amd64.tar.gz"
INTERCHANGE_S3="s3://nexus-cw-forgejo-aux/bootstrap/interchange-linux-amd64"

# ── Fetch secrets from SSM ────────────────────────────────────────────────────
fetch_ssm() {
  aws ssm get-parameter --region "${AWS_REGION}" --name "$1" --with-decryption \
    --query Parameter.Value --output text
}

TAILSCALE_AUTH_KEY=$(fetch_ssm /nexus-cw/tailscale-auth-key)
INTERCHANGE_ID=$(fetch_ssm /nexus-cw/interchange-id)
OWNER_SECRET=$(fetch_ssm /nexus-cw/interchange-owner-secret)

# ── Package baseline ──────────────────────────────────────────────────────────
# AL2023 ships curl-minimal; --allowerasing swaps it for full curl.
# tar is needed for litestream extraction.
dnf install -y --allowerasing curl tar

# ── System users ──────────────────────────────────────────────────────────────
useradd --system --no-create-home --shell /sbin/nologin git 2>/dev/null || true
useradd --system --home /var/lib/interchange --create-home --shell /sbin/nologin interchange 2>/dev/null || true

# ── Tailscale ─────────────────────────────────────────────────────────────────
curl -fsSL https://tailscale.com/install.sh | sh
systemctl enable --now tailscaled
tailscale up --authkey "${TAILSCALE_AUTH_KEY}" --hostname nexus-cw-ec2 --accept-routes
TAILSCALE_IP=$(tailscale ip -4)
echo "Tailscale IP: ${TAILSCALE_IP}"

# Enable Tailscale Funnel for Forgejo on port 3000 (HTTPS via *.ts.net)
tailscale funnel --bg 3000

# Resolve the ts.net hostname
FORGEJO_HOSTNAME=$(tailscale status --json | python3 -c "
import sys, json
data = json.load(sys.stdin)
print(data['Self']['DNSName'].rstrip('.'))
")
echo "Forgejo hostname: ${FORGEJO_HOSTNAME}"

# ── Forgejo ───────────────────────────────────────────────────────────────────
install -d -o git -g git -m 755 /var/lib/forgejo/data
install -d -o git -g git -m 755 /var/lib/forgejo/data/repositories
install -d -o git -g git -m 755 /var/lib/forgejo/custom/conf
install -d -m 755 /var/log/forgejo
chown git:git /var/log/forgejo

curl -fsSL "${FORGEJO_URL}" -o /usr/local/bin/forgejo
chmod +x /usr/local/bin/forgejo

cat > /var/lib/forgejo/custom/conf/app.ini << APPINI
APP_NAME = nexus-cw Forgejo
RUN_USER = git

[server]
DOMAIN           = ${FORGEJO_HOSTNAME}
ROOT_URL         = https://${FORGEJO_HOSTNAME}/
HTTP_ADDR        = 0.0.0.0
HTTP_PORT        = 3000
SSH_DOMAIN       = ${FORGEJO_HOSTNAME}
SSH_PORT         = 222
START_SSH_SERVER = true
SSH_LISTEN_PORT  = 222

[database]
DB_TYPE  = sqlite3
PATH     = /var/lib/forgejo/data/forgejo.db

[security]
INSTALL_LOCK             = true
DISABLE_GIT_HOOKS        = false

[service]
DISABLE_REGISTRATION = true

[storage]
STORAGE_TYPE              = minio
MINIO_ENDPOINT            = s3.${AWS_REGION}.amazonaws.com
MINIO_BUCKET              = nexus-cw-forgejo-aux
MINIO_USE_SSL             = true
MINIO_INSECURE_SKIP_VERIFY = false
SERVE_DIRECT              = true

[storage.lfs]
STORAGE_TYPE    = minio
MINIO_BASE_PATH = lfs/

[storage.attachments]
STORAGE_TYPE    = minio
MINIO_BASE_PATH = attachments/

[storage.avatars]
STORAGE_TYPE    = minio
MINIO_BASE_PATH = avatars/

[storage.repo-avatars]
STORAGE_TYPE    = minio
MINIO_BASE_PATH = repo-avatars/

[storage.archives]
STORAGE_TYPE    = minio
MINIO_BASE_PATH = archives/

[storage.packages]
STORAGE_TYPE    = minio
MINIO_BASE_PATH = packages/

[storage.actions_log]
STORAGE_TYPE    = minio
MINIO_BASE_PATH = actions_log/

[storage.actions_artifacts]
STORAGE_TYPE    = minio
MINIO_BASE_PATH = actions_artifacts/

[log]
MODE      = file
LEVEL     = Info
ROOT_PATH = /var/log/forgejo
APPINI

chown -R git:git /var/lib/forgejo

cat > /etc/systemd/system/forgejo.service << 'UNIT'
[Unit]
Description=Forgejo
After=network.target litestream-forgejo.service
Wants=litestream-forgejo.service

[Service]
User=git
Group=git
WorkingDirectory=/var/lib/forgejo
ExecStart=/usr/local/bin/forgejo web --config /var/lib/forgejo/custom/conf/app.ini
Restart=always
RestartSec=5s
Environment=HOME=/var/lib/forgejo USER=git GITEA_WORK_DIR=/var/lib/forgejo

[Install]
WantedBy=multi-user.target
UNIT

# ── Litestream ────────────────────────────────────────────────────────────────
curl -fsSL "${LITESTREAM_URL}" -o /tmp/litestream.tar.gz
tar -C /usr/local/bin -xzf /tmp/litestream.tar.gz litestream
chmod +x /usr/local/bin/litestream
rm /tmp/litestream.tar.gz

cat > /etc/litestream-forgejo.yml << LSCFG
dbs:
  - path: /var/lib/forgejo/data/forgejo.db
    replicas:
      - url: s3://nexus-cw-forgejo-metadata-litestream/forgejo.db
        region: ${AWS_REGION}
LSCFG

cat > /etc/litestream-interchange.yml << LSCFG
dbs:
  - path: /var/lib/interchange/state.db
    replicas:
      - url: s3://nexus-cw-interchange-litestream/state.db
        region: ${AWS_REGION}
LSCFG

cat > /etc/systemd/system/litestream-forgejo.service << 'UNIT'
[Unit]
Description=Litestream (Forgejo DB)
After=network.target

[Service]
ExecStartPre=/usr/local/bin/litestream restore -if-replica-exists -o /var/lib/forgejo/data/forgejo.db s3://nexus-cw-forgejo-metadata-litestream/forgejo.db
ExecStart=/usr/local/bin/litestream replicate -config /etc/litestream-forgejo.yml
User=git
Restart=always
RestartSec=5s

[Install]
WantedBy=multi-user.target
UNIT

cat > /etc/systemd/system/litestream-interchange.service << 'UNIT'
[Unit]
Description=Litestream (interchange DB)
After=network.target

[Service]
ExecStartPre=/usr/local/bin/litestream restore -if-replica-exists -o /var/lib/interchange/state.db s3://nexus-cw-interchange-litestream/state.db
ExecStart=/usr/local/bin/litestream replicate -config /etc/litestream-interchange.yml
User=interchange
Restart=always
RestartSec=5s

[Install]
WantedBy=multi-user.target
UNIT

# ── Interchange ───────────────────────────────────────────────────────────────
aws s3 cp "${INTERCHANGE_S3}" /usr/local/bin/interchange --region "${AWS_REGION}"
chmod +x /usr/local/bin/interchange

install -d -o interchange -g interchange -m 750 /var/lib/interchange

# Write interchange unit — owner-secret and interchange-id from SSM, expanded here
cat > /etc/systemd/system/interchange.service << UNIT
[Unit]
Description=interchange relay
After=network.target tailscaled.service litestream-interchange.service
Wants=litestream-interchange.service

[Service]
User=interchange
Group=interchange
ExecStart=/usr/local/bin/interchange \\
    -addr 0.0.0.0:10000 \\
    -tailnet-addr ${TAILSCALE_IP}:10001 \\
    -id ${INTERCHANGE_ID} \\
    -db /var/lib/interchange/state.db \\
    -owner-secret ${OWNER_SECRET}
Restart=always
RestartSec=5s

[Install]
WantedBy=multi-user.target
UNIT

# ── Nightly git bundle cron ────────────────────────────────────────────────────
cat > /usr/local/bin/git-bundle-backup.sh << 'SCRIPT'
#!/bin/bash
set -euo pipefail
BUNDLE_BUCKET="s3://nexus-cw-git-bundles"
REPO_ROOT="/var/lib/forgejo/data/repositories"
DATE=$(date -u +%Y-%m-%d)

find "${REPO_ROOT}" -name "*.git" -type d | while read -r repo; do
    rel="${repo#${REPO_ROOT}/}"
    bundle_name="${rel//\//_}.${DATE}.bundle"
    git bundle create "/tmp/${bundle_name}" --all --git-dir="${repo}" 2>/dev/null && \
        aws s3 cp "/tmp/${bundle_name}" "${BUNDLE_BUCKET}/${bundle_name}" && \
        rm -f "/tmp/${bundle_name}"
done
SCRIPT
chmod +x /usr/local/bin/git-bundle-backup.sh

echo "0 3 * * * root /usr/local/bin/git-bundle-backup.sh >> /var/log/git-bundle-backup.log 2>&1" \
  > /etc/cron.d/git-bundle-backup

# ── Enable and start everything ───────────────────────────────────────────────
systemctl daemon-reload
systemctl enable litestream-forgejo litestream-interchange forgejo interchange
systemctl start litestream-forgejo litestream-interchange
systemctl start forgejo interchange

echo "=== nexus-cw bootstrap complete $(date -u) ==="
echo "Forgejo:     https://${FORGEJO_HOSTNAME}"
echo "interchange: ${TAILSCALE_IP}:10000"
echo "Full log:    /var/log/nexus-bootstrap.log"
