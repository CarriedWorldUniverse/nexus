#!/usr/bin/env bash
# govulncheck gate with an explicit allowlist of accepted advisories.
#
# Runs govulncheck in JSON mode, extracts the OSV ids of symbol-level
# findings (finding objects whose trace starts with a function frame --
# the same set that fails govulncheck's default text mode), and compares
# them against scripts/govulncheck-allowlist.txt. Allowlisted findings are
# reported as accepted; any non-allowlisted finding fails the gate.
#
# Allowlist format: one OSV id per line, '#' comments allowed.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
allowlist_file="${repo_root}/scripts/govulncheck-allowlist.txt"

scan_json="$(mktemp)"
trap 'rm -f "${scan_json}"' EXIT

echo "govulncheck-gate: running govulncheck -format json ./..." >&2
(cd "${repo_root}" && govulncheck -format json ./...) > "${scan_json}"

# Symbol-level findings only: module/package-level progress entries have no
# function frame in trace[0] and do not gate, matching govulncheck's default
# text-mode behaviour.
findings="$(jq -r '
  select(.finding != null)
  | .finding
  | select(.trace != null and (.trace | length) > 0 and .trace[0].function != null)
  | .osv
' "${scan_json}" | sort -u)"

allowlist=""
if [[ -f "${allowlist_file}" ]]; then
  allowlist="$(sed -e 's/#.*//' -e 's/[[:space:]]//g' "${allowlist_file}" | grep -v '^$' || true)"
fi

rejected=""
while IFS= read -r id; do
  [[ -z "${id}" ]] && continue
  if grep -qxF "${id}" <<<"${allowlist}"; then
    echo "govulncheck-gate: ${id} accepted (allowlisted)"
  else
    rejected="${rejected}${id}"$'\n'
  fi
done <<<"${findings}"

if [[ -n "${rejected}" ]]; then
  echo "govulncheck-gate: FAIL — non-allowlisted findings:" >&2
  printf '%s' "${rejected}" | sed 's/^/  /' >&2
  echo "govulncheck-gate: fix the dependency or add a justified entry to ${allowlist_file}" >&2
  exit 1
fi

echo "govulncheck-gate: PASS (no non-allowlisted findings)"
