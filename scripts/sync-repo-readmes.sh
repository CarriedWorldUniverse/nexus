#!/usr/bin/env bash
# sync-repo-readmes.sh — pull each org repo's README into docs/repos/<repo>.md
# so the published repo page IS the repo's live README (single source of truth).
#
# Run before `mkdocs build`. Requires `gh` authenticated (the docs workflow
# provides GITHUB_TOKEN). If a repo has no README, or the fetch fails, the
# committed thin stub for that repo is left in place as a fallback.
#
# Edit the repo's README, not docs/repos/<repo>.md — these pages are generated.
set -uo pipefail

ORG="CarriedWorldUniverse"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="$REPO_ROOT/docs/repos"

# The set of repos surfaced on the docs site, in nav order. Keep in sync with
# mkdocs.yml's Repos nav and docs/repos/index.md.
REPOS=(
  nexus bridle agora acp-claude-pty interchange
  herald ledger commonplace cairn custodian
  cw cwb-client cwb-proto cwb-conformance
  nexus-platform porter lynxai vessel
  casket-go casket-ts casket-dotnet
)

mkdir -p "$OUT_DIR"

ok=0
skipped=0
for repo in "${REPOS[@]}"; do
  out="$OUT_DIR/$repo.md"
  body=""
  for attempt in 1 2 3; do
    body="$(gh api "repos/$ORG/$repo/contents/README.md" -q .content 2>/dev/null | base64 --decode 2>/dev/null)"
    [ -n "$body" ] && break
    sleep 2
  done
  if [ -z "$body" ]; then
    echo "skip  $repo (no README / fetch failed) — keeping committed stub"
    skipped=$((skipped + 1))
    continue
  fi
  # Rewrite repo-relative markdown links to absolute GitHub blob/tree URLs so
  # they resolve from the docs site (the README's neighbours aren't in docs/).
  # Matches ](path) where path doesn't start with a scheme, '#', or '/'.
  base="https://github.com/$ORG/$repo/blob/HEAD"
  body="$(printf '%s\n' "$body" \
    | sed -E "s#\]\(\.\/#](${base}/#g" \
    | sed -E "s#\]\((docs/|cmd/|internal/|deploy/|ios/|proto/|gen/|templates/|tests/|scripts/)#](${base}/\1#g" \
    | sed -E "s#\]\((LICENSE)\)#](${base}/\1)#g")"
  {
    echo "<!-- GENERATED FILE — do not edit."
    echo "     Sourced from https://github.com/$ORG/$repo/blob/HEAD/README.md"
    echo "     by scripts/sync-repo-readmes.sh at docs build time."
    echo "     Edit that README, not this file. -->"
    echo
    echo "!!! info \"Sourced from the repo README\""
    echo "    This page mirrors [\`$repo\`](https://github.com/$ORG/$repo)'s live \`README.md\`."
    echo "    Edit the README in the repo, not this page."
    echo
    printf '%s\n' "$body"
  } > "$out"
  echo "sync  $repo -> docs/repos/$repo.md"
  ok=$((ok + 1))
done

echo "---"
echo "synced $ok, skipped $skipped"
