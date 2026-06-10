<!-- GENERATED FILE — do not edit.
     Sourced from https://github.com/CarriedWorldUniverse/porter/blob/HEAD/README.md
     by scripts/sync-repo-readmes.sh at docs build time.
     Edit that README, not this file. -->

!!! info "Sourced from the repo README"
    This page mirrors [`porter`](https://github.com/CarriedWorldUniverse/porter)'s live `README.md`.
    Edit the README in the repo, not this page.

# porter

> *One who carries.* A casket-encrypted, searchable **cloud-storage-as-a-filesystem** — mount a remote object store, browse/search/read it locally, edit the occasional file, and have changes carried back as atomic versioned writes. Built for **low-IO, read-mostly, offsite** storage.

**Status:** design — pre-implementation. The core FS/manifest/check-in design is in [`docs/2026-05-29-porter-design.md`](https://github.com/CarriedWorldUniverse/porter/blob/HEAD/docs/2026-05-29-porter-design.md); the current lead direction (pluggable backends, Google-Drive-first) is in [`docs/2026-06-11-google-drive-backend-design.md`](https://github.com/CarriedWorldUniverse/porter/blob/HEAD/docs/2026-06-11-google-drive-backend-design.md).

A CarriedWorldUniverse product (sibling to `nexus`, `cairn`, `casket`, `keel`). Object-granular by design — whole-file versioned writes, no in-place partial writes or cross-file locking. Explicitly **not** for git or databases, and **not** a high-throughput filesystem.

## What it is

- Mounts a remote object store as a local, navigable, searchable filesystem.
- **Opt-in** per-file encryption via [casket](https://github.com/CarriedWorldUniverse) (per-file AEAD; the remote holds opaque ciphertext blobs only — the storage provider never sees plaintext).
- In-RAM **manifest** (metadata + search, zero remote round-trips); local cache + hydrate-on-open.
- **check-out → edit locally → atomic versioned check-in** (conditional compare-and-swap write); single-writer lease (via nexus) + non-blocking reads; type-aware 3-way merge on conflict.

## Backends — Drive-first, pluggable

Storage is abstracted behind a minimal `ObjectBackend` seam (`Get` / `PutConditional` / `List` / `Changes` / `Versioning`, with capability flags so the core degrades gracefully). The correctness model — local-edit → atomic conditional check-in → MVCC isolation — is preserved across backends; it just calls `PutConditional` instead of any one provider directly.

- **Google Drive — the first (v1) backend.** Drive addresses by opaque file ID (porter owns the path↔ID mapping in its manifest); conditional writes via ETag / `headRevisionId` preconditions; native revisions for versioning; the Drive **Changes API** as a first-class delta feed. Scoped to a dedicated app folder (`drive.file`), never the operator's whole Drive. Casket encrypts before bytes leave the cluster, so Google (and Google's AI) only ever see ciphertext.
- **S3** — the original target, demoted to a **future** implementation of the same interface (`PUT If-Match`, versioning, SQS change events). Not implemented in v1.
- No OneDrive / multi-cloud in v1 (one backend, done well; interface stays open).

## Deployment shape

Agents talk to a porter filesystem and **never know which cloud is underneath** — no Drive mounts on agent pods, no OAuth tokens scattered across the fleet. A single **backup/sync pod** owns the one Google connection, runs the sync, and absorbs Drive's rate limits + chunking. Two access patterns over the same backend + casket key:

- **Shed FS (hot)** — porter mounted in the pods that need shared scratch / inter-agent comms; hydrate-on-open + atomic check-in; broker lease for contended files, optimistic `If-Match` otherwise.
- **Backup (cold)** — the backup pod snapshots designated cluster data (sqld/`nexus.db` export, aspect PVCs, ledger, cairn repos) into a backup prefix on the same Drive, casket-sealed and revision-versioned, on the in-nexus scheduler. Restore = a porter check-out of the snapshot.

The single Drive OAuth token lives in **custodian** and is brokered to the backup pod at use-time (short-lived access token per sync) — porter's Google Drive backend is custodian's first real cloud-credential customer.

## Status / open

Design-stage; no implementation yet. The core FS still owes its implementation. Open decisions include the hydration mechanism (FUSE lead vs Windows Cloud-Files placeholder vs sync-daemon), the Drive path model (lean flat-app-folder-with-manifest, per S3 parity), and backup scope + retention. Build is sequenced behind the roundtable week — see the Drive backend design doc for phasing.
