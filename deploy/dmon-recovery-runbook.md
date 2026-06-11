# dMon recovery runbook — NVMe APST instability (NEX-600)

**Root cause (2026-06-11, SMART-confirmed):** the **NVMe APST (autonomous power-state) Linux bug** — NOT a failing drive, NOT the network. The Micron OEM NVMe (`nvme0n1`, btrfs root) is healthy (SMART: 2% used, 100% spare, 0 media/integrity errors, btrfs error stats all zero), but Linux mishandles its deep power-saving states → the drive doesn't wake within the kernel timeout → `nvme nvme0: I/O tag X timeout, completion polled` → I/O stalls cascade into hung tasks → wedged system D-Bus (`systemctl` dies) → random reboots → the apparent "network/flannel" symptoms (all downstream of processes hung on disk I/O). One root cause for the whole class of instability.

**Use when:** the box is hanging, randomly rebooting, `systemctl` won't connect to the bus, or the k3s cluster is isolated — and `sudo dmesg | grep nvme` shows `timeout, completion polled`.

## 0. Before reboot
Nothing needs saving on the drive's account — the NVMe is **healthy** (zero media/btrfs errors), and the irreplaceable corpora (forge, WakeStone, canon) are already on GitHub. A clean reboot loses nothing.

## 1. THE FIX — disable NVMe APST, then reboot (at the console)
Add the kernel boot parameter that disables the deep power states the drive mishandles, so it stays responsive:
```
# Fedora (grubby), applies to all kernels + persists across updates:
sudo grubby --update-kernel=ALL --args="nvme_core.default_ps_max_latency_us=0"
sudo grubby --info=DEFAULT | grep args      # confirm the arg is present
sudo reboot
```
(dMon = ASUS ROG; if the box is mid-hang and `grubby`/`sudo` won't run because systemd is wedged, a hard power-cycle to get a clean boot first is fine — the drive is healthy, btrfs is journaled.)

## 1b. Verify the fix took
```
sudo dmesg -T | grep -i "nvme.*timeout"     # expect: NO new entries since boot
sudo grubby --info=$(sudo grubby --default-kernel) | grep ps_max_latency   # arg present
```
If `nvme … timeout, completion polled` stops appearing, the instability is fixed at the root. (If it persists, follow-ups: a per-device NVMe quirk in the kernel, or a Micron firmware update — but APST disable resolves this signature in the large majority of cases.)

## 2. Verify recovery (after boot)
```
# pod overlay back?
ip -br link show flannel.1            # expect: flannel.1 ... UP
# node + k3s healthy?
sudo kubectl get nodes               # expect: Ready
sudo kubectl get pods -A | grep -vE 'Running|Completed'   # expect: empty
# broker listening + aspects reconnecting?
sudo kubectl get deploy -n nexus
sudo kubectl logs deploy/nexus-broker -n nexus --tail=20 | grep -iE 'listen|registered|7888'
# gemma up (keel/harrow depend on it)?
sudo kubectl get deploy gemma-ollama -n nexus
```
If `flannel.1` is present and the broker logs show it listening on :7888 and aspects registering → recovered, go to §4.

## 3. If flannel still struggles after a clean boot (secondary — only if needed)
With APST disabled, a clean boot should bring flannel/k3s back normally (the network symptoms were downstream of the I/O hangs). If k3s logs still spam `vxlan_network.go: external interface not found` after a stable boot, flannel's auto-detected interface is genuinely unstable — pin it explicitly:
```
# /etc/rancher/k3s/config.yaml   (survives k3s upgrades)
flannel-iface: "<stable-iface>"   # ip -br addr to pick the reliably-up iface
sudo systemctl restart k3s
ip -br link show flannel.1        # confirm recreated
```
This is a fallback, not the primary fix — the NVMe APST param (§1) is what resolves the root instability.

## 4. Once healthy — land the queued roundtable work + e2e
These branches are built, tested, and rebased onto current main, waiting on a live cluster:
- `feat/spawn-mcp-tool-clean` (NEX-601) — spawn MCP tool (hands agent-triggerable)
- `feat/convene` (NEX-580 P3) — convene (reviewed; merge after review clears)
- hands (NEX-571) is already merged to main (`d9e17c0`); the harness contracts are merged + were deployed/verified earlier.

Steps: merge spawn-tool + convene → on dMon `/usr/local/src/nexus` pull main → rebuild `nexus-builder:dev` + `nexus-broker:dev` (deploy/worker/build.sh, GOTOOLCHAIN=auto, podman→k3s ctr import) → rollout broker + builder image → then the **roundtable e2e**:
- **hands:** an aspect calls the `spawn` tool → a kindred-named hand (`harrow.tine`) boots from its JWT, runs on the parent's provider, reports into the audit thread, naps on completion.
- **convene:** `!convene plumb anvil — <problem>` → both nappers wake from the briefs, post from their lenses, facilitator (shadow) posts `CONSENSUS:` and closes; the stuck case → decision-point in dm:shadow → abandoned close.

That proves all four roundtable pillars live: presence · hands · spawn · convene.
