#!/usr/bin/env python3
"""PreToolUse guard: git config/remote inspection must be token-sanitized.

Third recurrence, 2026-07-28. A bare `git remote get-url origin` printed a
GitHub PAT into the session transcript. It had already happened twice on
2026-07-17 (the second time with a LIVE token, which the operator rotated).
The lesson was written down each time -- see the feedback memory
`no_tokens_in_git_urls` -- and each time it failed to fire, because a memory
in one project's store is not loaded in another's, and guidance you have to
remember is not a control.

So: make it a control. Any command that can print a credential-bearing git
URL is DENIED unless it carries the canonical redaction pipe:

    ... | sed -E "s/gh[a-z]_[A-Za-z0-9_]+/[REDACTED]/g"

Denied rather than asked on purpose. The mitigation is one pipe and costs
nothing, whereas an `ask` invites an approval that still leaks -- transcripts
persist and get ingested downstream, so the leak is not undone by noticing it
afterwards.

Detection is deliberately broad (it matches across the whole command string,
pipes included) and the allow-check is deliberately narrow. Over-triggering
costs a pipe; under-triggering costs a credential.
"""
import json, re, sys

# Commands whose output can contain https://<user>:<token>@host URLs.
TRIGGERS = [
    (re.compile(r"\bgit\b.*\bremote\b.*(\bget-url\b|\s-v\b|--verbose\b)"),
     "git remote -v / get-url"),
    (re.compile(r"\bgit\b.*\bconfig\b.*(--get-regexp|--list|--get-all|\s-l\b)"),
     "git config --list / --get-regexp"),
    (re.compile(r"\bgit\b.*\bconfig\b.*\b(credential|url)\."),
     "git config on a credential/url key"),
    (re.compile(r"\b(cat|bat|less|more|head|tail|grep|rg|awk|sed|strings)\b.*\.git/config\b"),
     "reading a .git/config"),
    (re.compile(r"\bgit\b.*\bsubmodule\b.*\b(status|foreach)\b.*\burl\b"),
     "git submodule url inspection"),
]

# The canonical sanitizer. Any of these substrings means a redaction is in the
# pipeline. `gh\[a-z\]_` is the literal text of the sed character class, which
# every variant of the approved pipe contains.
SANITIZED = re.compile(r"gh\[a-z\]_|\[REDACTED\]|REDACTED|_redact|sanitize")

# Output that never reaches the transcript at all.
DISCARDED = re.compile(r"(?:^|[^0-9])>\s*/dev/null")
HASHED = re.compile(r"\|\s*(sha\d+sum|md5sum|cksum|wc\b)")


def main():
    try:
        payload = json.load(sys.stdin)
    except Exception:
        sys.exit(0)
    if payload.get("tool_name") != "Bash":
        sys.exit(0)
    cmd = (payload.get("tool_input") or {}).get("command", "")
    if not cmd:
        sys.exit(0)

    label = None
    for rx, name in TRIGGERS:
        if rx.search(cmd):
            label = name
            break
    if label is None:
        sys.exit(0)

    # Already safe?
    if SANITIZED.search(cmd) or DISCARDED.search(cmd) or HASHED.search(cmd):
        sys.exit(0)

    reason = (
        f"Token-leak guard: `{label}` can print an embedded PAT into the transcript, "
        f"and this has leaked a real credential three times (2026-07-17 x2, 2026-07-28). "
        f"Re-run it with the redaction pipe appended:  "
        f"| sed -E \"s/gh[a-z]_[A-Za-z0-9_]+/[REDACTED]/g\"  "
        f"-- or send the output to /dev/null if you only need the exit status."
    )

    print(json.dumps({"hookSpecificOutput": {
        "hookEventName": "PreToolUse",
        "permissionDecision": "deny",
        "permissionDecisionReason": reason,
    }}))
    sys.exit(0)


main()
