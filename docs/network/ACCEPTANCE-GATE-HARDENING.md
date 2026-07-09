# Acceptance-gate hardening — from "PR exists" to "PR substantiates the work"

**Status:** spec, 2026-07-07 · **Author:** shadow · **Depends on:** nothing (self-contained) · **Unblocks:** `AUTO-ROUTING-DESIGN.md` Unit 2 (escalate-on-block is only sound if a block is honest).

## Why

The pool's completion gate decides `met` — did a dispatched builder actually finish. Everything downstream trusts it: the ledger marks the work item done, the orchestrator stops re-dispatching, and (once wired) **escalate-on-block only fires on an honest block**. A gate that can be satisfied without the work being real makes escalation build on sand and lets a cheap under-tiered brain "pass" by narrating success.

### What already exists (do NOT re-build)
The gate is already a **double gate**, and it is *not* naive:
- **Acceptance judge** (`nexus/frame/funnel/acceptance.go` `AcceptanceVerifier.Verify`): ornith-judge scores `criteria` vs the agent's **reported completion text** → `{met, reason}`. Fail-open on judge error (honor task_done).
- **PR-exists gate** (`runtime/cmd/agentfunnel/main.go` `builderPRVerifier`/`prExists`, NEX-468/471): an objective `gh` check that a PR exists on the run's **own** branch (`builder/<ticket>`), with a ticket-scoped fallback (`prExistsByTicketFn`). **Fail-closed** — a gh error or no-PR reports `false`. `builderDecide` (main.go:1716) requires this before a `reason=complete` turn may exit; else bounded PR re-prompts, then the wall-clock backstop.

So "judge says met AND a PR exists on my branch" is already required. The keel/NET-22-27 lineage hardened exactly this.

### The real gap: *exists* ≠ *substantiates*
The objective half of the gate proves a PR **exists**; it never inspects **what's in it**. The only thing judging *whether the diff does the work* is the judge — and the judge reads the **agent's self-reported prose**, not the diff. Three concrete holes:

1. **Empty / token PR passes.** A builder can open a PR whose diff is empty, whitespace-only, or touches an irrelevant file (a README), and the PR-exists gate is satisfied. The judge, reading only the narrative ("implemented §2 with passing tests"), can be told anything.
2. **Test claims are unverified.** Criteria routinely say "passing race-enabled tests." Nothing confirms tests were *run* or *passed* — the judge trusts "I ran `go test -race`, all green." A model that never ran them clears.
3. **The judge verdicts on narrative, not ground truth.** `Verify(criteria, output)` — `output` is the model's completion claim. The DoD is checked against what the agent *says it did*, not against `gh pr diff`. This is the self-report the whole gate is meant to stop leaking through, still leaking through the judge input.

The fix is depth, not a second gate: make the objective side check **substance**, and feed the judge **the actual diff** instead of the agent's prose.

---

## Unit 1 — Judge the diff, not the narrative

Change `AcceptanceVerifier.Verify`'s `output` from the agent's reported-completion text to (or augmented with) the **actual PR diff** of the run's own PR. The verdict then means "the *diff* satisfies the DoD," which is the claim we actually care about.

