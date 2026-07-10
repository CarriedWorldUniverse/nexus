// Package gates is the shared home of the ACCEPTANCE-GATE-HARDENING gate
// logic (docs/network/ACCEPTANCE-GATE-HARDENING.md), factored out of
// runtime/cmd/agentfunnel so it is callable from BOTH:
//
//   - the existing worker-advisory path (agentfunnel's builderPRVerifier /
//     builderAcceptanceGate — thin wrappers around this package, unchanged
//     behavior, existing tests untouched), and
//   - the new orchestrator-side AUTHORITATIVE gate runner (nexus/orchestrator
//     RunAuthoritativeGates, #473 — cairn#99 Option B: re-run the gates
//     broker/orchestrator-side, where the model being gated can't forge them).
//
// This package holds the PURE decision logic (JSON-shape parsing, ticket
// matching, provenance, substance thresholds, test-evidence detection) and
// the two orchestrating checks (PRExists, PRSubstantial) parameterized over
// injected fetch functions — it never shells out to `gh` itself. Each
// caller supplies its own fetch functions (agentfunnel's package-level
// exec.Command vars for the worker-advisory path; ctx-aware
// exec.CommandContext implementations for the orchestrator path — see
// nexus/orchestrator's authoritative gate runner) so the SAME decision
// logic runs in both places without this package taking a stance on how a
// caller wants to reach GitHub.
package gates

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PRDiffStats is the changed-line footprint of a PR — the substance signal
// (ACCEPTANCE-GATE-HARDENING Unit 2).
type PRDiffStats struct {
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changedFiles"`
}

// prTicketSearchEntry is one row of
// `gh pr list --json number,headRefName,title,createdAt`.
type prTicketSearchEntry struct {
	Number      int       `json:"number"`
	HeadRefName string    `json:"headRefName"`
	Title       string    `json:"title"`
	CreatedAt   time.Time `json:"createdAt"`
}

// prTicketStatsEntry is one row of the ticket-fallback stats query.
type prTicketStatsEntry struct {
	HeadRefName  string    `json:"headRefName"`
	Title        string    `json:"title"`
	Additions    int       `json:"additions"`
	Deletions    int       `json:"deletions"`
	ChangedFiles int       `json:"changedFiles"`
	CreatedAt    time.Time `json:"createdAt"`
}

// isAlnumByte reports whether b is an ASCII alphanumeric byte.
func isAlnumByte(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// TicketWordMatch reports whether ticket occurs in s as a whole token —
// bounded by string start/end or a non-alphanumeric byte on each side — so
// "NET-6" does NOT match inside "NET-66" (the substring bug the raw
// strings.Contains fallback had, which could credit one ticket's run with
// another ticket's PR). Ticket IDs and branch names are ASCII (e.g. NET-66,
// anvil/workers-json-flag); a multibyte title byte adjacent to a hit is >127,
// hence non-alnum, hence a valid boundary.
func TicketWordMatch(s, ticket string) bool {
	if ticket == "" {
		return false
	}
	for i := 0; i+len(ticket) <= len(s); {
		j := strings.Index(s[i:], ticket)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(ticket)
		beforeOK := start == 0 || !isAlnumByte(s[start-1])
		afterOK := end == len(s) || !isAlnumByte(s[end])
		if beforeOK && afterOK {
			return true
		}
		i = start + 1
	}
	return false
}

// MatchPRByTicket is the pure decision core of the ticket-search fallback:
// reports whether any open PR's head branch or title carries ticket as a
// whole token (TicketWordMatch) AND was created at/after notBefore (Unit 4
// provenance — never credit a run with a PR opened before it started).
func MatchPRByTicket(out []byte, ticket string, notBefore time.Time) (bool, error) {
	var prs []prTicketSearchEntry
	if err := json.Unmarshal(out, &prs); err != nil {
		return false, fmt.Errorf("gh pr list (ticket fallback): parse: %w", err)
	}
	for _, pr := range prs {
		if !TicketWordMatch(pr.HeadRefName, ticket) && !TicketWordMatch(pr.Title, ticket) {
			continue
		}
		if pr.CreatedAt.Before(notBefore) {
			continue // pre-existing/foreign PR — not this run's work
		}
		return true, nil
	}
	return false, nil
}

// ParsePRDiffStatsHead is the pure core: the first row of a `gh pr list
// --json additions,deletions,changedFiles` array, or found=false for an
// empty list.
func ParsePRDiffStatsHead(out []byte) (PRDiffStats, bool, error) {
	var rows []PRDiffStats
	if err := json.Unmarshal(out, &rows); err != nil {
		return PRDiffStats{}, false, fmt.Errorf("gh pr list (diff stats): parse: %w", err)
	}
	if len(rows) == 0 {
		return PRDiffStats{}, false, nil
	}
	return rows[0], true, nil
}

// SelectPRDiffStatsByTicket is the diff-footprint parallel of
// MatchPRByTicket — same rules (whole-token ticket match AND created
// at/after notBefore), returning the matched PR's diff footprint.
func SelectPRDiffStatsByTicket(out []byte, ticket string, notBefore time.Time) (PRDiffStats, bool, error) {
	var rows []prTicketStatsEntry
	if err := json.Unmarshal(out, &rows); err != nil {
		return PRDiffStats{}, false, fmt.Errorf("gh pr list (diff stats, ticket): parse: %w", err)
	}
	for _, r := range rows {
		if !TicketWordMatch(r.HeadRefName, ticket) && !TicketWordMatch(r.Title, ticket) {
			continue
		}
		if r.CreatedAt.Before(notBefore) {
			continue // pre-existing/foreign PR — not this run's work
		}
		return PRDiffStats{Additions: r.Additions, Deletions: r.Deletions, ChangedFiles: r.ChangedFiles}, true, nil
	}
	return PRDiffStats{}, false, nil
}

// PickPRNumberByTicket is the pure core: the number of the first
// prTicketSearchEntry that word-matches ticket and was created at/after
// notBefore (same match + provenance rules as MatchPRByTicket).
func PickPRNumberByTicket(out []byte, ticket string, notBefore time.Time) (int, bool, error) {
	var prs []prTicketSearchEntry
	if err := json.Unmarshal(out, &prs); err != nil {
		return 0, false, fmt.Errorf("gh pr list (diff number): parse: %w", err)
	}
	for _, pr := range prs {
		if !TicketWordMatch(pr.HeadRefName, ticket) && !TicketWordMatch(pr.Title, ticket) {
			continue
		}
		if pr.CreatedAt.Before(notBefore) {
			continue
		}
		return pr.Number, true, nil
	}
	return 0, false, nil
}

// CriteriaMentionsTests reports whether the acceptance criteria reference
// tests (case-insensitive "test") — the trigger for requiring a test-file
// change (Unit 3).
func CriteriaMentionsTests(criteria string) bool {
	return strings.Contains(strings.ToLower(criteria), "test")
}

// DiffTouchesTestFile reports whether a unified diff adds/modifies a Go
// test file — a `_test.go` on a `+++ ` new-file line or a `diff --git`
// header. The fleet is Go; a non-Go repo simply would not enable this gate.
func DiffTouchesTestFile(diff string) bool {
	for _, line := range strings.Split(diff, "\n") {
		if (strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "diff --git ")) &&
			strings.Contains(line, "_test.go") {
			return true
		}
	}
	return false
}

