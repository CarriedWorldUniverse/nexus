#!/usr/bin/env bash
# promptsync — move prompt content between this repo and the live broker.
#
# The prompts that shape every identity in the network live in two DB tables
# (nexus_settings.nexus_md, aspect_personalities) and were, until this repo
# existed, editable ONLY as live rows: no diff, no review, no history beyond
# an integer version. This makes the repo the source of truth and the DB a
# deployment target.
#
#   ./promptsync.sh capture [dir]   live -> repo (then `git diff` shows drift)
#   ./promptsync.sh diff            capture to a temp dir and diff; changes nothing
#   ./promptsync.sh apply-central <file> --yes         repo -> live central
#   ./promptsync.sh apply-aspect <name> <dir> --yes    repo -> live personality
#
# READ and WRITE deliberately take different routes, because the broker only
# has one of them:
#
#   write  PUT /api/admin/nexus-md                  (admin_nexus_md.go)
#          PUT /api/admin/aspect/{name}/personality (admin.go:205)
#   read   there is NO GET for either — the admin surface is write-only for
#          prompt content (GET /api/admin/aspects/all lists aspects, not their
#          prompts). So capture reads the DB directly through the sqld sidecar
#          on the broker pod. Adding the two GET endpoints would let capture
#          use the API and drop the cluster dependency; until then this is the
#          only way to see what is actually live.
#
# Auth for writes: an admin session token. Pass the PATH to a file holding it
# via NEXUS_ADMIN_TOKEN_FILE — never the token itself on a command line, where
# it lands in shell history and process listings.
#
# Cluster access for reads: ssh to the k3s node (NEXUS_SSH), which must be able
# to `sudo k3s kubectl exec` into the broker pod.
set -euo pipefail

BROKER="${NEXUS_BROKER_URL:-https://nexus.tail41686e.ts.net:7888}"
NEXUS_SSH="${NEXUS_SSH:-jacinta@100.91.185.71}"
NEXUS_NS="${NEXUS_NS:-nexus}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { echo "promptsync: $*" >&2; exit 1; }

token() {
  [[ -n "${NEXUS_ADMIN_TOKEN_FILE:-}" ]] || die "set NEXUS_ADMIN_TOKEN_FILE to a file holding an admin session token"
  [[ -r "${NEXUS_ADMIN_TOKEN_FILE}" ]] || die "cannot read ${NEXUS_ADMIN_TOKEN_FILE}"
  cat "${NEXUS_ADMIN_TOKEN_FILE}"
}

confirm() {
  for a in "$@"; do [[ "$a" == "--yes" ]] && return 0; done
  die "refusing to write without --yes (this bumps the live version and has no rollback but git)"
}

json_str() { python3 -c 'import json,sys; print(json.dumps(open(sys.argv[1],encoding="utf-8").read()))' "$1"; }

# query <sql> — run read-only SQL against the broker's sqld over its loopback.
query() {
  local sql="$1"
  ssh -o ConnectTimeout=20 "${NEXUS_SSH}" "
    P=\$(sudo k3s kubectl get pods -n ${NEXUS_NS} -l app=nexus-control -o jsonpath='{.items[0].metadata.name}')
    sudo k3s kubectl exec \$P -n ${NEXUS_NS} -c broker -- bash -c '
      Q=\$(cat <<\"JSONEOF\"
{\"statements\":[$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$sql")]}
JSONEOF
)
      exec 3<>/dev/tcp/localhost/8080
      printf \"POST / HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: \${#Q}\r\nConnection: close\r\n\r\n%s\" \"\$Q\" >&3
      cat <&3 | tail -2
    '" 2>/dev/null | tail -2
}

