---
name: dispatch
description: How to dispatch a ready ticket to a builder aspect and CONFIRM it actually started. For the orchestrator, not the builder. The send_chat "ok" is not acceptance — verify the runner took it.
when_to_use: When you are the orchestrator handing a ready ticket to a builder (anvil/plumb/etc.). Load before every dispatch.
---

# dispatch

You are the orchestrator. You are routing ready work to a builder and you are
ACCOUNTABLE for confirming it ran. A dispatch that didn't start is worse than
no dispatch — it looks done and isn't.

## The one rule that matters

`send_chat` returning "ok" means the CHAT MESSAGE stored. It does NOT mean the
builder accepted the job. The dispatch can fail milliseconds later in the
runner and you will not be told on the channel you acted on. **Never mark a
ticket In Progress or report "dispatched" on the strength of "ok". Verify
against the broker (step 4) first.** (Until NEX-640 lands, the system won't
hand you an honest result — you must go get it.)

## 1. Pick the builder
- Builders = aspects with an `aspect-keyfile-<agent>` secret: anvil, plumb
  (codex builder pods, idle at 0/0 = dispatch-on-demand; the runner launches
  an ephemeral Job per dispatch — they do NOT need to be scaled up).
- One ticket per builder at a time. Builders run in parallel. Alternate
  anvil/plumb, prefer idle.

## 2. Write the directive
First line is the directive; everything after is the task brief.
```
!dispatch <agent> repo=<repo> ticket=<KEY> <one-line task>
<full brief on following lines>
```
- `<agent>` is the BUILDER (anvil/plumb) — NOT the ticket key. Putting the
  ticket key in the agent slot is the classic failure: it parses (unknown
  agents default to enabled), then dies deep in the runner with
  `aspect-keyfile-<KEY> not found`.
- Set the comms `topic` to the ticket key so the builder's replies thread there.
- The brief body MUST include the cloud-git-workflow + developer-standards
  blocks (clone /work yourself, `cw setup-git github`, `gh pr create`, one
  ticket per PR, rebase, tests, CI green, reviewer = orchestrator, no self-merge).

## 3. Submit
Send it. You will get "ok". Ignore it as a success signal. Go to step 4.

## 4. VERIFY ACCEPTANCE (mandatory gate — do not skip)
Read the BROKER container (the pod has 3 containers; `containers[0]` is the
tailscale sidecar and will mislead you — always `-c broker`):
```
POD=$(kubectl -n nexus get pod -l app=nexus-broker -o jsonpath='{.items[0].metadata.name}')
kubectl -n nexus logs "$POD" -c broker --tail=60 | grep -iE 'dispatch|runner|err='
kubectl -n nexus get jobs,pods | grep run-
```
Require ALL of:
- `runner.Submit returned ... err=<nil>` (NOT a WARN "submit failed").
- `runner: builder job created`.
- a `builder-<agent>-run-… Running` pod.
- ideally the builder's own first ack in the ticket thread (true acceptance).

Only now: mark the ticket In Progress and report it dispatched.

## 5. Failure playbook (what step 4 catches)
- `aspect-keyfile-<X> not found` → wrong agent slot (X is the ticket key, not
  a builder) OR that builder has no keyfile secret. Re-dispatch to a real builder.
- `agent <X> is dispatch-disabled` → enable it or pick another builder.
- pod `ImagePullBackOff` / `CrashLoopBackOff` → builder image or boot problem; escalate.
- broker logs `skipping git credential grant; git credential name not
  configured` → expected; the builder self-provisions via `cw setup-git
  github` per the brief. Only a problem if the builder later can't push.

## 6. Watch to completion
Acceptance (step 4) ≠ completion. Poll the ticket thread / the repo for the
pushed branch + opened PR. On PR open, switch to the review skill. On a
builder going quiet with no PR after a reasonable window, read its run pod
logs (`kubectl -n nexus logs <builder-pod>`) — chat-emit can drop a builder's
text on non-zero exit, so absence of a reply is not absence of work.

## Why this skill exists
A mis-dispatch (ticket key in the agent slot) once looked successful for an
hour because "ok" was trusted and the failure was a buried WARN. NEX-640 makes
dispatch first-class — 200 only when the builder is up and has accepted, errors
escalated. When that ships, delete step 4's manual verification; until then it
is the contract enforced by hand.
