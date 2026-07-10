// Authoritative gate runner (#473, cairn#99 Option B —
// docs/network/ACCEPTANCE-GATE-HARDENING.md "Pull-checks wiring"). Re-runs
// the SAME four builder gates (pr-exists, pr-substantial, acceptance-judge,
// test-evidence) that runtime/cmd/agentfunnel's builderPRVerifier /
// builderAcceptanceGate already run — but from HERE, the orchestrator/broker
// side, against the PUSHED artifact (gh, the actual PR diff), where the
// model being gated cannot forge the verdict by shelling out or narrating.
//
// This is the "produce the verdicts" half of the separation-of-duties story
// cairn#99 calls for: today's agentfunnel gates are advisory (the worker
// self-reports and the worker process itself decides whether it's done);
// this package additionally recomputes the objective gates broker-side.
// Recording those verdicts durably (as cairn pull checks, superseding the
// dark f57c370/#468 wiring that currently rides an insecure trust boundary)
// is #474 — deliberately NOT done here. RunAuthoritativeGates only produces
// + slogs the verdicts.
//
// Shares its decision logic with agentfunnel via
// nexus/frame/funnel/gates — see that package's doc comment.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel/gates"
)

// Gate verdict names — fixed literals matching the #468 pull-checks
// vocabulary exactly (runtime/cmd/agentfunnel/builder_pullchecks.go
// pullCheck* consts / docs/network/ACCEPTANCE-GATE-HARDENING.md "Check-name
// vocabulary") so #474's recorder relocation can record these verdicts
// under the same check names cairn-server (and any dashboard/tooling built
// against them) already expects.
const (
	GatePRExists        = "pr-exists"
	GatePRSubstantial   = "pr-substantial"
	GateAcceptanceJudge = "acceptance-judge"
	GateTestEvidence    = "test-evidence"
)

// Gate verdict states — mirrors runtime/pullchecks.StatePass/StateFail (not
// imported directly: this package produces verdicts, #474's recorder is
// what maps them onto a cairn pull check, and that relocation is
// deliberately out of scope here — see package doc).
const (
	GateStatePass = "pass"
	GateStateFail = "fail"
)

// GateVerdict is one authoritative gate's outcome — the orchestrator-side
// parallel of the pull-check row #468 records, without the cairn-specific
// framing (pull id, etc: that's #474's concern).
type GateVerdict struct {
	Name        string // one of the Gate* consts above
	State       string // GateStatePass | GateStateFail
	Summary     string
	EvidenceURL string
}

// authGateRPCTimeout bounds every INDIVIDUAL default gh subprocess call the
// gate runner issues (mirrors runtime/cmd/agentfunnel/builder_pullchecks.go
// pullCheckRPCTimeout). OnJobDoneHook (wake.go) runs inside WatchJobs'
// single-goroutine namespace-wide select loop (runtime/dispatch/k8s.go) —
// there is no external kill switch on this path the way a worker pod has
// activeDeadlineSeconds, so a hung/unreachable `gh` (GitHub outage,
// rate-limit, an interactive auth prompt) would otherwise wedge job-done
// processing for EVERY builder in the namespace. Bounding per-call (rather
// than once around the whole RunAuthoritativeGates ctx) means one slow gate
// eats only its own budget, not the next gate's too. A var (not a const) so
// tests can shrink it instead of sleeping 5s+ per case.
var authGateRPCTimeout = 5 * time.Second

// PRExistsFunc/PRExistsByTicketFunc/PRDiffStatsFunc/PRDiffStatsByTicketFunc/
// PRDiffFunc are the ctx-aware gh-backed lookups RunAuthoritativeGates uses —
// context-aware (exec.CommandContext, matching the #468 arc's convention:
// see runtime/cmd/agentfunnel/main.go prURLFn) so a hung/unreachable gh can
// never stall the orchestrator's job-done path indefinitely: every DEFAULT
// implementation below derives its own bounded sub-context from ctx via
// authGateRPCTimeout before it shells out. Swappable via GateRunnerOptions
// (tests inject fakes the same way agentfunnel's prExistsFn/prDiffStatsFn
// package vars do — just parameterized here instead of package-global, since
// Orchestrator is a shared, potentially-concurrent library, not
// agentfunnel's one-shot-per-dispatch process) — an INJECTED fn is the
// caller's own responsibility to bound; RunAuthoritativeGates itself imposes
// no timeout on a caller-supplied fn (it already receives ctx and can derive
// its own deadline), only on its own defaults.
type (
	PRExistsFunc            func(ctx context.Context, repo, branch string) (bool, error)
	PRExistsByTicketFunc    func(ctx context.Context, repo, ticket string) (bool, error)
	PRDiffStatsFunc         func(ctx context.Context, repo, branch string) (gates.PRDiffStats, bool, error)
	PRDiffStatsByTicketFunc func(ctx context.Context, repo, ticket string) (gates.PRDiffStats, bool, error)
	PRDiffFunc              func(ctx context.Context, repo, branch, ticket string) (string, bool, error)
)

