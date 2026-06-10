<!-- GENERATED FILE — do not edit.
     Sourced from https://github.com/CarriedWorldUniverse/cw/blob/HEAD/README.md
     by scripts/sync-repo-readmes.sh at docs build time.
     Edit that README, not this file. -->

!!! info "Sourced from the repo README"
    This page mirrors [`cw`](https://github.com/CarriedWorldUniverse/cw)'s live `README.md`.
    Edit the README in the repo, not this page.

# cw — the CWB platform CLI

One binary for humans and agents. Anchored on a single edge URL (the interchange
gateway); authenticates against herald and keeps the session fresh.

## Auth

    cw auth login --edge https://cwb.example --context prod    # human: prompts email + password
    cw auth login --agent --agent-id <id> --slug shadow        # agent: CW_OWNER_SEED in env
    cw auth whoami
    cw auth token            # print a fresh access token (scripting)
    cw auth status           # list contexts + freshness
    cw auth switch prod
    cw auth logout

A *context* is `{edge, identity}`. The refresh token is stored in the OS
keychain; the access token is cached (0600) and silently refreshed. Use
`--token`/`CW_TOKEN` to present a bearer directly (no stored state).

## Identity

    cw whoami            # current identity: context, edge, kind, subject, display, slug, org, scopes, products, expiry
    cw whoami --json
    cw whoami --remote   # server-authoritative record (status, org name, live scopes, agent responsible-human + fingerprint) via herald GET /api/me
    cw auth status          # all contexts + token freshness
    cw auth status --json   # the same, as a JSON array (name/current/edge/kind/display/subject/org/state)

`cw whoami` (alias: `cw auth whoami`) merges the token claims (ids, scopes,
products, expiry) with the config context (display name, agent slug, edge) — all
local, no network call.

`--remote` calls herald (`GET /api/me`) for the authoritative record — the only
whoami path that makes a network call; it also proves the token works
server-side.

## Repos & PRs (cairn)

    cw repo create widgets                 # in your org
    cw repo list
    cw repo clone <org>/widgets [dir]      # shells git with a fresh bearer
    cw pr create --repo <org>/widgets --head feat --base main \
        --title "Add X" --project NEX [--body ...] [--dod ...]
    cw pr list  --repo <org>/widgets [--state open|merged|all]
    cw pr view  7 --repo <org>/widgets
    cw pr merge 7 --repo <org>/widgets     # fast-forward only

Inside a `cw repo clone`d directory the `--repo` flag is inferred from `origin`.
A bare `<slug>` uses your context's org; `<org>/<slug>` or `--org` targets another.

**Pushing** (no `cw push` yet): from a clone,

    git -c http.extraHeader="Authorization: Bearer $(cw auth token)" push

The herald admin command groups (`org`, `human`, `agent`) build on this core; see below.

## Issues (ledger)

    cw issue create --project NEX --type Story --title "Add X" [--body ...] [--dod ...] [--priority ...]
    cw issue list   [--mine | --ready | --project NEX]      # --mine is the default
    cw issue view   NEX-12
    cw issue claim  NEX-12
    cw issue transition NEX-12 "In Review"
    cw issue comment NEX-12 "looks good"

Issues are scoped to your org by the token (no --org). Needs an identity with
`issue:read`/`issue:write`/`issue:claim`.

## Knowledge (commonplace)

    cw kb store --topic onboarding [--visibility org|private] [--tag x] < doc.md
    echo "..." | cw kb store --topic notes
    cw kb search "how does auth work" [--top-k 5]   # semantic (returns full entries)
    cw kb list
    cw kb update <id> [--topic t] [--content c] [--visibility org|private] [--tag x]   # only the flags set; --tag replaces tags
    cw kb delete <id> --yes                                                            # irreversible hard-delete

Knowledge is scoped to your org by the token (no --org). Needs an identity with
`knowledge:read`/`knowledge:write`. `store` reads content from `--content` or stdin.

## Orgs (herald admin)

    cw org create acme [--product cairn --product ledger]
    cw org list
    cw org products <org-id>
    cw org enable  <org-id> ledger
    cw org disable <org-id> ledger
    cw org delete  <org-id> --confirm acme      # --confirm must equal the org name

## Humans (herald admin)

    cw human create --org <org-id> --name alice \
        --scope knowledge:read --scope knowledge:write --password-stdin <<< "$PW"
    cw human set-password <human-id> --password-stdin <<< "$PW"   # else prompts no-echo

Org and identity admin require a platform-admin (`herald:platform-admin`) or
org-admin (`herald:org-admin`) bearer. Passwords are read from stdin
(`--password-stdin`) or an interactive prompt — never a plaintext flag.
Provisioning a working identity end to end:

    ORG=$(cw org create acme)
    H=$(cw human create --org "$ORG" --name alice --scope knowledge:read --password-stdin <<< "$PW")
    cw auth login --edge <edge>      # at the prompt, enter $H and $PW

## Agents (herald admin)

    cw agent keygen                              # mint an owner seed -> set as CW_OWNER_SEED
    cw agent create --org <org-id> --name builder --slug builder \
        --responsible-human <human-id> --scope repo:read --scope repo:write
    cw agent pubkey --slug builder               # offline: derive this agent's casket pubkey + fingerprint from CW_OWNER_SEED

`cw agent pubkey` makes no network call. Its `fingerprint` matches herald's
stored value, so you can verify a local `CW_OWNER_SEED`+`--slug` derives an
already-registered agent by comparing it to `cw whoami --remote`.

An agent's ed25519 "casket" key is derived deterministically from `CW_OWNER_SEED`
+ `--slug` (so creation and `cw auth login --agent` produce the same key; agents
under one owner differ by slug). Creating one needs an org-admin/platform-admin
bearer to register it, and `CW_OWNER_SEED` to derive its key. After create, log
in as the agent:

    CW_OWNER_SEED=<seed> CW_AGENT_ID=<agent-id> CW_AGENT_SLUG=builder cw auth login --agent
