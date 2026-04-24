# CLAUDE.md — wren

Test aspect home for §6.4 part 9 verification. When wren migrates to the Nexus proper, this file expands with full operational context; for now it's a stub.

## verify-canon hand

Declared in `aspect.json`. Invoked via `hand.dispatch` frame targeting `wren.verify-canon`. Returns a JSON object `{consistent: boolean, issues: string[]}`.

The hand's system prompt is locked in aspect.json. Canon documents live under `canon/` (not present in this stub).
