# nexus hosting: dMon as the always-on hub

**Date:** 2026-05-29 (revised 2026-05-30 — Fedora install + dev-first reframe + wren/Unity layout)
**Status:** Design — approved direction; host now installed (Fedora Workstation 44), pre-implementation-plan
**Supersedes framing of:** the "move broker to cloud EC2" idea (evaluated and set aside — see §9)
**Related tickets:** NEX-310 (dMon Windows→Linux), NEX-309 (deploy daemon / OS-native runner), NEX-304 (in-nexus scheduler), NEX-336 (broker-resolved provider creds)

> **2026-05-30 note:** this file was committed at `fcbd075` (+ follow-ups) on a branch that never merged to `main`, so it was absent from `main`; this revision restores it and captures the decisions made once dMon was actually installed.

---

## 1. Goal & framing

Get nexus broker + ledger **off the workstation reboot cycle** so the hub (tickets, comms history, ledger) stays available while the user works/games and while the second machine travels.

**Reframe 1 (the original design driver):** this is a **home hobby**, not a critical always-on state machine. The original instinct to put the hub in a cloud EC2 was solving for uptime guarantees the project doesn't need. The actual problem was only that dMon **rebooted into Windows to game**, taking the broker down. Commit dMon to **Linux, always-on, no Windows reboots**, and that problem disappears — no cloud required.

**Reframe 2 (2026-05-30, post-install):** dMon is **primarily the operator's development machine** — specifically the box where **Unity runs for the wren / WakeStone workstream** — that *also* carries the hub. It is **not** a server with a screen. That inverts the priority: the operator's interactive dev/Unity work is the box's real job, and **the hub must be a polite background tenant that never janks the editor**. This is why dMon is Fedora **Workstation** (desktop), not Server (headless).

## 2. Topology

```
┌──────────────────────────── dMon (Fedora Workstation 44, always-on) ─────────────┐
│  operator (you): GNOME desktop — dev work + Unity editor (the box's real job)     │
│                                                                                   │
│  user: nexus  →  broker + ledger  (infrastructure; systemd system services)       │
│  Keel  → the Frame (architecture / keeps the network working)                     │
│  fixed aspects (one OS user each, non-interactive service identities):            │
│    anvil  → builder                                                               │
│    wren   → Unity     ┐  (hands-on via Unity MCP — see §6; shares the editor      │
│    forge  → game AI   │   the operator launches, not a separate one)              │
│    maren  → artist    │ WakeStone project                                         │
│    verity → canon/lore┘                                                           │
│    harrow → research                                                              │
│  Frame + aspects ↔ broker over LOCALHOST                                          │
│  hub processes confined to a resource-capped `nexus.slice` (yields to your seat)  │
└───────────────────────────────────────────────────────────────────────────────-─┘
                                     ▲  Tailscale tailnet ("agentnetwork")
                                     │
┌──────────── little-blue (MacBook Neo, roaming, not always-on) ──────────┐
│  portable aspects: shadow (orchestrator), plumb (builder)               │
│    →  reach dMon broker via tailnet                                      │
│  (LAN when home; cellular when away → metered, see §8)                   │
└─────────────────────────────────────────────────────────────────────────┘
```

- **dMon** hosts the **nexus broker + ledger** (infrastructure) under the `nexus` account, **and** the network's participants: **Keel** — the **Frame** (responsible for the architecture and keeping the network working; peer to an Aspect, not infrastructure) — plus the 6 fixed aspects: **anvil** (builder); the **WakeStone** group — **wren** (Unity), **forge** (game AI), **maren** (artist), **verity** (canon/lore); and **harrow** (research). Frame and aspects talk to the broker over **localhost**. (Naming + Frame/Aspect concepts come from the **Carried World** lore.)
- **little-blue** carries the **portable aspects** **shadow** (orchestrator) and **plumb** (builder), reaching the broker over the **tailnet** — LAN at home, cellular away.
- LLM inference is **direct agent → provider** (Anthropic/DeepSeek); it never traverses the broker. The hub only carries comms/observability.

## 3. Host: dMon = Fedora Workstation 44

Installed 2026-05-30 (after the Fedora-live boot fix in NEX-310 — ACPI/MUX kernel params + `nouveau.modeset=0` + BIOS GPU MUX→Discrete). Fedora Workstation, not Server, because the box is a **dev machine first** (§1 reframe 2).

- **systemd / Tailscale / asusctl are all first-class on Fedora** — no change from the original (Pop!_OS-assumed) plan there.
- **Keep the Nvidia dGPU ON.** Earlier (headless-hub) thinking suggested `supergfxctl` integrated-only to park the dGPU; that's wrong for a Unity dev box. The dGPU stays available for the editor; the hub doesn't touch the GPU (inference is remote). `asusctl` still useful for fan/thermal profiles.
- **Fedora-specific wrinkles to get right in the plan:**
  - **SELinux is enforcing by default.** Custom data dirs (`nexus.db`/`ledger.db`), per-user homedir content, systemd unit files, and `EnvironmentFile=` cred files need correct contexts/labels (`semanage fcontext` + `restorecon`) or services fail to start in non-obvious ways. This is the main new failure mode vs a non-SELinux distro.
  - **firewalld is on.** The broker listens on localhost (no inbound rule needed for the on-box participants) and on the **tailscale0** interface for little-blue — put the broker port in the trusted/tailscale zone, not the public zone.
  - NetworkManager + GNOME are the Workstation defaults; the operator's account is the only graphical login.
