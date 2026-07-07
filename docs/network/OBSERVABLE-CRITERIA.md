# Observable acceptance criteria — the authoring standard

**Status:** standard, 2026-07-07 · **Author:** shadow · **Pairs with:** `ACCEPTANCE-GATE-HARDENING.md` (the gate *verifies*; this is what makes verification possible).

## The principle

**A criterion must be checkable from the artifact plus the run's evidence alone — without trusting the agent's word.**

The gate hardening gives the judge real evidence: the PR diff (Unit 1), a non-empty own-branch PR (Units 2/4), a test file when required (Unit 3), the run's actual tool calls (provenance). But every one of those units silently degrades to *trusting the narrative* when the criterion isn't observable. A criterion the judge cannot check against ground truth is a criterion the agent can satisfy by *saying* it's done. **An un-observable criterion is a hole in the whole gate.**

Live evidence this is real:
- **NET-30 (maren):** "produce the correct SHA-512 of a password you cannot know" — ungameable *and* un-checkable. The judge can't confirm correctness, so the only outcomes are an honest block or a lucky fabrication. The criterion was the bug.
- **Vision smoke (NET-68):** "a description produced by read_image" — the agent wrote a plausible description without read_image ever running, and a bare-prose criterion let it pass. (Now caught by the provenance gate *because the criterion named a tool* — see rule 3.)

## The rules

1. **Name the artifact and where it lives.** "A PR on `builder/<ticket>` adding `X`", not "X is done." The gate needs to locate the thing.
2. **Point at ground truth the judge can read.** The criterion must be satisfiable by inspecting the **diff**, a **tool's output**, or a **test result** — never only the agent's summary. If the only evidence is the agent's word, it isn't observable.
3. **Require the process's evidence, not its claim.** When a specific tool/command/computation must produce the result, require its **output to be present** and **name the tool** — the provenance gate then confirms the tool actually ran. `"docs/x.md contains the text returned by the read_image tool"` (checkable: tool in the invoked-list + output in the diff), not `"correctly describes the image"` (a bare claim).
4. **Make required tokens/strings literal and verbatim.** `"the PR body contains the literal token CONVERGED-OK"` (grep-able), not `"convergence confirmed"` (a paraphrase the judge must trust).
5. **Tests: name them and require them in the diff.** `"adds a _test.go covering below/in/above range; go test -race ./pkg passes"` — Unit 3 checks the test file is in the diff; the diff shows the coverage.
6. **Never require a target the judge can't independently verify.** A "correct" value of something unknowable to the checker (a hash of a secret, a fact the judge can't confirm) is un-observable by construction. Either reframe it to something checkable, or route it to **human verification** and mark it so — don't hand a text judge a criterion it can only guess at.
7. **One artifact, one checkable claim.** Compound or vague criteria ("works well and is clean") are unverifiable. Split them; each clause must be independently checkable.
8. **Lean on what the gate already enforces.** Write criteria that ride the existing units — PR exists · non-empty · own-branch · diff-satisfies-DoD · test-in-diff · named-tool provenance. A criterion phrased in those terms is verifiable for free.

## Checklist (apply before dispatching a unit)

- [ ] Names the concrete artifact (file/PR/branch) and its location.
- [ ] Satisfiable by reading the diff / a tool's captured output / a test result — not the agent's narrative.
- [ ] If a tool/computation is required, it is **named** and its **output** (not a claim) is required present.
- [ ] Any required token/string is **literal and verbatim**.
- [ ] No clause depends on a value the checker cannot independently verify (else: reframe, or mark human-verify).
- [ ] Each clause is a single, independently-checkable statement.

## Good / bad

| ✗ un-observable (gameable) | ✓ observable (checkable) |
|---|---|
| "The image is correctly described." | "`docs/x.md` contains the text returned by the `read_image` tool for the image." |
| "Memory matches 100%." | "The diff adds `func F` in `file.go`; `go test -race ./pkg` passes." |
| "Convergence confirmed." | "The PR body contains the literal token `CONVERGED-OK`." |
| "Produce the SHA-512 of the secret." | *(un-checkable by a judge → reframe, or mark human-verify).* |
| "Add tests and make them pass." | "Adds `pkg/x_test.go` covering the empty/one/many cases; `go test -race ./pkg` passes." |

## Where this applies

- **Whoever authors a unit's Definition of Done** — a human writing a ticket, or the orchestrator decomposing work (`orchestrator` skill: "Spec each delegated unit … acceptance criteria"). The orchestrator skill carries a short form of this standard so agentic DoD-writing applies it.
- **The rubric is not a gate on people** — it's a heuristic for writing criteria the machine can actually verify. A criterion that must stay subjective (a visual/UX judgment) is legitimate; mark it **human-verify** rather than dressing it up as machine-checkable, so the gate doesn't fake-verify it. (The operator remains the visual judge — some things are meant to be human-checked.)
