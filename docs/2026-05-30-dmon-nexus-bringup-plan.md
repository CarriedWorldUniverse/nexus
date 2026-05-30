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

- `DMON_TS` = dMon's Tailscale MagicDNS name, e.g. `dmon.tailXXXX.ts.net` (set in Task 0.5).
- `NEXUS_DATA=/var/lib/nexus` = the hub data dir (owned by `nexus`).
- `ASPECTS_DIR=/var/lib/nexus/aspects` = broker-side aspect homes (owned by `nexus`).
- Broker listens on `:7888`; the broker URL everyone uses is `wss://$DMON_TS:7888`.
- `SRC=/usr/local/src/nexus` = a shared build checkout (built once, binaries installed to `/usr/local/bin`).

---

## Phase 0 — Host prep (Fedora Workstation 44)

### Task 0.1: Confirm baseline + disable suspend (always-on)

**Files:** Create `/etc/systemd/logind.conf.d/nexus-noSuspend.conf`

- [ ] **Step 1: Confirm the host.**
  Run: `cat /etc/fedora-release && uname -r && getenforce`
  Expected: `Fedora ... 44 ...`, a 6.x kernel, `Enforcing`.

- [ ] **Step 2: Stop lid-close / idle suspend** (laptop-as-server).
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

- [ ] **Step 3: Verify.**
  Run: `systemctl status sleep.target | head -3`
  Expected: `Loaded: masked`.

### Task 0.2: Tailscale up

- [ ] **Step 1: Install + bring up Tailscale.**
  Run: `sudo dnf install -y tailscale && sudo systemctl enable --now tailscaled && sudo tailscale up`
  (Authenticate in the browser to the `agentnetwork` tailnet.)

- [ ] **Step 2: Verify + capture the MagicDNS name.**
  Run: `tailscale status --self --json | grep -i dnsname`
  Expected: dMon's `*.ts.net` name. Record it — this is `$DMON_TS` (Task 0.5).

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

> Replace `OPERATOR` below with your interactive login name.

- [ ] **Step 1: Create the group; add the operator + wren.**
  Run:
  ```bash
  sudo groupadd wakestone
  sudo usermod -aG wakestone OPERATOR
  sudo usermod -aG wakestone wren
  ```

- [ ] **Step 2: Create the shared project root, setgid so new files inherit the group.**
  Run:
  ```bash
  sudo install -d -o OPERATOR -g wakestone -m 2775 /srv/wakestone
  sudo semanage fcontext -a -t user_home_t "/srv/wakestone(/.*)?"
  sudo restorecon -Rv /srv/wakestone
  ```

- [ ] **Step 3: Verify setgid + membership.**
  Run: `ls -ld /srv/wakestone && id wren`
  Expected: dir mode `drwxrwsr-x` (the `s`), `wakestone` in wren's groups. (Group membership applies on the user's next service start / login.)

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

- [ ] **Step 1: Mint a Tailscale cert for dMon's name.**
  Run:
  ```bash
  sudo -u nexus mkdir -p /var/lib/nexus/tls
  sudo tailscale cert --cert-file /var/lib/nexus/tls/broker.crt --key-file /var/lib/nexus/tls/broker.key "$DMON_TS"
  sudo chown -R nexus:nexus /var/lib/nexus/tls && sudo chmod 0640 /var/lib/nexus/tls/broker.key
  sudo restorecon -Rv /var/lib/nexus/tls
  ```

- [ ] **Step 2: Verify the cert CN matches `$DMON_TS`.**
  Run: `openssl x509 -in /var/lib/nexus/tls/broker.crt -noout -subject`
  Expected: subject contains `$DMON_TS`.

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

### Task 7.2: Put the WakeStone project in the shared group dir

- [ ] **Step 1:** Move/clone the WakeStone Unity project into `/srv/wakestone/<project>` (created setgid in Task 1.3). Ensure group-write:
  Run: `sudo chgrp -R wakestone /srv/wakestone && sudo chmod -R g+rwX /srv/wakestone && sudo find /srv/wakestone -type d -exec chmod g+s {} +`

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