- New input: `prDiff` (from `gh pr diff <n>` / `gh pr view --json files` for the run's PR — reuse `lookupPRURL`'s resolution so we target the *own-branch* PR, never a fallback match).
- Judge prompt gains: "Judge the DIFF against the criteria. The agent's prose is context, not evidence. If the diff does not contain the required change, `met` MUST be false." (Extends the existing acceptance prompt's "not present verbatim → false" rule from prose to diff.)
- Bounded: cap the diff fed to the judge (reuse `maxAcceptanceOutputLen`-style truncation); for a huge diff, send the file list + the hunks touching the criteria's named artifacts.
- Fail-open unchanged (judge/gh error → today's behavior), so this never *hangs* the pipeline — it only makes a *pass* harder to fake.

## Unit 2 — Substance preconditions (objective, pre-judge, fail-closed)

Before the judge runs, an objective check on the run's own PR — same fail-closed posture as `prExists`:
- **Non-empty diff:** `gh pr view --json additions,deletions,changedFiles` → require additions+deletions > a floor and changedFiles ≥ 1. An empty/whitespace PR fails here without spending a judge call.
- **(Optional, brief-driven) path relevance:** if the `Brief` carries expected-path globs, require the diff to touch at least one. Off when the brief names none (back-compat).
- A precondition miss is a **block**, not a judge-pass — routes into the existing bounded re-prompt / backstop exactly like a missing PR.

## Unit 3 — Test evidence, not test claims

"Passing tests" must be **shown**, not asserted:
- The builder contract requires attaching captured test output (e.g. the tail of `go test -race ./...`) as a run artifact / final-message fenced block, keyed so the gate can find it.
- A cheap objective check (no LLM): the evidence exists, ends in `ok`/`PASS`, and contains no `FAIL`/`panic:`/`--- FAIL`. Absent or failing evidence → block.
- Stretch (later, gated): the gate itself runs `go build ./... && go test -race <changed pkgs>` in the run's workspace and keys `met` off the exit code — ground truth over any claim. Env-gated (`ACCEPTANCE_RUN_TESTS=1`) and time-boxed; dark by default because it costs wall-clock and a workspace.

## Unit 4 — Own-PR provenance

Guard the ticket-fallback so a *pre-existing* PR can never be credited to a new run:
- The credited PR's head branch MUST be **this run's** `builder/<ticket>` (or its commits reachable from this run's pushed HEAD). The `prExistsByTicketFn` fallback stays for resilience but must additionally match the run's ticket *and* not resolve to a branch/PR authored before this run started (compare against the run's start time / the branch's first-commit author).
- Prevents the failure mode I initially mis-attributed to NET-66: a run "passing" by pointing at someone else's already-merged PR for a different ticket.

---

## Posture & interaction
- **Fail-closed on substance, fail-open on the judge.** Objective checks (Units 2–4) that error or come up empty → block (never a silent pass). The *judge* (Unit 1) stays fail-open so a flaky ornith-judge can't wedge the pipeline — but it now judges the diff, so a fail-open only ever *reverts to today*, never worse.
- **Bounded, reuses existing loop.** Every new "block" routes through `builderDecide`'s existing bounded re-prompt + `-builder-timeout` backstop. No new unbounded paths.
- **Escalation depends on this.** `AUTO-ROUTING-DESIGN.md` Unit 2 climbs the ladder on an honest block. Units 1–3 here are what make "block" mean "the work isn't real" instead of "the narrative wasn't convincing." Land this before escalation goes live.

## Tests
- empty-diff PR → Unit 2 blocks, no judge call.
- diff missing the required artifact → Unit 1 judge returns met=false (fed a real diff via a fake gh).
- test evidence absent / contains FAIL → Unit 3 blocks; present + ok → passes.
- pre-existing foreign PR (different ticket, older branch) → Unit 4 refuses to credit; own-branch PR → credited.
- all objective checks error (gh down) → block, bounded re-prompt, backstop fires (no silent pass).
- judge errors with a valid substantive PR → fail-open to today's honor-task_done (no regression).

## Sequencing
Unit 2 (cheapest, pure-objective) → Unit 4 (provenance, pure-objective) → Unit 1 (judge-the-diff) → Unit 3 evidence-check, then the gated run-tests stretch. Each is independently landable and each only makes a *pass* harder — never blocks work that today legitimately completes.

---

## Pull-checks wiring — gate verdicts recorded on the cairn pull (cairn#99)

Every gate above decides `pass`/`fail`/`block` **broker-side**; that verdict is
already authoritative. Once a run carries **cairn-pull addressing** (its repo
lives in cairn, not bare GitHub), the SAME verdicts are additionally recorded
as **cairn-server pull checks** (`PullService.RecordPullCheck`) — a durable,
cairn-native record of what each gate decided, independent of the broker's own
logs, and the precondition `MergePull` needs to refuse a merge on a non-`pass`
check. This is pure observability/enforcement plumbing on cairn's side; it
changes nothing about how a gate reaches its own verdict.

### Check-name vocabulary
Fixed literals, one per gate, matching this doc's units exactly:

| Check name | Gate | Recorded when |
|---|---|---|
| `pr-exists` | `builderPRVerifier`/`prExists` | Every PR-gate evaluation (pass or fail) |
| `pr-substantial` | `builderPRVerifier`/`prSubstantial` (Unit 2) | Only reached after `pr-exists` passes |
| `acceptance-judge` | `AcceptanceVerifier.Verify` via `builderAcceptanceGate.Decide` | Every time the judge actually runs (`hasCriteria && verify != nil` and the RPC itself didn't error — a judge error is fail-open and records nothing, matching "no verdict to speak of") |
| `test-evidence` | Unit 3 (`testEvidenceMissing`) | Only when the test-diff requirement was **active** for this run (`ACCEPTANCE_REQUIRE_TEST_DIFF=1`, a repo present, and the criteria mention tests) — an inactive Unit 3 records nothing |

State is always `pass` or `fail` (RecordPullCheck also accepts `pending`, but
no gate in this codebase has a use for it — every gate above resolves to a
definite verdict by the time it records). `summary` carries the judge's
reason / the gate's own explanation; `evidence_url`, when resolvable, is the
run's PR URL.

### Env config (dark by default)
Rides the same broker-env→worker seam as `CW_VCS` (`runtime/dispatch/jobspec.go`
`acceptanceGateEnvKeys` — set on the broker Deployment, forwarded onto
dispatched builder Jobs only when set):

- `CW_PULL_SERVER_ADDR` — cairn-server gRPC address. **Unset → the recorder is
  never built and the gate path makes zero PullService calls.** This is the
  back-compat contract: every run without this set behaves byte-identical to
  before this wiring existed.
- `CW_PULL_ORG` / `CW_PULL_SLUG` — the cwb-org and cairn repo slug pull checks
  are recorded against. Both required (with `CW_PULL_SERVER_ADDR`) or the
  recorder stays dark, logged.
- `CW_PULL_PROJECT` — default ledger project key `EnsurePull` opens the pull
  under.
- `CW_PULL_TLS_CERT` / `CW_PULL_TLS_KEY` / `CW_PULL_TLS_CA` — cwb mesh mTLS
  material for the cairn-server dial (same convention as `WORKGRAPH_TLS_*` —
  see `nexus/workgraph/dial.go`).
- `CW_PULL_DEV_INSECURE=1` — dial without mTLS (local dev only).

Implementation: `runtime/pullchecks` (client + `Recorder` + sanitizer),
`pullchecks.NewRecorderFromEnv` is the single dark-by-default entry point.
Wired into `runtime/cmd/agentfunnel/main.go` via the package-level
`pullCheckRun` (read from `builderPRVerifier`'s closure) and
`builderAcceptanceGate.pullCheck` (read from `Decide`) — both nil unless
`buildPullCheckRun` resolves a live recorder at builder-wiring time, and every
existing test in that package leaves both nil, so this wiring is provably
inert for every caller that doesn't opt in.

### Broker-gate subject convention
Every `OpenPull`/`RecordPullCheck` call a `Recorder` makes presents
`cwb-subject=broker-gate` (`pullchecks.BrokerGateSubject`), **not** the
builder aspect's own identity. A pull check must be attributable to the gate
that produced the verdict, not the worker being gated — this is the start of
the separation-of-duties story (cairn#99); the corresponding cairn-side scope
split (a narrower `repo:write`-equivalent scope for gate-only recording,
distinct from a builder's own push/PR-open credentials) is a later unit, not
part of this wiring.

### Sanitize-before-send
`RecordPullCheckRequest.name` is capped at 128 bytes and must contain no
non-printable rune (cairn's stricter check-name rule); `summary`/
`evidence_url` are capped at 8192/2048 bytes and must contain no control
character — cairn-server rejects violations with `codes.InvalidArgument`.
`runtime/pullchecks.SanitizeName/SanitizeSummary/SanitizeEvidenceURL` strip
and truncate (with a small margin below each cap) before every RPC, so the
broker's recorder can never trip this validation regardless of what a gate's
summary text happens to contain (e.g. a judge's raw reason string).

### Failure policy — best-effort, never fails the run
A `RecordPullCheck`/`OpenPull` failure (cairn-server down, network blip) is
logged loudly (`slog.Error`, inside `Recorder` itself) and simply drops the
recording — it **never** fails the run or changes a gate's own pass/fail
return value. The broker gate's verdict is already authoritative broker-side;
`MergePull`'s enforcement on non-`pass` checks is a cairn-side backstop for
runs that DO carry cairn-pull addressing, not a second copy of the broker's
own decision logic.