// GateRunnerOptions configures RunAuthoritativeGates. The zero value is
// EXPLICITLY inert: Enabled defaults false, so a caller that forgets to set
// it up gets a guaranteed no-op (dark-by-default, same posture as #468's
// CW_PULL_* wiring) rather than an accidental live run against real gh/judge
// endpoints.
type GateRunnerOptions struct {
	// Enabled gates the ENTIRE runner. false (the zero value) means
	// RunAuthoritativeGates returns (nil, nil) immediately — no gh calls, no
	// judge calls, not even for a configured Verifier. This is the runner's
	// dark default; an orchestrator wires it live only once its own
	// production readiness (auth, judge credential, gh availability) is
	// confirmed.
	Enabled bool

	// ExistsFn/ExistsByTicketFn/DiffStatsFn/DiffStatsByTicketFn/DiffFn
	// override the default gh-backed lookups (below) — nil uses the
	// package's own ctx-aware `gh` implementation. Tests set these to seed
	// a fake PR/diff without shelling out.
	ExistsFn            PRExistsFunc
	ExistsByTicketFn    PRExistsByTicketFunc
	DiffStatsFn         PRDiffStatsFunc
	DiffStatsByTicketFn PRDiffStatsByTicketFunc
	DiffFn              PRDiffFunc

	// MinDiffLines is the pr-substantial floor (additions+deletions) — see
	// gates.PRSubstantial. <=0 disables that gate (back-compat: an existing
	// PR is substantial regardless of size). Callers should mirror
	// agentfunnel's ACCEPTANCE_MIN_DIFF_LINES default (1) unless they have a
	// reason to diverge.
	MinDiffLines int

	// Verifier is the acceptance judge — reused from
	// nexus/frame/funnel/acceptance.go via judge.BuildAcceptanceVerifier
	// (the SAME brain config the worker-advisory path uses; see
	// docs/network/MODEL-SELECTOR.md for the cheap-judge tier). nil skips
	// the acceptance-judge gate entirely (no verdict emitted, no judge
	// call) — the judge is the one gate this runner treats as genuinely
	// optional infrastructure, mirroring funnel.AcceptanceVerifier's own
	// "nil verifier = unavailable" contract.
	Verifier *funnel.AcceptanceVerifier

	// RequireTestEvidence enables the test-evidence gate (Unit 3) — mirrors
	// agentfunnel's ACCEPTANCE_REQUIRE_TEST_DIFF opt-in. false (default) =
	// the gate never runs, never emits a verdict (fail-open, matching the
	// worker-advisory posture exactly).
	RequireTestEvidence bool

	// NotBefore is the Unit 4 provenance floor for the ticket-search
	// fallback (gates.MatchPRByTicket / SelectPRDiffStatsByTicket /
	// PickPRNumberByTicket): a PR credited via the loose ticket match must
	// have been created at/after this instant. Callers should set this to
	// the job's dispatch time (mirrors agentfunnel's provenanceNotBefore,
	// which uses process-start time — the orchestrator has no equivalent
	// single process lifetime, so it must be threaded in explicitly). The
	// zero Time makes the guard inert (every PR passes it) — acceptable for
	// tests, NOT recommended for production wiring.
	NotBefore time.Time
}

