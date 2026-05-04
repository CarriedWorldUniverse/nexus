# AWS bootstrap — Forgejo + Interchange on EC2

**Date:** 2026-05-01
**Status:** Draft for anvil — build brief
**Scope:** Provision a single AWS EC2 instance running Forgejo (private git host) + interchange (carried-world relay) + Litestream replication to S3, joined to the operator's existing Tailscale tailnet.
**Repo:** `nexus-cw/interchange` (binary lives at `cmd/interchange`); operator uses Forgejo upstream binary.
**AWS account:** 088109723923 (Jacinta Saggers); IAM users `ADMjacinta`/`nexus-cw`/`nexus-ops` provisioned 2026-05-01.

---

## 1. Goals & non-goals

**Goals:**
- Move interchange off operator's personal machine onto a dedicated EC2.
- Provide private git hosting via Forgejo for `nexus-cw/*` repos (currently only on operator's laptop + GitHub).
- Treat S3 as the durability layer; the EC2 is stateless and rebuildable from S3.
- Tailnet-attached (Tailscale on the instance) so all admin access is over the existing tailnet.
- Public surface (if any) routes via Tailscale Funnel for TLS termination — no Nginx + certbot.
- Single-region (ap-southeast-6 primary), with ap-southeast-4 (Melbourne) policy-allowed for future failover. Failover itself is out of scope for v1.

**Non-goals:**
- High availability (single instance, accepted single point of failure for v1).
- Auto-scaling.
- Multi-tenant Forgejo (one operator, one instance).
- IAM Identity Center / SSO setup (deferred; static keys for service accounts are fine for v1 scope).

---

## 2. Instance

- **Type:** `t2.micro` (1 vCPU, 1 GiB RAM) — AWS Free Tier eligible (750 hr/mo, 12 months on new accounts). Upgrade to `t4g.small` (2 GiB RAM, ARM, ~$5/mo with 1-year RI) if RAM pressure surfaces.
- **OS:** Amazon Linux 2023 (latest AMI in ap-southeast-6).
- **EBS root:** 30 GiB gp3 (Free Tier covers 30 GiB for 12 months).
- **Region:** ap-southeast-6 (Asia Pacific - New Zealand). Already opt-in-enabled.
- **AZ:** any single AZ in the region; availability zone selection happens at RunInstances time.

---

## 3. Networking

- **Tailscale:** install via package manager. Auth via `tailscale up --authkey <ephemeral-key>` from operator's tailnet. Joins existing tailnet.
- **Tailscale Funnel:** enabled on the instance for the Forgejo HTTPS endpoint only. Manages TLS via Tailscale's `*.ts.net` cert flow; no Nginx + certbot.
- **Security group:** allow inbound only on
  - port 41641/UDP (Tailscale)
  - port 22/TCP from operator's tailnet IP only (SSH, defensive — Tailscale should be primary access)
  - egress: all (default)
- **No public ports** other than what Tailscale Funnel opens to the internet (which lives outside the security group — Funnel terminates at Tailscale, then proxies in).

---

## 4. Services on the instance

All services are bare Go binaries on systemd. No Docker.

### 4.1 Forgejo

- **Binary:** download Forgejo's Linux ARM64 (or AMD64 if t2.micro) static binary from `https://codeberg.org/forgejo/forgejo/releases/`. Pin a specific version (e.g., latest stable v9.x at build time).
- **Systemd unit:** `/etc/systemd/system/forgejo.service`. Runs as `git` user, listens on `0.0.0.0:3000` (web UI) and `0.0.0.0:22` (git+ssh — but SSH port is taken by sshd; use port 222 or disable Forgejo SSH and use HTTPS-only).
- **Tailscale Funnel:** point public `*.ts.net` hostname at `localhost:3000`.
- **app.ini config:**
  - `[database]` — SQLite at `/var/lib/forgejo/data/forgejo.db`. Litestream replicates to S3.
  - `[storage.lfs]`, `[storage.attachments]`, `[storage.archives]`, `[storage.packages]`, `[storage.actions_log]`, `[storage.actions_artifacts]` — all S3 (`nexus-cw-forgejo-aux` bucket, distinct prefix per type).
  - `[server]` — root URL = the Funnel `*.ts.net` URL.
  - `[security]` — disable signup, single-user mode.
- **Storage paths (EBS-backed):**
  - `/var/lib/forgejo/data/forgejo.db` (SQLite metadata)
  - `/var/lib/forgejo/data/repositories/` (git pack files — NOT S3-backed by Forgejo)
- **Git data backup:** nightly `git bundle` of every repo to `nexus-cw-git-bundles` S3 bucket via cron + bash script. Restore-from-bundle path documented in this spec's §7.

### 4.2 Interchange

- **Binary:** build `nexus-cw/interchange/cmd/interchange` for Linux + the instance's arch. CI or manual cross-build.
- **Systemd unit:** `/etc/systemd/system/interchange.service`. Runs as `interchange` user, listens on `0.0.0.0:10000` bound to the Tailscale interface only (not 0.0.0.0 — use `--bind <tailscale-ip>`). Tailnet-only access; no Funnel.
- **Storage:** SQLite at `/var/lib/interchange/state.db`. Litestream replicates to S3.
- **Existing identity / keys:** interchange already has Ed25519 server identity from the existing tailnet host; migrate that key over (don't regenerate, peers won't recognize a new pubkey).

### 4.3 Litestream

- **Binary:** download from `https://github.com/benbjohnson/litestream/releases`. Pin to v0.3.x.
- **Systemd unit (per SQLite db):**
  - `/etc/systemd/system/litestream-forgejo.service` — replicates `/var/lib/forgejo/data/forgejo.db` → `s3://nexus-cw-forgejo-metadata-litestream/forgejo.db`
  - `/etc/systemd/system/litestream-interchange.service` — replicates `/var/lib/interchange/state.db` → `s3://nexus-cw-interchange-litestream/state.db`
- **Replication frequency:** Litestream defaults (continuous WAL streaming + 5-second sync interval). Acceptable RPO ≈ 5 seconds.

---

## 5. S3 buckets

All buckets in `ap-southeast-6`. Versioning enabled on all. Server-side encryption with S3-managed keys (SSE-S3) enabled by default.

| Bucket | Purpose | Retention |
|---|---|---|
| `nexus-cw-forgejo-aux` | Forgejo LFS + attachments + archives + packages + actions logs/artifacts | Forever (operator-driven) |
| `nexus-cw-forgejo-metadata-litestream` | Forgejo SQLite WAL stream | Forever; Litestream manages internal lifecycle |
| `nexus-cw-interchange-litestream` | Interchange SQLite WAL stream | Forever; Litestream manages internal lifecycle |
| `nexus-cw-git-bundles` | Nightly git bundle dumps of every repo | 30 days lifecycle policy |

---

## 6. IAM

### 6.1 EC2 instance role: `nexus-cw-instance-role`

**Operator must pre-create this role via Console as ADMjacinta** (nexus-cw cannot create IAM roles per the privilege-escalation review).

**Trust policy** (allows EC2 to assume the role):

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "Service": "ec2.amazonaws.com" },
    "Action": "sts:AssumeRole"
  }]
}
```

**Permissions policy** (inline or managed, attached to the role):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "S3AccessForLitestreamAndForgejoAux",
      "Effect": "Allow",
      "Action": [
        "s3:GetObject", "s3:PutObject", "s3:DeleteObject",
        "s3:ListBucket", "s3:GetBucketLocation"
      ],
      "Resource": [
        "arn:aws:s3:::nexus-cw-forgejo-aux", "arn:aws:s3:::nexus-cw-forgejo-aux/*",
        "arn:aws:s3:::nexus-cw-forgejo-metadata-litestream", "arn:aws:s3:::nexus-cw-forgejo-metadata-litestream/*",
        "arn:aws:s3:::nexus-cw-interchange-litestream", "arn:aws:s3:::nexus-cw-interchange-litestream/*",
        "arn:aws:s3:::nexus-cw-git-bundles", "arn:aws:s3:::nexus-cw-git-bundles/*"
      ]
    },
    {
      "Sid": "SSMAgentRequirements",
      "Effect": "Allow",
      "Action": [
        "ssm:UpdateInstanceInformation",
        "ssmmessages:CreateControlChannel", "ssmmessages:CreateDataChannel",
        "ssmmessages:OpenControlChannel", "ssmmessages:OpenDataChannel",
        "ec2messages:GetMessages", "ec2messages:AcknowledgeMessage",
        "ec2messages:DeleteMessage", "ec2messages:FailMessage",
        "ec2messages:GetEndpoint", "ec2messages:SendReply"
      ],
      "Resource": "*"
    }
  ]
}
```

