# deploy/

Container image builds for the nexus services. Each subdirectory holds the
`Dockerfile` + `build.sh` that build and `ctr`-import a service image
(`localhost/<service>:dev`) onto the dMon k3s node.

- `worker/` — builder-agent worker image + the dispatch Job template (`job.yaml`)
- `broker/` — broker image
- `dispatch-controller/` — dispatch-controller image

The Kubernetes manifests that host these services — Deployments, Services,
PVCs, CoreDNS naming, and the reconcile loop — live in the **carriedworld-cloud
hosting platform** (`hosting/services/`, reconciled by `hosting/apply.sh`),
which is the source of truth for runtime configuration.
