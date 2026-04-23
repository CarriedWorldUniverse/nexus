# agents/ — aspect home folders

Each subdirectory is a complete aspect. Layout per spec §2.4:

```
<name>/
  aspect.json          # see spec §3
  CLAUDE.md            # or AGENT.md — TBD alignment with harness-v2
  SOUL.md
  PRIMER.md
  .credentials/        # per-aspect API keys
  session/
    global.jsonl       # if context_mode=global
    threads/<id>.jsonl # if context_mode=thread
  memory/
  logs/
```

Populates at spec §6.6 (migration). Empty at scaffold time.

keel is NOT an entry here — keel is embedded in the Nexus process (`nexus/frame/`).