// RunAuthoritativeGates re-runs the four builder gates against the PUSHED
// artifact for (repo, branch, ticket) — ground truth (gh, the real PR diff),
// not the worker's self-report. Returns the verdicts it was able to
// evaluate; a gate whose precondition isn't met (see the doc table in
// nexus/orchestrator/README or ACCEPTANCE-GATE-HARDENING.md) simply emits no
// verdict for itself, mirroring the #468 pull-checks "recorded when" column
// exactly:
//
//   - pr-exists:        every call (opts.Enabled), pass or fail.
//   - pr-substantial:   only when pr-exists passed.
//   - acceptance-judge: only when opts.Verifier is set, criteria is
//     non-empty, AND the run's own-PR diff was fetched successfully
//     (ground truth or nothing — see package doc: this is the gate that
//     must never rubber-stamp a narrative it can't verify).
//   - test-evidence:    only when opts.RequireTestEvidence, criteria
//     mentions tests, AND a diff was fetched.
//
// opts.Enabled=false is the dark default: returns nil immediately, no
// gh/judge calls at all.
//
// No error return: every gate below is fail-closed-to-a-verdict or
// fail-open-to-silence internally (see the per-gate comments) — there is no
// hard-failure mode that isn't already expressed as a GateVerdict (a "fail"
// state) or an absent verdict. An earlier revision carried an (unused)
// error return; dropped rather than left dead (review finding, NEX-473
// follow-up).
func RunAuthoritativeGates(ctx context.Context, repo, branch, ticket, criteria string, opts GateRunnerOptions) []GateVerdict {
	if !opts.Enabled {
		return nil
	}

	// notBefore threads opts.NotBefore into the DEFAULT ticket-fallback
	// implementations below (they are plain package funcs, not closures
	// over opts, so it's captured here rather than read from a field on
	// the func itself). Only the defaults need this — an injected
	// opts.*Fn is the caller's own closure and applies its own provenance
	// rule (or none, for a test).
	notBefore := opts.NotBefore

	existsFn := opts.ExistsFn
	if existsFn == nil {
		existsFn = func(ctx context.Context, repo, branch string) (bool, error) {
			return defaultPRExistsFn(ctx, repo, branch)
		}
	}
	existsByTicketFn := opts.ExistsByTicketFn
	if existsByTicketFn == nil {
		existsByTicketFn = func(ctx context.Context, repo, ticket string) (bool, error) {
			return defaultPRExistsByTicketFn(ctx, repo, ticket, notBefore)
		}
	}
	diffStatsFn := opts.DiffStatsFn
	if diffStatsFn == nil {
		diffStatsFn = func(ctx context.Context, repo, branch string) (gates.PRDiffStats, bool, error) {
			return defaultPRDiffStatsFn(ctx, repo, branch)
		}
	}
	diffStatsByTicketFn := opts.DiffStatsByTicketFn
	if diffStatsByTicketFn == nil {
		diffStatsByTicketFn = func(ctx context.Context, repo, ticket string) (gates.PRDiffStats, bool, error) {
			return defaultPRDiffStatsByTicketFn(ctx, repo, ticket, notBefore)
		}
	}
	diffFn := opts.DiffFn
	if diffFn == nil {
		diffFn = func(ctx context.Context, repo, branch, ticket string) (string, bool, error) {
			return defaultPRDiffFn(ctx, repo, branch, ticket, notBefore)
		}
	}

	var verdicts []GateVerdict

	// --- pr-exists: always evaluated when the runner is enabled. ---
	existsOK, existsErr := gates.PRExists(repo, branch, ticket,
		func(r, b string) (bool, error) { return existsFn(ctx, r, b) },
		func(r, t string) (bool, error) { return existsByTicketFn(ctx, r, t) },
	)
	existsSummary := "PR found on branch or ticket search"
	if existsErr != nil {
		existsSummary = existsErr.Error()
	} else if !existsOK {
		existsSummary = "no open PR found for this run"
	}
	verdicts = append(verdicts, GateVerdict{
		Name:    GatePRExists,
		State:   gateState(existsOK && existsErr == nil),
		Summary: existsSummary,
	})

	// --- pr-substantial: only reached after pr-exists passes. ---
	if existsOK && existsErr == nil {
		subOK, subErr := gates.PRSubstantial(repo, branch, ticket, opts.MinDiffLines,
			func(r, b string) (gates.PRDiffStats, bool, error) { return diffStatsFn(ctx, r, b) },
			func(r, t string) (gates.PRDiffStats, bool, error) { return diffStatsByTicketFn(ctx, r, t) },
		)
		subSummary := fmt.Sprintf("PR diff clears the substance floor (min_diff_lines=%d)", opts.MinDiffLines)
		if subErr != nil {
			subSummary = subErr.Error()
		} else if !subOK {
			subSummary = fmt.Sprintf("PR diff is empty or below the substance floor (min_diff_lines=%d)", opts.MinDiffLines)
		}
		verdicts = append(verdicts, GateVerdict{
			Name:    GatePRSubstantial,
			State:   gateState(subOK && subErr == nil),
			Summary: subSummary,
		})
	}

	// Resolve the run's own-PR diff once — both acceptance-judge and
	// test-evidence need it, and both are ground-truth-or-nothing (a diff
	// this runner can't fetch produces NO verdict for either, not a
	// rubber-stamped pass — see package doc).
	diff, diffFound, diffErr := diffFn(ctx, repo, branch, ticket)
	diffAvailable := diffErr == nil && diffFound && strings.TrimSpace(diff) != ""

	// --- acceptance-judge: only when configured, criteria given, and a
	// real diff was fetched. ---
	if opts.Verifier != nil && strings.TrimSpace(criteria) != "" && diffAvailable {
		// The brain/tier this judges on is opts.Verifier's own configured
		// Provider/Model — reused verbatim from the worker-advisory path's
		// judge.BuildAcceptanceVerifier (the cheap judge tier; see
		// docs/network/MODEL-SELECTOR.md). This runner does not invent a
		// second judge config.
		augmented := funnel.AugmentOutputWithDiff("", diff)
		verdict, verr := opts.Verifier.Verify(ctx, criteria, augmented)
		if verr == nil {
			verdicts = append(verdicts, GateVerdict{
				Name:    GateAcceptanceJudge,
				State:   gateState(verdict.Met),
				Summary: verdict.Reason,
			})
		}
		// verr != nil: fail-open on judge error, matching the worker-advisory
		// posture (docs/network/ACCEPTANCE-GATE-HARDENING.md "Posture &
		// interaction") — a flaky judge must not wedge the orchestrator. No
		// verdict emitted (mirrors the #468 "judge error records nothing" row).
	}

	// --- test-evidence: only when opted in, criteria mention tests, and a
	// real diff was fetched. ---
	if opts.RequireTestEvidence && gates.CriteriaMentionsTests(criteria) && diffAvailable {
		touches := gates.DiffTouchesTestFile(diff)
		summary := "diff changes a _test.go file"
		if !touches {
			summary = "criteria call for tests but the diff touches no _test.go file"
		}
		verdicts = append(verdicts, GateVerdict{
			Name:    GateTestEvidence,
			State:   gateState(touches),
			Summary: summary,
		})
	}

	return verdicts
}

