# Verified provenance — the five papers

Each citation was verified against primary sources during authoring (ACM DL / original PDFs / authoritative summaries). For each: the exact citation, what the paper **actually** established, and its scope caveat. Cite these so claims aren't misattributed.

## P1 — Parnas 1972
**D. L. Parnas, "On the Criteria To Be Used in Decomposing Systems into Modules," CACM 15(12), Dec 1972, pp. 1053–1058.** DOI 10.1145/361598.361623.
- *Established:* decompose by hidden design decisions (**secrets**), not flowchart/dataflow steps (the KWIC index example contrasts the two). Payoff = changeability + low coupling (connections = the assumptions modules share, kept minimal). Explicitly a judgment **with costs** — wrong secrets give rigid modules, and module indirection can cost performance ("efficiency recovered by other means").
- *Scope:* leaks/coupling and changeability. **NOT latency** — Parnas names indirection as a perf *cost*, not a cure.

## P2 — Liskov & Zilles 1974
**Barbara Liskov & Stephen Zilles, "Programming with Abstract Data Types," Proc. ACM SIGPLAN Symp. on Very High Level Languages, 1974; SIGPLAN Notices 9(4), pp. 50–59.** DOI 10.1145/800233.807045.
- *Established:* a type is **characterized by its operations**, not its representation ("completely characterized by the operations available on those objects"). CLU **clusters** bundle the rep with its operations and make the rep accessible only inside the cluster. Abstraction = deliberate omission of irrelevant detail.
- *Caveat:* this is the **ADT/CLU paper — NOT the Liskov Substitution Principle** (LSP = Liskov's 1987 OOPSLA keynote, formalized in Liskov & Wing 1994 "A Behavioral Notion of Subtyping"; named "LSP" later by R. C. Martin). Two separate ideas ~13–20 years apart. Encapsulation does **not** fight bloat — clean clusters can multiply.

## P3 — Dijkstra 1968
**E. W. Dijkstra, "Go To Statement Considered Harmful," CACM 11(3), Mar 1968, pp. 147–148.** DOI 10.1145/362929.362947. A **Letter to the Editor** (not a full article); editor **Niklaus Wirth** renamed it from Dijkstra's "A Case Against the Goto Statement."
- *Established:* minimize the gap between the **static program text** and the **dynamic process**, so execution state is locatable in a small, meaningful set of coordinates (textual index + call-stack + loop counters). Unrestricted `goto` destroys those coordinates. An argument from **human comprehensibility**.
- *Caveat:* NOT a performance result and NOT the Böhm–Jacopini theorem (that goto is *unnecessary*, 1966 — separate). Branch-minimal kernels help **GPU warp coherence** for a distinct **hardware** reason; do not cite Dijkstra as a speed proof.

## P4 — Hoare 1969
**C. A. R. Hoare, "An Axiomatic Basis for Computer Programming," CACM 12(10), Oct 1969, pp. 576–580, 583.** DOI 10.1145/363235.363259.
- *Established:* axiomatic/deductive reasoning about programs; the **triple** (printed `P{Q}R`; modern `{P} C {Q}`); the assignment axiom + rules of consequence, composition, and iteration (the while rule); **loop invariants**. Scoped honestly to **partial correctness** — proves "*if* it terminates, the postcondition holds," **not** termination.
- *Caveat:* leans on the axioms faithfully modelling the machine (the finite-arithmetic axioms are the in-paper basis — float/overflow can violate a "proved" invariant). **NOT latency, NOT termination.** Resource-leak reasoning is **separation logic** (Reynolds/O'Hearn, ~2000s), a descendant — not this paper.

## P5 — Brooks 1986
**F. P. Brooks, Jr., "No Silver Bullet — Essence and Accident in Software Engineering."** IFIP *Information Processing 86*, pp. 1069–1076 (primary); reprinted **IEEE Computer 20(4), Apr 1987, pp. 10–19** (reprint title uses plural "Accidents"). Retrospective: **"'No Silver Bullet' Refired" (1995)**, in *The Mythical Man-Month* Anniversary Edition.
- *Established:* the **essence/accident** split; accidental difficulty was largely conquered by high-level languages, time-sharing, and unified environments → **diminishing returns** from further tooling. Four properties make the essence hard: **complexity, conformity, changeability, invisibility**. Endorsed *partial* attacks (not silver bullets): **buy-vs-build, rapid prototyping / requirements refinement, incremental "grow don't build," great designers.**
- *Central claim (verbatim):* "there is no single development, in either technology or management technique, which by itself promises even one order of magnitude [tenfold] improvement within a decade in productivity, in reliability, in simplicity." Note **"single"** (many attacks may compound) and **"within a decade"** — a **bounded prediction, not a law**.
- *Caveat:* about **development productivity / conceptual complexity**, **NOT** runtime latency/leaks/bloat. Much of it targets **large-team management** ("great designers paid like executives") — dead weight for a solo dev.