cmd_capture() {
  local out="${1:-${repo_root}}"
  # Responses go through FILES, never through shell interpolation into python
  # source: prompt content is multi-line markdown and embedding it in a string
  # literal breaks on the first control character.
  # NB: no `trap ... RETURN` here. Bash RETURN traps are not function-local,
  # so one set inside this function fires again when its CALLER returns, by
  # which point the variable is out of scope and `set -u` aborts. Clean up
  # explicitly instead.
  local tmpq; tmpq="$(mktemp -d)"
  query 'select version, nexus_md from nexus_settings' > "${tmpq}/central.json"
  query 'select aspect_name, nexus_md, soul_md, primer_md, version from aspect_personalities order by aspect_name' > "${tmpq}/pers.json"
  python3 - "$out" "${tmpq}/central.json" "${tmpq}/pers.json" <<'PY'
import json, os, sys
out = sys.argv[1]
central = json.load(open(sys.argv[2]))[0]["results"]
pers    = json.load(open(sys.argv[3]))[0]["results"]

os.makedirs(os.path.join(out, "central"), exist_ok=True)
cver, cmd_ = central["rows"][0]
with open(os.path.join(out, "central", "nexus-md.live.md"), "w", encoding="utf-8") as f:
    f.write(cmd_ or "")

manifest = {"central_version": cver, "aspects": {}}
cols = {c: i for i, c in enumerate(pers["columns"])}
for row in pers["rows"]:
    name = row[cols["aspect_name"]]
    d = os.path.join(out, "aspects", name)
    os.makedirs(d, exist_ok=True)
    for col, fn in (("nexus_md", "nexus.md"), ("soul_md", "soul.md"), ("primer_md", "primer.md")):
        v = row[cols[col]] or ""
        p = os.path.join(d, fn)
        if v:
            with open(p, "w", encoding="utf-8") as f:
                f.write(v)
        elif os.path.exists(p):
            os.remove(p)
    manifest["aspects"][name] = row[cols["version"]]

with open(os.path.join(out, "MANIFEST.json"), "w", encoding="utf-8") as f:
    json.dump(manifest, f, indent=2, sort_keys=True)
    f.write("\n")
print(f"captured central v{cver} + {len(manifest['aspects'])} aspect(s) -> {out}")
PY
  rm -rf "${tmpq}"
}

cmd_diff() {
  local tmp rc=0; tmp="$(mktemp -d)"
  cmd_capture "$tmp" >/dev/null
  # Compare only the captured surface: parts/, drafts, tooling and docs are
  # repo-side artefacts with no live counterpart.
  if diff -ru "${repo_root}/aspects" "$tmp/aspects" \
       && diff -u "${repo_root}/central/nexus-md.live.md" "$tmp/central/nexus-md.live.md"; then
    echo "promptsync: repo matches live"
  else
    echo "promptsync: DRIFT — live differs from repo (< repo, > live)" >&2
    rc=1
  fi
  rm -rf "$tmp"
  return $rc
}

cmd_apply_central() {
  local file="${1:-}"; shift || true
  [[ -f "$file" ]] || die "usage: promptsync.sh apply-central <file> --yes"
  confirm "$@"
  curl -fsS -X PUT "${BROKER}/api/admin/nexus-md" \
    -H "Authorization: Bearer $(token)" -H "Content-Type: application/json" \
    -d "{\"nexus_md\": $(json_str "$file")}"
  echo
}

cmd_apply_aspect() {
  local name="${1:-}" dir="${2:-}"; shift 2 || true
  [[ -n "$name" && -d "$dir" ]] || die "usage: promptsync.sh apply-aspect <name> <dir> --yes"
  confirm "$@"
  local body
  body="$(python3 -c '
import json, os, sys
d = sys.argv[1]
print(json.dumps({col: (open(os.path.join(d, fn), encoding="utf-8").read()
                        if os.path.exists(os.path.join(d, fn)) else "")
                  for col, fn in (("nexus_md","nexus.md"),("soul_md","soul.md"),("primer_md","primer.md"))}))' "$dir")"
  curl -fsS -X PUT "${BROKER}/api/admin/aspect/${name}/personality" \
    -H "Authorization: Bearer $(token)" -H "Content-Type: application/json" \
    -d "$body"
  echo
}

case "${1:-}" in
  capture)       shift; cmd_capture "$@" ;;
  diff)          shift; cmd_diff "$@" ;;
  apply-central) shift; cmd_apply_central "$@" ;;
  apply-aspect)  shift; cmd_apply_aspect "$@" ;;
  *) sed -n '2,33p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 1 ;;
esac