func gateState(pass bool) string {
	if pass {
		return GateStatePass
	}
	return GateStateFail
}

// LogVerdicts is the THIS-TICKET consumer of RunAuthoritativeGates' output:
// structured slog lines, one per verdict. #474 additionally records these as
// cairn pull checks (relocating runtime/pullchecks.Recorder here) — until
// that lands, logging is the only sink, which is sufficient for this
// ticket's scope (produce the verdicts; #474 records them durably).
func LogVerdicts(log *slog.Logger, workItemID string, verdicts []GateVerdict) {
	if log == nil {
		log = slog.Default()
	}
	for _, v := range verdicts {
		log.Info("orchestrator: authoritative gate verdict",
			"work_item", workItemID, "gate", v.Name, "state", v.State, "summary", v.Summary, "evidence_url", v.EvidenceURL)
	}
}

// --- default ctx-aware gh-backed implementations ---
//
// These mirror runtime/cmd/agentfunnel/main.go's prExistsFn/
// prExistsByTicketFn/prDiffStatsFn/prDiffStatsByTicketFn/prDiffFn exactly,
// made ctx-aware via exec.CommandContext AND bounded by authGateRPCTimeout
// on every individual gh call (the #468 arc's convention for any gh
// subprocess reachable from a hook that must not block indefinitely — see
// main.go prURLFn's doc comment; here the bound is load-bearing rather than
// best-effort, since OnJobDoneHook has no other backstop — see
// authGateRPCTimeout's doc). They are package-level funcs (not vars) since
// GateRunnerOptions is how callers/tests substitute behavior here, not
// global reassignment.

