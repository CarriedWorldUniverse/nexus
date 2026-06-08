# dMon nexus bring-up — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: use superpowers:subagent-driven-development or superpowers:executing-plans to work this plan task-by-task. Steps use checkbox (`- [ ]`) syntax. **This plan runs on dMon itself (Fedora Workstation 44), not in CI** — most "tests" are command-output assertions (`systemctl`, `curl`, `sudo -u …`), not unit tests.

**Goal:** Stand up the always-on nexus hub on dMon (Fedora Workstation 44) — broker + embedded Keel Frame + in-process ledger under a `nexus` service account, plus the fixed aspects as per-OS-user `agentfunnel` services, with wren wired to collaborate in a shared Unity editor.

**Architecture:** `nexus serve` is a **single process** = broker + Keel (embedded Frame) + ledger (in-process Go module) + identity, run as the `nexus` user (`nexus.db` + `ledger.db` in one data dir; ledger REST on the broker mux). Each aspect is a **separate keyless `agentfunnel` process** running as its own OS user, connecting to the broker over the tailnet name; aspect personality *homes* live broker-side in the broker's `--aspect-dir`. The whole hub sits in a resource-capped `nexus.slice` so it yields to the operator's interactive/Unity work. TLS is a real Tailscale `*.ts.net` cert (no self-signed).

**Tech stack:** Fedora Workstation 44 (systemd, SELinux enforcing, firewalld), Go (build nexus + agentfunnel), SQLite, Tailscale (transport + cert), Unity (wren), `asusctl`/`supergfxctl`.

> **Design-doc reconciliation:** `docs/2026-05-29-nexus-hosting-dmon-design.md` §4 lists `keel` as a per-OS-user peer to the aspects. The code embeds the Frame **inside** `nexus.exe` (it does not connect over the wire). This plan follows the code: **no `keel` OS user**; Keel runs in the `nexus` process from a frame-role home in `--aspect-dir`. Task 9.1 updates the design doc to match.

> **Scope / sequencing:** phased and incremental. After **Phase 3** you have a working hub (broker + Keel + ledger). Phases 5–7 add aspects additively; you can stop after any aspect. Recommended first cut: through Phase 5 (broker + Keel + ledger + anvil), then add wren (Phase 7), then the rest (Phase 6).

> **Plan location note:** saved under `docs/` (repo convention, next to the design doc) rather than `docs/superpowers/plans/`.

> **Operator-input points** (no fake placeholders — each is an explicit decision step with the concrete command for the chosen branch): the tailnet hostname (Task 0.5), provider API keys (Task 4), the Unity editor version + licence (Task 7.3), and which Unity MCP bridge (Task 7.5).

---

## Conventions used below

- `DMON_TS` = dMon's Tailscale MagicDNS name — **`dmonextreme.tail41686e.ts.net`** (confirmed 2026-05-30).
- `NEXUS_DATA=/var/lib/nexus` = the hub data dir (owned by `nexus`).
- `ASPECTS_DIR=/var/lib/nexus/aspects` = broker-side aspect homes (owned by `nexus`).
- Broker listens on `:7888`; the broker URL everyone uses is `wss://$DMON_TS:7888`.
- `SRC=/usr/local/src/nexus` = a shared build checkout (built once, binaries installed to `/usr/local/bin`).
- `BACKUP=/run/media/jacinta/Backup/backup-DMONEXTREME` = the USB backup of the old Windows DMONEXTREME (the migration source).
- `NEXUS_ID=7a5f2d56-de16-40e8-8505-3360cd982d1d` = the broker identity carried in the restored `nexus.db` — the post-restore boot MUST log this exact id.

---

## This is a RESTORE, not greenfield (read before Phase 2)

dMon already had a running nexus on Windows; its state is on the USB backup. We **migrate** it forward rather than starting empty, so aspect identities, comms history, the Commonplace knowledge, the ledger, credentials, roster, and network-defaults all carry over. Concretely, the **"Migration variant"** section below **overrides**:

