# PRIMER.md — wren

*My project context. I own this file — update it when state changes in ways that matter for cold-start.*

---

## Active Project

**The Carried World — Found-Village MVP**
Pre-release build to generate interest. Player selects region (North/South) + difficulty, places an Inn to found a settlement, manages population/resources/bush reversion.

**Win condition:** Inn + 2+ homesteads within 2 in-game years → `FoundingMilestone` event. Game continues past it.

**Project directory:** `C:\src\experimental`
**Spec:** `C:\src\agent-network\agents\specs\mvp_found_village.md`

---

## Ticket State (as of 2026-04-14)

| # | Name | Status |
|---|------|--------|
| 46 | Region selection system | DONE |
| 47 | Inn placement (InnPlacementController) | DONE (partial — real NPCs + kit wiring open) |
| 48 | Starting kit ScriptableObjects | SOs created, not wired into SettlementController |
| 49 | Difficulty parameter SOs | SOs created, not wired into decay rates |
| 50 | Bush boundary shader | Blocked on #57 (mask) + forge #55 (done) |
| 51 | Population selector UI | Blocked on forge #54 (AI arrival system) |
| 52 | Settlement status display HUD | Blocked on forge #53 (done) |
| 59 | Inn interior UI panel | Open |
| 60 | Founding site scatter props | Open — `FoundingCompletedEvent` hook in place, waiting on maren props |

---

## Team Division

- **wren** — Unity/engine (#46–52, #59–60)
- **forge** — AI/simulation (#53–55; #53 and #55 done)
- **maren** — Visual assets (#56–57; Inn done, dwellings batch 1 done, props imported)
- **verity** — Canon review (#58, standing)

---

## Key Interface Contracts

**Unity ↔ Python sim:** Unity polls at dawn/dusk. Python never pushes.
- Dawn: `GetDawnState(UnitySettlementSnapshot)` → `FoundingSettlementState`
- Dusk: `RecordDuskOutcome(FoundingDuskOutcome)`
- Arrival selection: `CommitArrivalSelection(ArrivalCandidate[])`

**`ArrivalCandidate`:** has `transient: bool` — true for Cargill. UI shows "passing through" tag.

**`FoundingCompletedEvent`** (EventBus) — fired when Inn placed. Carries regionName + innPosition.

---

## Canon Notes

- Inn: logistics centre, not tavern. Stone base + heavy timber + steep thatch. Faces route. Named after family.
- North race bias: human 0.48 / orc 0.18 / dwarf 0.16 / halfling 0.10 / cargill 0.06 / elf 0.02
- South race bias: human 0.50 / orc 0.20 / halfling 0.12 / cargill 0.08 / dwarf 0.07 / elf 0.03
- Cargill: "stopping, not settling" register
- No debug text visible in shipping build.

---

## Infrastructure Notes

**Hands architecture** spec lives at `C:\src\agent-network\agents\keel\HANDS_ARCHITECTURE.md`. Approved, not yet implemented. Wren's hands will need Unity editor connection — hold until simpler hands proven first (likely harrow's trace Hand).

---

*Update this file when: ticket status changes, a new project starts, a blocked ticket unblocks, interface contracts change.*