func defaultPRExistsFn(ctx context.Context, repo, branch string) (bool, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, authGateRPCTimeout)
	defer cancel()
	out, err := exec.CommandContext(rpcCtx, "gh", "pr", "list", "--repo", repo, "--head", branch, "--state", "open",
		"--json", "url", "-q", ".[0].url").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("gh pr list: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func defaultPRExistsByTicketFn(ctx context.Context, repo, ticket string, notBefore time.Time) (bool, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, authGateRPCTimeout)
	defer cancel()
	out, err := exec.CommandContext(rpcCtx, "gh", "pr", "list", "--repo", repo, "--state", "open",
		"--json", "number,headRefName,title,createdAt").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("gh pr list (ticket fallback): %w: %s", err, strings.TrimSpace(string(out)))
	}
	return gates.MatchPRByTicket(out, ticket, notBefore)
}

func defaultPRDiffStatsFn(ctx context.Context, repo, branch string) (gates.PRDiffStats, bool, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, authGateRPCTimeout)
	defer cancel()
	out, err := exec.CommandContext(rpcCtx, "gh", "pr", "list", "--repo", repo, "--head", branch, "--state", "open",
		"--json", "additions,deletions,changedFiles").CombinedOutput()
	if err != nil {
		return gates.PRDiffStats{}, false, fmt.Errorf("gh pr list (diff stats): %w: %s", err, strings.TrimSpace(string(out)))
	}
	return gates.ParsePRDiffStatsHead(out)
}

func defaultPRDiffStatsByTicketFn(ctx context.Context, repo, ticket string, notBefore time.Time) (gates.PRDiffStats, bool, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, authGateRPCTimeout)
	defer cancel()
	out, err := exec.CommandContext(rpcCtx, "gh", "pr", "list", "--repo", repo, "--state", "open",
		"--json", "headRefName,title,additions,deletions,changedFiles,createdAt").CombinedOutput()
	if err != nil {
		return gates.PRDiffStats{}, false, fmt.Errorf("gh pr list (diff stats, ticket): %w: %s", err, strings.TrimSpace(string(out)))
	}
	return gates.SelectPRDiffStatsByTicket(out, ticket, notBefore)
}

// defaultPRDiffFn issues up to THREE sequential gh subprocesses (own-branch
// diff, then — only on miss — the ticket-fallback list + diff-by-number);
// each gets its OWN authGateRPCTimeout-bounded sub-context (not one shared
// budget across all three), so a slow-but-eventually-successful own-branch
// call doesn't starve the fallback's time, and vice versa.
func defaultPRDiffFn(ctx context.Context, repo, branch, ticket string, notBefore time.Time) (string, bool, error) {
	diffCtx, cancel := context.WithTimeout(ctx, authGateRPCTimeout)
	out, err := exec.CommandContext(diffCtx, "gh", "pr", "diff", branch, "--repo", repo).CombinedOutput()
	cancel()
	if err == nil {
		return string(out), strings.TrimSpace(string(out)) != "", nil
	}

	listCtx, cancel := context.WithTimeout(ctx, authGateRPCTimeout)
	numOut, nerr := exec.CommandContext(listCtx, "gh", "pr", "list", "--repo", repo, "--state", "open",
		"--json", "number,headRefName,title,createdAt").CombinedOutput()
	cancel()
	if nerr != nil {
		return "", false, fmt.Errorf("gh pr list (diff number): %w: %s", nerr, strings.TrimSpace(string(numOut)))
	}
	num, found, perr := gates.PickPRNumberByTicket(numOut, ticket, notBefore)
	if perr != nil {
		return "", false, perr
	}
	if !found {
		return "", false, nil
	}

	numDiffCtx, cancel := context.WithTimeout(ctx, authGateRPCTimeout)
	out2, err2 := exec.CommandContext(numDiffCtx, "gh", "pr", "diff", strconv.Itoa(num), "--repo", repo).CombinedOutput()
	cancel()
	if err2 != nil {
		return "", false, fmt.Errorf("gh pr diff #%d: %w: %s", num, err2, strings.TrimSpace(string(out2)))
	}
	return string(out2), strings.TrimSpace(string(out2)) != "", nil
}