- **Task 2.2** (`nexus init` / `identity init`) → replaced by *restoring the DBs* (identity comes with `nexus.db`).
- **Task 3.1** (write Keel's home by hand) → replaced by *restoring the aspect homes* from the backup.
- **Tasks 5.2 / 6.1 keyfile mint** → replaced by **re-mint** (the backup/Drive keyfiles are stale-format — pre-NEX-367/368/332 — and pinned to the old URL; re-mint produces current-format keyfiles for the new host).

Everything else (Phase 0/1 host prep, the `nexus.slice` + units in 3.2, credentials rotation in 4.1, the aspect@ template in 5.3, wren/Unity in 7, backup in 8) applies unchanged.

### Migration variant — RESTORE from the DMONEXTREME backup (Path B)

> Run after Phase 0 + Phase 1 (host prep, users, dirs, SELinux, wakestone group) but **instead of** Phase 2.2 / 3.1 and the mints in 5.2/6.1.

- [ ] **M1: Restore the DBs** (use `src/nexus/nexus/data/` — it has the freshest `nexus.db` + its WAL; copy db+wal+shm together so SQLite replays the WAL on first open).
  ```bash
  D="$BACKUP/src/nexus/nexus/data"
  sudo install -d -o nexus -g nexus -m 0750 /var/lib/nexus
  sudo cp "$D"/nexus.db "$D"/nexus.db-wal "$D"/nexus.db-shm /var/lib/nexus/ 2>/dev/null || sudo cp "$D"/nexus.db /var/lib/nexus/
  sudo cp "$D"/ledger.db /var/lib/nexus/
  sudo chown nexus:nexus /var/lib/nexus/*.db*
  sudo restorecon -Rv /var/lib/nexus
  # Insurance before first boot (older DB → schema migration runs on open):
  sudo -u nexus cp /var/lib/nexus/nexus.db /var/lib/nexus/nexus.db.pre-migrate
  ```

- [ ] **M2: Restore the aspect homes** (carry personalities + per-aspect `.mcp.json`, incl. wren→unity-mcp).
  ```bash
  sudo install -d -o nexus -g nexus -m 0750 /var/lib/nexus/aspects
  sudo cp -a "$BACKUP"/src/nexus/nexus/agents/. /var/lib/nexus/aspects/
  sudo chown -R nexus:nexus /var/lib/nexus/aspects
  sudo restorecon -Rv /var/lib/nexus/aspects
  ```
  (Keel's home comes along too → the broker embeds it as the Frame. **No `keel` keyfile needed** — the Frame runs in-process.)

- [ ] **M3: TLS + units + boot** — do Task 2.3 (Tailscale cert) + Task 3.2 (slice + `nexus.service`), then start.

- [ ] **M4: Verify identity continuity** — the boot MUST be the *same* Nexus.
  ```bash
  journalctl -u nexus.service -n 40 --no-pager | grep -iE "nexus identity loaded|ledger service initialised|frame"
  curl -sk "https://$DMON_TS:7888/api/nexus_id"; echo
  ```
  Expected: logs show `nexus identity loaded nexus_id=7a5f2d56-…`; `/api/nexus_id` returns `$NEXUS_ID`. If it shows a *different* id, STOP — the wrong DB was restored.

- [ ] **M5: Re-mint the 6 connecting aspects** (not keel — embedded). Restored `nexus.db` already has the aspect rows; `mint --force` bumps version + replaces the pubkey in-place and stamps the **new** URL.
  ```bash
  for a in anvil wren forge maren verity harrow; do
    sudo -u nexus env NEXUS_DATA_DIR=/var/lib/nexus \
      nexus aspect mint "$a" --force --data-dir /var/lib/nexus \
        --nexus-url "wss://$DMON_TS:7888/connect" -o /tmp/$a.keyfile.json
    sudo install -o "$a" -g "$a" -m 0600 /tmp/$a.keyfile.json /home/$a/keyfile.json
    sudo rm -f /tmp/$a.keyfile.json
  done
  ```
  > If the broker is already running, the offline direct-DB mint above can race the live writer — either stop `nexus.service` for the mint loop, or use the `--via https://$DMON_TS:7888 --admin-token <token>` broker-single-writer path instead. Decide live based on whether you want zero downtime.

- [ ] **M6:** Continue at Phase 4.1 (**rotate the leaked DeepSeek key** in the restored cred store), then enable the `aspect@` services (Phase 5.3 / 6) — keyfiles are already in place from M5.

---

## Phase 0 — Host prep (Fedora Workstation 44)

### Task 0.1: Remote-manageable host hardening (never suspend / never lock you out)

> dMon is managed remotely over SSH and the GUI is flaky on this hardware — GNOME/**Wayland** froze once already with Unity open on the very-new RTX 5090 driver (boot log shows ASUS SBIOS power-handler NVRM warnings — the ROG ACPI quirk family). SSH (`sshd` + Tailscale SSH) is **independent of the GUI** (confirmed: SSH stayed up through the freeze), so the desktop locking/freezing never costs SSH. The two things that *would* cost remote access are **suspend** (a suspended laptop drops off the tailnet) and **node-key expiry** (Task 0.2). Make the host never suspend / never lock / never blank, and always recoverable over SSH.

**Files:** Create `/etc/systemd/logind.conf.d/nexus-noSuspend.conf` + `/etc/dconf/db/local.d/00-nexus-server`

- [ ] **Step 1: Confirm the host.**
  Run: `cat /etc/fedora-release && uname -r && getenforce`
  Expected: `Fedora ... 44 ...`, a 6.x kernel, `Enforcing`.

- [ ] **Step 2: Stop lid-close / idle suspend (logind — always-applies, session-independent).**
  Create `/etc/systemd/logind.conf.d/nexus-noSuspend.conf`:
  ```ini
  [Login]
  HandleLidSwitch=ignore
  HandleLidSwitchExternalPower=ignore
  HandleLidSwitchDocked=ignore
  IdleAction=ignore
  ```
  Then: `sudo systemctl restart systemd-logind`
  Also mask sleep targets: `sudo systemctl mask sleep.target suspend.target hibernate.target hybrid-sleep.target`

- [ ] **Step 3: GNOME defaults system-wide via dconf (no auto-suspend / no lock / no blank).**
  Fedora **Workstation** auto-suspends on AC idle and locks the screen by default (`sleep-inactive-ac-type='suspend'`), *on top of* logind. Set these system-wide (not per-session) so they survive GUI restarts and need no working desktop to apply.
  Create `/etc/dconf/db/local.d/00-nexus-server`:
  ```ini
  [org/gnome/settings-daemon/plugins/power]
  sleep-inactive-ac-type='nothing'
  sleep-inactive-battery-type='nothing'

  [org/gnome/desktop/session]
  idle-delay=uint32 0

  [org/gnome/desktop/screensaver]
  lock-enabled=false
  idle-activation-enabled=false
  ```
  Then: `sudo dconf update`

- [ ] **Step 4: Verify + know the GUI-recovery lever.**
  Run: `systemctl status sleep.target | head -3`
  Expected: `Loaded: masked`.
  Remember the recovery path: if the desktop ever wedges, **`sudo systemctl restart gdm` over SSH** respawns it without a reboot (or `loginctl terminate-session <n>` for just the hung seat) — no physical console needed.

- [ ] **Step 5 (optional): Wayland → Xorg fallback if the desktop keeps freezing under Unity.**
  Nvidia + heavy-GPU apps (Unity) are steadier on Xorg than Wayland right now. If freezes recur, set `WaylandEnable=false` under `[daemon]` in `/etc/gdm/custom.conf`, then `sudo systemctl restart gdm` and log back into "GNOME on Xorg". This directly reduces how often you'd need the Step-4 recovery lever.

### Task 0.2: Tailscale up + remote-access durability

- [ ] **Step 1: Install + bring up Tailscale, with Tailscale SSH.**
  Run: `sudo dnf install -y tailscale && sudo systemctl enable --now tailscaled && sudo tailscale up --ssh`
  (Authenticate in the browser to the `tail41686e` tailnet.) `--ssh` makes the node accept tailnet-identity SSH (works through NAT/IP changes) as a robust path alongside `sshd`. Verify it's on: `tailscale debug prefs | grep RunSSH` → `"RunSSH": true`.

- [ ] **Step 2: Verify + capture the MagicDNS name.**
  Run: `tailscale status --self --json | grep -i dnsname`
  Expected: `dmonextreme.tail41686e.ts.net` — this is `$DMON_TS` (Task 0.5).

- [ ] **Step 3: Disable node-key expiry (in the admin console) — REQUIRED for an always-on remote box.**
  In https://login.tailscale.com/admin/machines → `dmonextreme` → ⋯ → **Disable key expiry**. Otherwise the node key expires (~180 days), the box silently drops off the tailnet, and re-auth needs interactive access *at the machine* — exactly what you can't do remotely. Verify the machine shows "Expiry disabled".

### Task 0.3: GPU + asusctl (keep dGPU on for Unity)

- [ ] **Step 1: RPM Fusion + Nvidia driver** (needed for Unity + gaming; the dGPU stays available — do NOT park it integrated-only).
  Run:
  ```bash
  sudo dnf install -y \
    https://mirrors.rpmfusion.org/free/fedora/rpmfusion-free-release-$(rpm -E %fedora).noarch.rpm \
    https://mirrors.rpmfusion.org/nonfree/fedora/rpmfusion-nonfree-release-$(rpm -E %fedora).noarch.rpm
  sudo dnf install -y akmod-nvidia xorg-x11-drv-nvidia-cuda
  ```
  Wait ~5 min for the akmod to build, then reboot.

- [ ] **Step 2: Verify the dGPU is live.**
  Run: `nvidia-smi`
  Expected: the Nvidia GPU listed, driver loaded.

- [ ] **Step 3 (optional): asusctl for fan/thermal profiles.**
  Run: `sudo dnf copr enable -y lukenukem/asus-linux && sudo dnf install -y asusctl supergfxctl && sudo systemctl enable --now asusd`
  Leave `supergfxctl` on Hybrid (default) — Unity wants the dGPU. Verify: `asusctl profile -p`.

### Task 0.4: Build toolchain + firewalld zone

- [ ] **Step 1: Install build deps.**
  Run: `sudo dnf install -y golang git sqlite policycoreutils-python-utils`
  (`policycoreutils-python-utils` provides `semanage` for Phase 1 SELinux labels.)

- [ ] **Step 2: Put the broker port on the tailscale zone, not public.**
  Run:
  ```bash
  sudo firewall-cmd --permanent --zone=trusted --add-interface=tailscale0
  sudo firewall-cmd --permanent --zone=trusted --add-port=7888/tcp
  sudo firewall-cmd --reload
  ```

- [ ] **Step 3: Verify the public zone does NOT expose 7888.**
  Run: `sudo firewall-cmd --zone=public --list-ports`
  Expected: `7888/tcp` NOT listed (it's only on `trusted`/tailscale0).

### Task 0.5: Pin the tailnet hostname for the rest of the plan

- [ ] **Step 1: Export `DMON_TS` for this shell session** (used in many tasks below).
  Run: `export DMON_TS=$(tailscale status --self --json | python3 -c 'import sys,json;print(json.load(sys.stdin)["Self"]["DNSName"].rstrip("."))')`
  Verify: `echo "$DMON_TS"` prints dMon's `*.ts.net` name.

---

## Phase 1 — Service accounts, groups, data dir, SELinux

### Task 1.1: Create the `nexus` service account + data dir

- [ ] **Step 1: Create the account (no login shell, own home).**
  Run: `sudo useradd --system --create-home --home-dir /var/lib/nexus --shell /usr/sbin/nologin nexus`

- [ ] **Step 2: Create data + aspects dirs.**
  Run:
  ```bash
  sudo install -d -o nexus -g nexus -m 0750 /var/lib/nexus
  sudo install -d -o nexus -g nexus -m 0750 /var/lib/nexus/aspects
  ```

- [ ] **Step 3: Label the data dir for SELinux** (it holds the DBs + the broker writes here; give it a context systemd services may write).
  Run:
  ```bash
  sudo semanage fcontext -a -t var_lib_t "/var/lib/nexus(/.*)?"
  sudo restorecon -Rv /var/lib/nexus
  ```
  Verify: `ls -Zd /var/lib/nexus` shows `var_lib_t`.

### Task 1.2: Create the aspect users

- [ ] **Step 1: Create one system user per aspect** (non-interactive service identities).
  Run:
  ```bash
  for a in anvil wren forge maren verity harrow; do
    sudo useradd --system --create-home --home-dir /home/$a --shell /usr/sbin/nologin $a
  done
  ```

- [ ] **Step 2: Verify.**
  Run: `for a in anvil wren forge maren verity harrow; do id $a; done`
  Expected: each prints a uid/gid.

### Task 1.3: Create the shared `wakestone` group (wren ↔ operator)

> Operator login = `jacinta`.

- [ ] **Step 1: Create the group; add the operator + wren.**
  Run:
  ```bash
  sudo groupadd wakestone
  sudo usermod -aG wakestone jacinta
  sudo usermod -aG wakestone wren
  ```

- [ ] **Step 2: Create the shared project root, setgid so new files inherit the group.**
  Run:
  ```bash
  sudo install -d -o jacinta -g wakestone -m 2775 /srv/wakestone
  sudo semanage fcontext -a -t user_home_t "/srv/wakestone(/.*)?"
  sudo restorecon -Rv /srv/wakestone
  ```

- [ ] **Step 3: Verify setgid + membership.**
  Run: `ls -ld /srv/wakestone && id wren`
  Expected: dir mode `drwxrwsr-x` (the `s`), `wakestone` in wren's groups. (Group membership applies on the user's next service start / login.)

### Task 1.4: Repo-clone policy (per-user, on-demand)

**Decision:** code repos are **cloned per aspect, into that aspect's own `~/Source`, only the repos it actually uses** — NOT a central shared folder.

- **Why per-user, not central:** aspects do real dev (branch, edit, leave uncommitted state, build, PR). A shared working tree can hold only one branch + one set of changes at a time, so two aspects in it stomp each other. Separate clones give each aspect its own branch / dirty state / build — the point of the per-OS-user isolation. A central folder writable by all six would also need a setgid group *per repo* (like `wakestone`) — lots of plumbing for negative benefit. Disk is a non-issue (1.9 TB, ~12 G used).
- **The one shared-tree exception is the Unity/WakeStone project** (Task 1.3's `wakestone` group) — shared *because* Unity forces a single editor/working-copy, not as the general pattern.
- **"Only what it uses":** e.g. anvil → `nexus`, `bridle`; forge/wren → the game repos; harrow → `research`. Each aspect (or an operator setup step) clones into its own `~/Source/` as needed; nothing is pre-provisioned centrally.

- [ ] **Step 1:** No central action — clones happen per aspect under `/home/<aspect>/Source/`. (Aspects with build/dev duties clone on first use; or seed a starter clone per aspect during its Phase 5/6 setup.)

- [ ] **Step 2 (optional efficiency — skip unless disk/bandwidth matters):** a shared read-only Go module cache so aspects don't each re-download deps.
  ```bash
  sudo install -d -o root -g nexus -m 2775 /var/cache/gomod
  # then add `Environment=GOMODCACHE=/var/cache/gomod` to aspect@.service (and the build step),
  # with the aspect users in a group that can read it. (Or leave per-user GOMODCACHE — simplest.)
  ```
  Later option (not now): a local bare **git reference mirror** + `git clone --reference` so per-user clones share objects without sharing working trees.

---

## Phase 2 — Build + bootstrap nexus

### Task 2.1: Clone + build the binaries

- [ ] **Step 1: Clone into a shared build checkout.**
  Run: `sudo git clone https://github.com/CarriedWorldUniverse/nexus $SRC && sudo chown -R $(whoami) $SRC`

- [ ] **Step 2: Build broker + agentfunnel, install to /usr/local/bin.**
  Run:
  ```bash
  cd $SRC
  go build -o /tmp/nexus ./nexus/cmd/nexus
  go build -o /tmp/agentfunnel ./runtime/cmd/agentfunnel
  sudo install -m 0755 /tmp/nexus /usr/local/bin/nexus
  sudo install -m 0755 /tmp/agentfunnel /usr/local/bin/agentfunnel
  ```

- [ ] **Step 3: Verify.**
  Run: `nexus --help 2>&1 | head -3 ; agentfunnel -h 2>&1 | head -3`
  Expected: both print usage without "command not found".

### Task 2.2: Bootstrap the hub state (as the `nexus` user)

> All three run as `nexus` against `$NEXUS_DATA`. Order matters: `init` → `identity` → `cert`.

- [ ] **Step 1: Initialise the DB + data dir.**
  Run: `sudo -u nexus env NEXUS_DATA_DIR=/var/lib/nexus nexus init`
  Expected: creates `/var/lib/nexus/nexus.db`. Verify: `sudo ls -l /var/lib/nexus/nexus.db`.

- [ ] **Step 2: Initialise identity** (session-signing secret etc. — separate, required).
  Run: `sudo -u nexus env NEXUS_DATA_DIR=/var/lib/nexus nexus identity init`
  Expected: identity material written under the data dir. Verify the command exits 0.

- [ ] **Step 3: Capture the admin token.**
  The bootstrap prints / stores an admin bearer token. Record it as `NEXUS_TOKEN` for Task 3. (If `nexus init` printed it, copy it now; otherwise retrieve via `nexus operator`/`admin` per its `--help`.)

### Task 2.3: TLS via a real Tailscale cert (no self-signed)

> The old `agentnetwork` certs in the backup (`src/nexus/certs/`) are dead (wrong name) — this is a **fresh** cert for `dmonextreme.tail41686e.ts.net`. The broker requires its own TLS (`-tls-cert` is mandatory), so the broker **holds** the cert (we do not front it with `tailscale serve`).

- [ ] **Step 0 (PREREQUISITE — `tailscale cert` fails without it): enable HTTPS certs in the tailnet.**
  In the Tailscale admin console (https://login.tailscale.com/admin/dns): confirm **MagicDNS** is on and toggle **HTTPS Certificates** ON for the tailnet. Verify on dMon:
  Run: `tailscale cert "$DMON_TS" --cert-file /tmp/_probe.crt --key-file /tmp/_probe.key && echo "certs enabled" && rm -f /tmp/_probe.*`
  Expected: succeeds (`certs enabled`). If it errors with "HTTPS not enabled", fix it in the admin console first.

- [ ] **Step 1: Mint a Tailscale cert for dMon's name.**
  Run:
  ```bash
  sudo -u nexus mkdir -p /var/lib/nexus/tls
  sudo tailscale cert --cert-file /var/lib/nexus/tls/broker.crt --key-file /var/lib/nexus/tls/broker.key "$DMON_TS"
  sudo chown -R nexus:nexus /var/lib/nexus/tls && sudo chmod 0640 /var/lib/nexus/tls/broker.key
  sudo restorecon -Rv /var/lib/nexus/tls
  ```

- [ ] **Step 2: Verify the cert CN matches `$DMON_TS`.**
  Run: `openssl x509 -in /var/lib/nexus/tls/broker.crt -noout -subject -enddate`
  Expected: subject contains `$DMON_TS`; note the `notAfter` — Tailscale/LE certs are **~90-day**, so renewal (Task 8.4) is mandatory.

> Because this is a real cert for the MagicDNS name, **every** aspect (localhost and remote) connects via `wss://$DMON_TS:7888` and validates normally — no keyfile cert-pinning (NEX-367) needed here. Keyfile-pinning stays the fallback for hosts without a tailnet cert (e.g. the work laptop / pure self-signed).

---

## Phase 3 — `nexus.service`: broker + Keel Frame + ledger

### Task 3.1: Create Keel's frame-role aspect home

- [ ] **Step 1: Lay down Keel's home** (the embedded Frame's personality source) under the broker's aspect-dir.
  Run: `sudo -u nexus install -d -m 0750 /var/lib/nexus/aspects/keel`

- [ ] **Step 2: Write Keel's `aspect.json` with `role: "frame"`.**
  Create `/var/lib/nexus/aspects/keel/aspect.json` (owned by `nexus`):
  ```json
  {
    "name": "keel",
    "role": "frame",
    "context_mode": "global",
    "provider": "claude-code",
    "provider_config": {},
    "capabilities": []
  }
  ```
  Add Keel's `NEXUS.md` / `SOUL.md` / `PRIMER.md` personality files in the same dir (copy from the existing Keel home if one exists; otherwise minimal stubs to start).

- [ ] **Step 3: Verify discovery.**
  Run: `sudo -u nexus env NEXUS_DATA_DIR=/var/lib/nexus nexus aspect list --aspect-dir /var/lib/nexus/aspects 2>&1 | head` (or the equivalent per `nexus aspect --help`).
  Expected: `keel` shown with role `frame`.

### Task 3.2: Create the `nexus.slice` + `nexus.service`

**Files:** Create `/etc/systemd/system/nexus.slice`, `/etc/systemd/system/nexus.service`

- [ ] **Step 1: Create the resource-capped slice** (the polite-tenant guarantee).
  Create `/etc/systemd/system/nexus.slice`:
  ```ini
  [Unit]
  Description=nexus hub slice (broker + ledger + aspects) — yields to the interactive session
  Before=slices.target

  [Slice]
  CPUWeight=20
  MemoryHigh=4G
  MemoryMax=6G
  ```
  (Starting values — tune in Task 8. Default user/system weight is 100, so 20 means the hub gets ~1/5 the CPU share of your session under contention.)

- [ ] **Step 2: Create the broker unit** (token + RPID via env; data dir + cert via flags; in the slice).
  Create `/etc/systemd/system/nexus.service`:
  ```ini
  [Unit]
  Description=nexus broker + Keel Frame + ledger
  After=network-online.target tailscaled.service
  Wants=network-online.target

  [Service]
  User=nexus
  Group=nexus
  Slice=nexus.slice
  Environment=NEXUS_DATA_DIR=/var/lib/nexus
  Environment=NEXUS_TOKEN=PASTE_ADMIN_TOKEN_FROM_TASK_2.2
  Environment=NEXUS_OPERATOR_RPID=DMON_TS_NAME_HERE
  Environment=NEXUS_ASPECT_DIR=/var/lib/nexus/aspects
  ExecStart=/usr/local/bin/nexus -addr :7888 \
    -data-dir /var/lib/nexus \
    -tls-cert /var/lib/nexus/tls/broker.crt \
    -tls-key /var/lib/nexus/tls/broker.key \
    -aspect-dir /var/lib/nexus/aspects
  Restart=on-failure
  RestartSec=3
  # Hardening (Fedora/systemd):
  NoNewPrivileges=true
  ProtectSystem=strict
  ReadWritePaths=/var/lib/nexus
  ProtectHome=true

  [Install]
  WantedBy=multi-user.target
  ```
  Replace `PASTE_ADMIN_TOKEN_FROM_TASK_2.2` and `DMON_TS_NAME_HERE` (the literal `$DMON_TS` value, e.g. `dmon.tailXXXX.ts.net`). RPID is the bare hostname for WebAuthn (operator login); it is now optional (NEX-368) but set it so operator login works.

- [ ] **Step 3: Enable + start.**
  Run: `sudo systemctl daemon-reload && sudo systemctl enable --now nexus.service`

- [ ] **Step 4: Verify the broker is up, in the slice, with ledger healthy.**
  Run:
  ```bash
  systemctl status nexus.service --no-pager | head -6
  systemctl status nexus.slice --no-pager | head -4
  curl -sk "https://$DMON_TS:7888/healthz/ledger" ; echo
  journalctl -u nexus.service -n 30 --no-pager | grep -iE "ledger service initialised|frame|listening"
  ```
  Expected: service `active (running)`; the process shown under `nexus.slice`; ledger healthz returns OK; logs show "ledger service initialised" + the embedded Keel Frame building + the broker listening.

> **Checkpoint:** the hub is now live. Phases 5–7 are additive.

---

## Phase 4 — Provider credentials (+ rotate the live key)

### Task 4.1: Rotate the leaked DeepSeek key

- [ ] **Step 1:** In the DeepSeek console, **revoke** the key currently hardcoded in `start-plumb.sh` and **issue a fresh one.** Do not paste any key into a file in the repo. (Per the hosting design §7.)

### Task 4.2: Load provider credentials into the broker store

> `credential` flags come BEFORE the positional name, and the data dir must be set (env or `--data-dir`).

- [ ] **Step 1: Add the DeepSeek (Anthropic-shape) provider credential** used for cheap judging / DeepSeek-backed aspects.
  Run (paste the fresh key bundle on stdin; `base_url` MUST be non-empty):
  ```bash
  sudo -u nexus env NEXUS_DATA_DIR=/var/lib/nexus \
    nexus credential set --kind provider --bundle-stdin deepseek <<'JSON'
  {"api_shape":"anthropic","base_url":"https://api.deepseek.com/anthropic","key":"PASTE_FRESH_DEEPSEEK_KEY","default_model":"deepseek-v4-flash"}
  JSON
  ```

- [ ] **Step 2: Add the Anthropic credential** (for claude-api aspects, if any).
  Run:
  ```bash
  sudo -u nexus env NEXUS_DATA_DIR=/var/lib/nexus \
    nexus credential set --kind provider --bundle-stdin anthropic <<'JSON'
  {"api_shape":"anthropic","base_url":"https://api.anthropic.com","key":"PASTE_ANTHROPIC_KEY","default_model":"claude-haiku-4-5-20251001"}
  JSON
  ```

- [ ] **Step 3: Verify (metadata only — secrets never printed).**
  Run: `sudo -u nexus env NEXUS_DATA_DIR=/var/lib/nexus nexus credential list`
  Expected: `deepseek` + `anthropic` listed.

> Aspect→credential defaults are set per aspect in Phase 5/6 (`nexus aspect-default`). Network-wide judge defaults (provider/model/credential) can be set later via the dashboard Settings → Network defaults panel (the NEX-365 #3 surface).

---

## Phase 5 — First aspect: anvil (proves the per-user pattern)

### Task 5.1: Lay down anvil's broker-side personality home

- [ ] **Step 1:** Create `/var/lib/nexus/aspects/anvil/` (owned by `nexus`) with `aspect.json` (`name: "anvil"`, `role` omitted/`aspect`, `provider`, `context_mode`) + `NEXUS.md`/`SOUL.md`/`PRIMER.md`. Mirror Keel's `aspect.json` shape (Task 3.1) minus the frame role; set `provider` to whatever anvil should run (e.g. `claude-code` or `claude-api`).

- [ ] **Step 2:** Set anvil's default provider credential.
  Run: `sudo -u nexus env NEXUS_DATA_DIR=/var/lib/nexus nexus aspect-default --anthropic anthropic anvil`
  (Use `--openai`/`--anthropic` per the aspect's api_shape; here routing anvil to the `anthropic` credential.)

### Task 5.2: Mint anvil's keyfile (its identity) into anvil's homedir

- [ ] **Step 1: Mint, addressed to the tailnet broker URL.**
  Run:
  ```bash
  sudo -u nexus env NEXUS_DATA_DIR=/var/lib/nexus \
    nexus aspect mint --data-dir /var/lib/nexus --nexus-url "wss://$DMON_TS:7888" anvil -o /tmp/anvil.keyfile.json
  sudo install -o anvil -g anvil -m 0600 /tmp/anvil.keyfile.json /home/anvil/keyfile.json
  sudo rm /tmp/anvil.keyfile.json
  ```
  (Real tailnet cert → no `broker_tls_cert` pin needed; mint still embeds the nexus_url + nexus_id envelope.)

- [ ] **Step 2: Verify perms.**
  Run: `sudo ls -l /home/anvil/keyfile.json`
  Expected: `-rw------- anvil anvil`.

### Task 5.3: Create the `aspect@.service` template + start anvil

**Files:** Create `/etc/systemd/system/aspect@.service`

- [ ] **Step 1: Create the template** (parameterised by username = aspect id).
  Create `/etc/systemd/system/aspect@.service`:
  ```ini
  [Unit]
  Description=nexus aspect %i (agentfunnel)
  After=nexus.service network-online.target
  Wants=network-online.target
  Requires=nexus.service

  [Service]
  User=%i
  Group=%i
  Slice=nexus.slice
  WorkingDirectory=/home/%i
  ExecStart=/usr/local/bin/agentfunnel -k /home/%i/keyfile.json
  Restart=on-failure
  RestartSec=5
  NoNewPrivileges=true
  ProtectSystem=strict
  ReadWritePaths=/home/%i
  [Install]
  WantedBy=multi-user.target
  ```
  (Keyless: agentfunnel gets personality/provider/model from the broker's validate handshake. `provider_env` is broker-resolved per NEX-332 phase 4 — no creds in the unit. Once #213 merges, append `-auto-recall` here to enable Commonplace recall.)

- [ ] **Step 2: Enable + start anvil.**
  Run: `sudo systemctl daemon-reload && sudo systemctl enable --now aspect@anvil.service`

- [ ] **Step 3: Verify anvil connected + took a turn.**
  Run:
  ```bash
  journalctl -u aspect@anvil.service -n 40 --no-pager | grep -iE "validated|keyfile loaded|connected|aspect anvil"
  ```
  Expected: "keyfile loaded" → "validated" (aspect=anvil, provider, model) → connected. Then from the dashboard or a chat seed addressed to `@anvil`, confirm a reply appears. (Roster: `curl -sk https://$DMON_TS:7888/...` per the roster endpoint, or the dashboard Status view.)

---

## Phase 6 — Remaining aspects (forge, maren, verity, harrow)

### Task 6.1: Replicate the anvil pattern for each

- [ ] **Step 1:** For each of `forge maren verity harrow`: create its broker-side home (Task 5.1 step 1), set its `aspect-default` credential (Task 5.1 step 2), mint its keyfile into `/home/<a>/keyfile.json` (Task 5.2), then `sudo systemctl enable --now aspect@<a>.service`.
  (wren is Phase 7 — it needs the Unity wiring first.)

- [ ] **Step 2: Verify all five non-wren aspects are live.**
  Run: `systemctl status 'aspect@*' --no-pager | grep -E "aspect@|Active" | head -20`
  Expected: `aspect@anvil/forge/maren/verity/harrow` all `active (running)`.

---

## Phase 7 — wren + the shared Unity editor

### Task 7.1: wren's broker-side home + credential + keyfile

- [ ] **Step 1:** Create `/var/lib/nexus/aspects/wren/` home (Task 5.1), set its `aspect-default` credential, mint `/home/wren/keyfile.json` (Task 5.2). Do **not** start `aspect@wren` yet.

### Task 7.2: Move the WakeStone project into the shared group dir

> The project currently lives at **`/home/jacinta/Project/WakeStone`** (the operator's home), so `wren` can't write to it. wren is a coding aspect — it wants to write C# directly with its file tools (not route every edit through the MCP bridge) — so the project moves to the shared, group-writable `wakestone` tree (created setgid in Task 1.3), out of the operator's home (no home-traversal grant needed for wren).

- [ ] **Step 1: Move it (with Unity CLOSED — don't move a project with the editor holding locks / churning `Library/`).**
  Run:
  ```bash
  sudo mv /home/jacinta/Project/WakeStone /srv/wakestone/WakeStone
  sudo chgrp -R wakestone /srv/wakestone/WakeStone
  sudo chmod -R g+rwX /srv/wakestone/WakeStone
  sudo find /srv/wakestone/WakeStone -type d -exec chmod g+s {} +
  sudo restorecon -Rv /srv/wakestone/WakeStone
  ```
  Git is path-relative — `.git` moves with the tree, remotes/branches unaffected. Afterward, re-add the project in **Unity Hub** from `/srv/wakestone/WakeStone`.
  > Alternative (keep it in your home): leave it at `~/Project/WakeStone`, make it setgid `wakestone`, and `sudo setfacl -m u:wren:x /home/jacinta /home/jacinta/Project` so wren can traverse in. Works, but opens a path into your home — the move is cleaner.

- [ ] **Step 2: Set `umask 002` for both identities** so new files stay group-writable.
  Append `umask 002` to `/home/wren/.bashrc` (wren) and to the operator's shell rc; for the wren *service*, add `UMask=0002` under `[Service]` in a drop-in: `sudo systemctl edit aspect@wren.service` → add `[Service]\nUMask=0002`.

- [ ] **Step 3: Verify cross-write.**
  Run: `sudo -u wren bash -c 'touch /srv/wakestone/.wren_write_test' && ls -l /srv/wakestone/.wren_write_test`
  Expected: file created, group `wakestone`, group-writable. (Then remove it.)

### Task 7.3: Install Unity for the shared editor (operator-driven)

> **Operator input:** which Unity editor version the WakeStone project targets.

- [ ] **Step 1:** Install Unity Hub (the official `.AppImage` or the Unity dnf repo) **in the operator account**, install the project's editor version, and activate the licence (Personal or Pro) **as the operator** — the operator launches the editor.
- [ ] **Step 2: Verify** the editor opens `/srv/wakestone/<project>` and the operator can enter play mode.

### Task 7.4: Wire the Unity MCP bridge (operator-driven)

> **Operator input:** which Unity MCP bridge/package (e.g. an editor-package + local MCP server). Install its editor package into the WakeStone project; note the local MCP endpoint it exposes (host/port or stdio command) while the editor runs.

- [ ] **Step 1:** Add the chosen Unity MCP package to the project; start the editor so the MCP server is listening.
- [ ] **Step 2: Verify** the MCP endpoint is reachable on localhost while the editor is open (e.g. `curl`/handshake per the bridge's docs).

### Task 7.5: Give wren the Unity MCP profile + start it

- [ ] **Step 1:** Add the Unity MCP server to wren's broker-side MCP profile so wren's funnel materialises it into `.mcp.json` at bind (NEX-170). Set it via the broker's MCP-profile admin surface for `wren` (per `nexus admin --help` / the dashboard), pointing at the localhost endpoint from Task 7.4.

- [ ] **Step 2: Start wren.**
  Run: `sudo systemctl enable --now aspect@wren.service`

- [ ] **Step 3: Verify the collaborative loop.**
  With the editor open: from chat, ask `@wren` to make a small scripted change; confirm wren writes a `.cs` under `/srv/wakestone/<project>` (as `wren`, group-writable), Unity hot-reloads, and wren can drive a compile/play via MCP.
  With the editor **closed**: confirm `@wren` still responds to comms and reports Unity is unavailable rather than erroring/looping (the graceful-degradation check from design §6 — file a fix if it loops).

---

## Phase 8 — Backup, tuning, reboot test

### Task 8.1: Nightly DB snapshot + off-box copy

**Files:** Create `/etc/systemd/system/nexus-backup.service` + `/etc/systemd/system/nexus-backup.timer` + `/usr/local/bin/nexus-backup.sh`

- [ ] **Step 1: Backup script** (SQLite-safe `.backup`, then off-box via tailnet `scp` to little-blue when reachable).
  Create `/usr/local/bin/nexus-backup.sh`:
  ```bash
  #!/usr/bin/env bash
  set -euo pipefail
  ts=$(date +%Y%m%d-%H%M%S)
  out=/var/lib/nexus/backups; mkdir -p "$out"
  for db in nexus ledger; do
    sqlite3 "/var/lib/nexus/$db.db" ".backup '$out/$db-$ts.db'"
  done
  find "$out" -name '*.db' -mtime +14 -delete
  # Off-box (best-effort; ignore failure when little-blue is away):
  scp -q "$out"/*-"$ts".db little-blue:/path/to/nexus-backups/ || true
  ```
  `sudo install -m 0755 /usr/local/bin/nexus-backup.sh /usr/local/bin/nexus-backup.sh` and `sudo chown nexus:nexus` the script if run as nexus.

- [ ] **Step 2: Service + timer** (`nexus-backup.service` `Type=oneshot`, `User=nexus`, `ExecStart=/usr/local/bin/nexus-backup.sh`; `nexus-backup.timer` `OnCalendar=*-*-* 03:00:00`, `Persistent=true`). Enable: `sudo systemctl enable --now nexus-backup.timer`.

- [ ] **Step 3: Verify.**
  Run: `sudo systemctl start nexus-backup.service && ls -l /var/lib/nexus/backups`
  Expected: fresh `nexus-*.db` + `ledger-*.db` snapshots.

### Task 8.2: Confirm the hub yields under load

- [ ] **Step 1:** With the aspects running, open Unity + run a build while watching `systemd-cgtop`.
  Run: `systemd-cgtop` and watch `nexus.slice` CPU stay capped vs your session.
  Expected: under contention, `nexus.slice` is throttled (the low `CPUWeight`) and the editor stays responsive. Tune `CPUWeight`/`MemoryHigh` in `/etc/systemd/system/nexus.slice` if needed → `sudo systemctl daemon-reload`.

### Task 8.4: Tailscale cert auto-renewal (the broker's TLS expires ~90 days)

> Without this the broker's cert expires and every aspect drops. `tailscale cert` re-issues the LE cert; the broker reads the files at startup, so renewal = re-cert + restart (cheap — aspects reconnect). Run monthly so renewal lands well inside the 90-day window.

**Files:** Create `/usr/local/bin/nexus-cert-renew.sh` + `/etc/systemd/system/nexus-cert-renew.service` + `.timer`

- [ ] **Step 1: Renewal script** (re-cert into the same paths, then restart the broker).
  Create `/usr/local/bin/nexus-cert-renew.sh`:
  ```bash
  #!/usr/bin/env bash
  set -euo pipefail
  DMON_TS="dmonextreme.tail41686e.ts.net"
  tailscale cert --cert-file /var/lib/nexus/tls/broker.crt --key-file /var/lib/nexus/tls/broker.key "$DMON_TS"
  chown nexus:nexus /var/lib/nexus/tls/broker.crt /var/lib/nexus/tls/broker.key
  chmod 0640 /var/lib/nexus/tls/broker.key
  restorecon /var/lib/nexus/tls/broker.crt /var/lib/nexus/tls/broker.key
  systemctl restart nexus.service
  ```
  `sudo install -m 0755 /usr/local/bin/nexus-cert-renew.sh /usr/local/bin/nexus-cert-renew.sh`

- [ ] **Step 2: Service + timer** (`nexus-cert-renew.service` `Type=oneshot`, runs as **root** — `tailscale cert` + `systemctl restart` need it; `ExecStart=/usr/local/bin/nexus-cert-renew.sh`. `nexus-cert-renew.timer` `OnCalendar=*-*-01 04:00:00`, `Persistent=true`). Enable: `sudo systemctl enable --now nexus-cert-renew.timer`.

- [ ] **Step 3: Verify** the timer is scheduled + a manual run renews + the broker comes back.
  Run: `sudo systemctl start nexus-cert-renew.service && systemctl is-active nexus.service && openssl x509 -in /var/lib/nexus/tls/broker.crt -noout -enddate`
  Expected: broker `active`, a fresh `notAfter` ~90 days out. `systemctl list-timers nexus-cert-renew.timer` shows the next monthly run.

### Task 8.3: Reboot survival

- [ ] **Step 1:** `sudo reboot`. After it comes back (without logging into GNOME), verify everything auto-started:
  Run:
  ```bash
  systemctl is-active nexus.service
  systemctl is-active 'aspect@anvil' 'aspect@forge' 'aspect@maren' 'aspect@verity' 'aspect@harrow' 'aspect@wren'
  curl -sk "https://$DMON_TS:7888/healthz/ledger"; echo
  ```
  Expected: `nexus.service` active, all aspects active, ledger healthy — **with no graphical login** (proves the always-on, session-independent model).

---

## Phase 9 — Reconcile docs

### Task 9.1: Fix the design-doc Keel discrepancy

- [ ] **Step 1:** Update `docs/2026-05-29-nexus-hosting-dmon-design.md` §2/§4 to state that **Keel runs embedded in `nexus.exe` (as the `nexus` user)**, not as a separate `keel` OS user — Frame + ledger are in-process with the broker. Adjust the topology box accordingly. Commit.

---

## Self-review notes (already applied)

- **Coverage vs design doc:** host/Fedora (§3)→Phase 0; service accounts + wakestone group (§4/§4a)→Phase 1; per-OS-user aspects + `aspect@` template + `nexus.slice` (§5)→Phases 5–7 + 3.2; wren/Unity (§6)→Phase 7; creds + key rotation (§7)→Phase 4; backup (§9)→Phase 8.1. The §12 open items are resolved here: one unit (ledger in-process), Option-A autostart, SELinux contexts in Phases 1–3, slice values in 3.2/8.2, incremental first-cut via phase ordering, Unity MCP as Task 7.4/7.5.
- **Cellular obsforward guardrail (design §8)** is a *little-blue* concern, out of scope for this dMon plan — tracked separately.
- **`-auto-recall`** (Commonplace, PR #213) is noted in Task 5.3 as an additive flag once merged — not required for bring-up.