The SSM agent permissions are required for nexus-ops to use `ssm:SendCommand` and Session Manager against this instance.

### 6.2 Instance profile

Operator also creates an instance profile of the same name (`nexus-cw-instance-role`) and attaches the role to it. nexus-cw's policy allows attaching this pre-created instance profile to a launched instance — but not creating new roles.

---

## 7. Build sequence

1. **(Operator, ADMjacinta)** Pre-create `nexus-cw-instance-role` IAM role + instance profile per §6.
2. **(anvil, nexus-cw)** Create the four S3 buckets per §5.
3. **(anvil, nexus-cw)** Launch t2.micro EC2 with:
   - Amazon Linux 2023 latest AMI
   - 30 GiB gp3 EBS root
   - Security group per §3
   - Instance profile = `nexus-cw-instance-role`
   - User-data script that does the in-instance setup (steps 4-9)
4. **(EC2 user-data)** Install Tailscale, join tailnet via ephemeral auth key.
5. **(EC2 user-data)** Create system users (`git`, `interchange`), install Forgejo binary, install interchange binary, install Litestream binary.
6. **(EC2 user-data)** Write systemd units (forgejo.service, interchange.service, two litestream units), enable + start.
7. **(EC2 user-data)** Forgejo first-run config (admin user, app.ini per §4.1, tying to S3 buckets).
8. **(EC2 user-data)** Migrate interchange Ed25519 identity from existing tailnet host (manual step — operator drops the keypair via SSH/scp before user-data triggers interchange.service, OR interchange.service waits for a flag file).
9. **(EC2 user-data)** Configure cron job for nightly `git bundle` → `nexus-cw-git-bundles`.
10. **(anvil)** Verify Tailscale Funnel hostname for Forgejo.
11. **(anvil)** Verify interchange responds on its tailnet IP at port 10000.
12. **(operator)** Repoint nexus-work and any other federated peers from the old tailnet host to the new EC2 instance's tailnet IP for interchange.
13. **(operator)** Decommission the old tailnet host running interchange.

