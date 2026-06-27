# Worked applications (adversarially checked)

The strongest engine + nexus move per principle that survived a skeptic pass, plus the non-obvious calls that baseline testing surfaced. These are illustrations — the rules in `SKILL.md` are the standard.

## P1 · Cut by secrets
- **Engine:** put the `godot_voxel` storage/streaming/meshing behind **one thin sampling seam** (raycast + VoxelTool reads, voxel writes). The boundary test then doubles as a leak detector: any caller (NPC siting, economy, water CA) reaching past it to analytic `surface_height` is coupled to a hidden representation — that *is* the recurring float-drift bug, reframed as a flowchart-style boundary violation.
- **Nexus:** the pillar-freeze **precondition** — enumerate every place the game (or another pillar) assumes a pillar's internals (a schema, a transport, a model name) and sever it. Each assumption is a thaw-risk that pins a frozen pillar back open.

## P2 · Type = operations
- **Engine:** define `CAKernel` / `VoxelWorld` by an **operation set** (step, seed/clear region, sample cell, query tile; `ground_at` / `sample_voxel` / `raycast_place`) and keep the GPU buffer layout (texture vs SSBO, packed encoding, tiling) **private**, so a perf-driven relayout never touches water/belief/economy callers. Encapsulate at the **system edge, not every cell access**.
- **Nexus — the deletion knife:** an operation **no client anywhere** calls is not part of any needed abstraction.
  1. Correct the survey scope — grepping only the game tree is wrong (e.g. `porter` reads `almanac`'s backup-sources list; freezing `almanac` wholesale once **bricked backups**).
  2. Complete the consumer survey across *all* systems/pillars.
  3. **Delete** every operation with zero real clients; **freeze** the genuinely-minimal remainder.
  Reversibility is a reason to delete *after* the survey — not a licence to hoard verified-dead surface.

## P3 · Mappable control flow
- **Engine:** write the per-voxel CA step **straight-line, branch-minimal** — doubly justified by text↔process correspondence *and* GPU warp coherence (divergent lanes in a warp serialize). Handle dry/empty cells with uniform/predicated math (`flow = mask * computed_flow`) or a coarse **active-chunk compaction pass + indirect dispatch** (with a 1-cell apron), not per-thread `if (dry) return` early-outs.
- **Nexus:** "**can I name the coordinates of its running state?**" as a freeze diagnostic — a service that needs log-archaeology across processes to locate its state is a freeze candidate.
- **Caution:** branch-minimal is right *for the GPU, for a hardware reason*. It is **not** a Dijkstra speed proof, and branchy CPU AI with predictable branches is usually fine — measure before rewriting.

## P4 · Invariant in debug
- **Engine:** the **CA conservation invariant** — sum the buffer before/after a tick, assert within epsilon, **debug-flag guarded** so it never touches the shipping hot path (and read the GPU buffer latently / off-frame, never an `rd.sync()` in `_process`). It is a **bug-detector, not a guarantee** — float/GPU drift can violate a "proved" conservation law. Producer-post == consumer-pre at the GPU fence catches staleness invisible to ordinary testing.
- **Correctness ≠ perf:** the conservation assert is deterministic and can run anywhere (even a headless CI box); the tick-time budget is **statistical** and must be measured on the real target GPU (the 5090, not the `:9` seat or CI), after warmup, median/p95. **Never fuse them into one gate** — a flaky perf check poisons the trustworthy correctness check, which then gets muted / `--no-verify`'d, destroying the signal you most wanted.
- **Nexus:** freeze each always-on thing behind a **tiny pre/post smoke assertion** (given a fixed input → expected output/state), so a silently-broken frozen surface is caught without standing the platform back up. Partial correctness is the right bar — a personal tool needs "if invoked, it returns the right thing," not liveness/SLA.

## P5 · Essence over accident
- **Engine triage:** **essential** = CA rules, water, belief field, NPC minds, economy, the real-voxel world model → your scarce **design hours**. **Accidental** = GPU-buffer plumbing, off-thread/`_process` scheduling, sampling utilities, serialization, build tooling → solve **once**, well, then **freeze and stop touching**. (Orthogonal to the perf doctrine, which governs runtime cycles *within* the accidental layer.)
- **Nexus:** "no single development 10x's building the game" + grow-don't-build + buy-vs-build underwrite the **YAGNI freeze** — the nexus platform is the silver bullet Brooks predicts will fail. A unifying `GameSystem` base-class "framework" across genuinely different systems (GPU CA kernels vs CPU economic rules) hides **no shared likely-to-change decision** — it's accidental complexity. Sunk cost (it's about the past) and an architect's authority (verify against local constraints) are not reasons to finish it.