- **Always-on config:** `logind` so a closed lid / idle does **not** suspend (`HandleLidSwitch=ignore`, `HandleLidSwitchExternalPower=ignore`, disable automatic sleep). The laptop-as-server step.
- Gaming, if kept, runs **on Linux** (Steam/Proton via RPM Fusion + the Nvidia driver) — no Windows dual-boot, preserving "never reboots away from the hub." Linux gaming needs no reboot, so it coexists with hosting.
- Data dir + certs migrate per NEX-310. (A Pop!_OS migration runbook drafted in dropped session `ce653f96` is now partly moot — Fedora is installed — but the data `scp` + Tailscale re-up + systemd-unit steps still apply; fold the still-relevant bits into NEX-310.)

## 4. Service accounts & the per-OS-user participant model

The pattern generalises today's `start-plumb.sh` (`~/Source/{nexus,bridle,keyfile}` + agentfunnel) to N users on one host.

- **`nexus` service account** — owns broker + ledger. Non-root, own homedir, owns `nexus.db` + `ledger.db`. No agent workload.
- **One OS user per participant** (Frame or Aspect — same level, same pattern), named for the identity: the Frame `keel` plus the 6 fixed aspects `anvil`, `wren`, `forge`, `maren`, `verity`, `harrow`. These are **non-interactive service identities** — they never sit at a GNOME login (only the operator's account does). Each has:
  - its own homedir + **its own repo clones** (`~/Source/{nexus,bridle,…}`) — agents clone/build independently without colliding on a shared tree;
  - its own **keyfile** (aspect identity);
  - its own credential env (see §7 — **not** hardcoded).
- Isolation benefit: an agent's builds, clones, and creds are sandboxed to its user; blast radius is one homedir.

### 4a. Shared WakeStone group (the wren↔operator collaboration)

wren works **hands-on in Unity, collaboratively with the operator, on one shared editor + project** (see §6). Unity locks a project to a single editor process, so wren and the operator cannot each have their own copy open — they share **one** project tree. wren stays its own isolated user (clean comms identity, keyfile, creds, homedir like the others), but the **WakeStone Unity project lives in a shared group directory**, not locked inside either homedir:

- A `wakestone` Unix group; **the operator and `wren` are both members**.
- The project lives in a shared path (e.g. **`/srv/wakestone/<project>`**), **`setgid`** on the directory tree so new files inherit the `wakestone` group, with **`umask 002`** for both the operator and wren so files stay **group-writable both ways**.
- The operator launches/runs the editor (as their user) against it; **wren writes C# / assets there (as wren)** and drives the editor over MCP. Unity (running as the operator) reads wren's files (group-readable); wren reads the operator's. SELinux: label the shared tree appropriately (see §3).
- Only `wren` (not the other aspects) joins `wakestone`. forge/maren/verity touch WakeStone via their own repos / the broker, not the live editor.

## 5. Autostart & resource confinement

**Decision (2026-05-30): system services with `User=`, under a resource-capped slice** (Option A; the alternative — lingered per-user `systemctl --user` services — was set aside as more moving parts for non-interactive identities, with isolation that the separate users already provide).

- **broker + ledger:** systemd **system** services (`nexus-broker.service`; one unit if ledger is in-process with the broker, two if separate — confirm in plan, see §12). `WantedBy=multi-user.target`, `Restart=on-failure`, `User=nexus`.
- **Frame + aspects:** a **templated `aspect@.service`** parameterised by username, `User=%i`, so adding/removing a participant is `systemctl enable --now aspect@maren`. Drops to each identity's user; starts at boot before any graphical login; one place to see everything (`systemctl status 'aspect@*'`).
- **`nexus.slice` (the polite-tenant guarantee):** put broker, ledger, Frame, and all aspect units under a single slice with a **low `CPUWeight`** and a **`MemoryHigh`/`MemoryMax`** cap, so the operator's interactive session + Unity always win contention and a runaway aspect/build can't starve the editor. This is the mechanism that makes "dev machine first, hub second" real rather than aspirational. (Aspects are mostly light locally — inference is remote — but claude-code subprocesses and local builds do compete.)
- This is the concrete home for **NEX-309** (deploy daemon / OS-native runner) and overlaps **NEX-304** (scheduler): the unit files + slice + restart policy are what NEX-309 should generate/manage.
- The operator's own GNOME session (and any Unity instance launched from it) stays **entirely outside** `nexus.slice` — it is not a managed participant; it's the box's primary workload.

## 6. wren ↔ Unity: the collaborative editor model

wren is the one participant that breaks the clean "headless system service" mold, because Unity on Linux needs a live Editor + display + GPU for hands-on (MCP-driven) work — pure `-batchmode -nographics` can't cover it.

**Decision (2026-05-30): option (iii) — one shared editor in the operator's session; wren attaches via MCP.** The operator is *often* working with wren and *often* needs to run the engine to playtest, so the work is inherently interactive/sessional and there can only be one editor. (Options (i) wren's own headless-GPU service via Xvfb+EGL, and (ii) a dedicated virtual desktop session for wren, are **shelved as non-goals** — autonomous-while-away Unity work is not a near-term need.)