---

## 8. Disaster recovery

**Goal:** new EC2 instance can rehydrate full state from S3 in <10 minutes.

- **Forgejo SQLite:** `litestream restore -o /var/lib/forgejo/data/forgejo.db s3://nexus-cw-forgejo-metadata-litestream/forgejo.db`
- **Forgejo aux storage:** lives natively on S3 — instance just reads from there.
- **Forgejo git data:** restore most recent bundle per repo from `nexus-cw-git-bundles` (RPO ≤ 24h since last bundle). For zero-RPO of git data, future work: move to a Forgejo fork that supports git-on-S3, or sync `/var/lib/forgejo/data/repositories/` to S3 hourly via aws-s3-sync. Out of scope for v1; bundle-based recovery is acceptable.
- **Interchange SQLite:** `litestream restore -o /var/lib/interchange/state.db s3://nexus-cw-interchange-litestream/state.db`
- **Interchange Ed25519 identity:** must be backed up out-of-band (operator's password manager). Cannot regenerate without re-pairing every federated peer.

---

## 9. Cost estimate

- **EC2 t2.micro:** Free Tier (12 mo); ~$8.50/mo afterward in ap-southeast-6.
- **EBS gp3 30GB:** Free Tier (12 mo); ~$3/mo afterward.
- **S3 storage:** Free Tier 5GB; first month ~$0.60/mo for 100GB; usage-driven thereafter (~$0.023/GB/mo).
- **Data transfer:** Free Tier 100GB/mo egress; rare to exceed for this workload.
- **Tailscale Funnel:** free (community plan covers it).

**v1 first 12 months:** ~$0–2/month all-in (S3 covers it; everything else free tier).
**v1 month 13+:** ~$12–15/month.

---

## 10. Open questions for build time

- **Forgejo arch:** t2.micro is x86_64 only (the t4g.small upgrade path is ARM). If we go t2.micro for free tier, use AMD64 binaries; switch to ARM64 if/when we move to t4g.small.
- **First-boot vs reboot semantics:** decide if user-data idempotently re-runs on reboot or only on first launch (`cloud-init-per once` vs `always`). Lean: once for Tailscale + bootstrap, Litestream restore on every boot to handle disaster recovery cleanly.
- **Forgejo HTTPS only?** Tailscale Funnel terminates HTTPS; Forgejo serves HTTP behind it. Decide if we still want Forgejo to enforce HTTPS internally or trust the Funnel boundary.

These are all build-time calls; flag them on chat as you hit them.

---

## 11. Handoff

anvil owns: §7 steps 2-11. Operator owns §7 step 1 (IAM role pre-create) and steps 12-13 (interchange migration + decom). keel is the spec author and standby reviewer; not in the build path.

Any spec issues — push back on chat, I'll patch.
