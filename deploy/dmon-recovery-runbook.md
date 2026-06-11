# dMon recovery runbook — pod-networking outage (NEX-600)

**Use when:** the k3s cluster is network-isolated (broker not listening, aspects can't connect, `flannel.1` missing). Root cause 2026-06-11: the USB-Ethernet dongle (`enp0s13f0u4u4c2`) flapped → flannel lost its external interface; a wedged system D-Bus then made `systemctl` unusable, so k3s couldn't be restarted in place. Recovery is a console reboot + (durably) pinning flannel off the flaky dongle.

## 0. Before reboot (optional, from console)
Nothing needs saving — sqld (the DB) and the broker PVC are on persistent volumes; pods are Running, just isolated. A clean reboot loses nothing.

## 1. Reboot (at the console)
```
sudo reboot
```
(Remote reboot was avoided: dMon = ASUS ROG with boot quirks (NEX-310) + the flaky dongle — needs console eyes in case it doesn't come back on the network.)

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

## 3. If flannel STILL can't find its interface (the recurrence — NEX-600 durable fix)
Symptom: k3s logs spam `vxlan_network.go: external interface not found, retrying`. flannel auto-detects via the default route, which is the flaky USB dongle. Pin it to a stable interface instead.

1. Pick a STABLE interface at the console (NOT the USB dongle `enp0s13f0u4u4c2`):
   ```
   ip -br addr            # find the reliably-up iface with connectivity (built-in NIC, or wifi wlo1)
   ```
2. Add `--flannel-iface=<stable-iface>` to the k3s server args. k3s on Fedora reads `/etc/systemd/system/k3s.service` (ExecStart) or `/etc/rancher/k3s/config.yaml`. Prefer the config file (survives k3s upgrades):
   ```
   # /etc/rancher/k3s/config.yaml
   flannel-iface: "<stable-iface>"
   ```
3. Restart k3s (systemd should be healthy post-reboot):
   ```
   sudo systemctl restart k3s
   ip -br link show flannel.1     # confirm recreated
   ```
4. **Durable hardware fix:** the USB-Ethernet dongle is the root instability (behind both the random reboots and this outage). Replace it with a stable wired NIC, or commit to wifi (`wlo1`) as the pinned flannel-iface + default route. Until the dongle is out of the critical path, expect recurrence.

## 4. Once healthy — land the queued roundtable work + e2e
These branches are built, tested, and rebased onto current main, waiting on a live cluster:
- `feat/spawn-mcp-tool-clean` (NEX-601) — spawn MCP tool (hands agent-triggerable)
- `feat/convene` (NEX-580 P3) — convene (reviewed; merge after review clears)
- hands (NEX-571) is already merged to main (`d9e17c0`); the harness contracts are merged + were deployed/verified earlier.

Steps: merge spawn-tool + convene → on dMon `/usr/local/src/nexus` pull main → rebuild `nexus-builder:dev` + `nexus-broker:dev` (deploy/worker/build.sh, GOTOOLCHAIN=auto, podman→k3s ctr import) → rollout broker + builder image → then the **roundtable e2e**:
- **hands:** an aspect calls the `spawn` tool → a kindred-named hand (`harrow.tine`) boots from its JWT, runs on the parent's provider, reports into the audit thread, naps on completion.
- **convene:** `!convene plumb anvil — <problem>` → both nappers wake from the briefs, post from their lenses, facilitator (shadow) posts `CONSENSUS:` and closes; the stuck case → decision-point in dm:shadow → abandoned close.

That proves all four roundtable pillars live: presence · hands · spawn · convene.