- **One Unity Editor**, launched by the operator, in the operator's GNOME session (real display + GPU — no Xvfb/EGL).
- **wren attaches to that same editor via the Unity MCP bridge** + writes C# to the shared `wakestone` tree (Unity hot-reloads). wren calls MCP tools to compile, enter play mode, run tests, query the scene; the operator hits play to test. Collaborative loop, the standard Unity-AI pattern.
- wren's funnel **`.mcp.json` points at the Unity MCP server on localhost** (nexus already supports per-aspect MCP — NEX-170 materialises `.mcp.json`). That endpoint is **only live while the operator's editor is up**; wren's always-on **comms** half doesn't depend on it.
- **wren's comms half stays a normal `aspect@wren` system service** like the other six. Only its *Unity* half is sessional + operator-coupled.
- **To confirm in plan:** wren should **degrade gracefully** when the Unity MCP endpoint is down ("Unity isn't open right now") rather than erroring hard or looping — the editor is absent most of the day.

## 7. Credentials (and a live-key cleanup)

- **Drop the hardcoded key.** `start-plumb.sh` currently ships a **live DeepSeek API key** as the env fallback default (for both `ANTHROPIC_API_KEY` and `OPENAI_API_KEY`). **Rotate that key** and remove the literal — it must not live in a script, especially one copied across per-agent homedirs.
- **Broker-resolved creds (NEX-336):** agents fetch provider creds from the broker over WS at bind time rather than reading env — the right model for N users (no per-homedir secret sprawl, central rotation). The keyless-aspect delivery path (validate → `provider_env`) already exists (NEX-332 phase 4). Until NEX-336 is the default everywhere, per-user creds go in a `0600` file in each agent's homedir (not a shared script), or a systemd `EnvironmentFile=` with locked perms + correct SELinux label.

## 8. Observability, egress, and the cellular guardrail

- Comms fan-out is **subscription-based** (`subscribe.chat` / `roster` / `aspect_status`, and **per-aspect** `subscribe.observe` with a `sinceSeq` cursor; `nexus-watch` unsubscribes the previous aspect on switch). The heavy `model_chunk` stream only flows for the aspect actively being watched.
- With the hub **local on dMon**, the dMon participants' comms are localhost (free). Only little-blue's agents + remote dashboard viewing cross the network.
- **Cellular guardrail (little-blue):** agent→broker **observability forwarding** (`obsforward` `model_chunk` frames) is *upload* over a metered mobile plan when off home Wi-Fi. Add a connection-aware gate: **suspend or down-sample observability forwarding when not on home Wi-Fi** (or pause those agents). The subscription model bounds the *fan-out*; this bounds the *forwarding*.

## 9. Data & backup

- `nexus.db` + `ledger.db` live under the `nexus` account on dMon. Confirm both are in a periodic backup. A nightly local snapshot + an off-box copy (tailnet `scp` to little-blue when home, or the existing free-tier cloud account) is sufficient for hobby-grade durability. Backup target + cadence: confirm in plan.

## 10. Future path: lift to cloud (escape hatch, not now)

The day this outgrows hobby and genuinely cannot be down: lift the **same** broker + ledger onto a tiny ARM EC2 on the tailnet. Because it's a Go binary + SQLite + Tailscale, the move is ~hours, not a rewrite — agents only need the broker's tailnet address to change. A **free-tier cloud account already exists** (currently hosts an unused `cairn` deployment). Trigger conditions: needing the hub up while dMon is off, or hosting always-on agents off all personal hardware.

## 11. What we explicitly are NOT doing (YAGNI)

- No cloud EC2 now. No public endpoint. No HA/failover. No containers (per-OS-user isolation is enough). No custom VPN (Tailscale covers it). No headless/autonomous Unity for wren (Xvfb/EGL/virtual-seat) — see §6.

## 12. Open items for the implementation plan

- broker+ledger: **one systemd unit or two?** (depends on whether ledger is in-process — confirm).
- `nexus.slice` weights/caps: concrete `CPUWeight` + `MemoryHigh`/`MemoryMax` values.
- NEX-336 rollout state vs interim `0600` cred files / `EnvironmentFile=`.
- SELinux contexts for data dir, cred files, shared `wakestone` tree, unit files.
- Unity MCP server: which bridge, how wren's `.mcp.json` addresses it, and the graceful-degradation behaviour when the editor's closed (§6).
- backup target + cadence (§9).
- first-cut scope: stand up broker+ledger+Keel first and add aspects incrementally, or the full roster at once? (recommend incremental: broker+ledger+keel, then anvil + wren, then the rest.)