// ExistsFn looks up whether a PR exists for branch's own head in repo.
type ExistsFn func(repo, branch string) (bool, error)

// ExistsByTicketFn is the ticket-scoped fallback lookup.
type ExistsByTicketFn func(repo, ticket string) (bool, error)

// PRExists reports whether a PR exists for branch in repo. Missing
// repo/branch returns an error so a caller does not treat an unverifiable
// state as "complete" (fail-closed toward NEX-468).
//
// Checks the conventional branch head first (existsFn); when that misses
// (or errors) and ticket is non-empty, falls back to searching open PRs by
// ticket ID in the head branch name or title (existsByTicketFn — NET-46: a
// worker may have committed to its own branch name instead of the
// conventional one, opening a real PR the head-only check can't find).
func PRExists(repo, branch, ticket string, existsFn ExistsFn, existsByTicketFn ExistsByTicketFn) (bool, error) {
	if repo == "" || branch == "" {
		return false, fmt.Errorf("PRExists: repo/branch not set (repo=%q branch=%q)", repo, branch)
	}
	ok, err := existsFn(repo, branch)
	if err == nil && ok {
		return true, nil
	}
	if strings.TrimSpace(ticket) == "" {
		return ok, err
	}
	found, ferr := existsByTicketFn(repo, ticket)
	if ferr != nil {
		if err != nil {
			return false, err
		}
		return false, ferr
	}
	if found {
		return true, nil
	}
	return ok, err
}

// DiffStatsFn fetches the diff footprint of the open PR on branch's head.
// found=false means no such PR (distinct from a real PR with a zero diff).
type DiffStatsFn func(repo, branch string) (PRDiffStats, bool, error)

// DiffStatsByTicketFn is the ticket-scoped fallback diff-stats lookup.
type DiffStatsByTicketFn func(repo, ticket string) (PRDiffStats, bool, error)

// PRSubstantial reports whether the run's PR has a non-empty diff clearing
// floor — the objective, pre-judge substance precondition (Unit 2).
// Fail-closed: a gh error returns (false, err). Resolves the same
// own-branch-then-ticket PR that PRExists credits. floor<=0 disables the
// check (returns true — back-compat). found=false (no PR to measure)
// returns (false, nil): PRExists already reports not-verified in that case;
// this just agrees without inventing an error.
func PRSubstantial(repo, branch, ticket string, floor int, statsFn DiffStatsFn, statsByTicketFn DiffStatsByTicketFn) (bool, error) {
	if floor <= 0 {
		return true, nil
	}
	stats, found, err := statsFn(repo, branch)
	if err != nil {
		return false, err
	}
	if !found && strings.TrimSpace(ticket) != "" {
		stats, found, err = statsByTicketFn(repo, ticket)
		if err != nil {
			return false, err
		}
	}
	if !found {
		return false, nil
	}
	return stats.ChangedFiles >= 1 && (stats.Additions+stats.Deletions) >= floor, nil
}
