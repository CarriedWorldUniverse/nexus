package dispatch

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
)

// Brief is one dispatch request: the structured header plus the task text.
type Brief struct {
	Agent    string `json:"agent"`
	Provider string `json:"provider,omitempty"`
	Repo     string `json:"repo"`
	Ticket   string `json:"ticket"`
	Branch   string `json:"branch"`
	Thread   string `json:"thread"`
	// DispatchMsgID is the persisted chat message id of the !dispatch command.
	DispatchMsgID int64 `json:"dispatch_msg_id,omitempty"`
	// RunID is set by the broker's Runner at dispatch time — a unique run
	// identity per dispatched job, used for the job/run labels and (later)
	// cost-trace correlation.
	RunID string `json:"run_id,omitempty"`
	// ParentRunID is the RunID of the job that sub-dispatched this run.
	// Empty for root dispatches (operator → shadow).
	ParentRunID string `json:"parent_run_id,omitempty"`

	// SpawnParent marks a hand brief (NEX-571): the BASE aspect this
	// worker is a fresh-context instance of. Agent is then the derived
	// identity `<SpawnParent>.sub-N`. Empty for ticket dispatches.
	SpawnParent string `json:"spawn_parent,omitempty"`

	// SessionJWT is the broker-minted derived credential injected into
	// a hand's Job env in place of the aspect keyfile volume. Set by
	// Runner.launch at spawn time (never enqueued/serialised — fresh
	// TTL per launch).
	SessionJWT string `json:"-"`

	// --- Role-at-spawn (M1 Unit 3, PHASE2-DESIGN §3 / ROLE-MODEL.md) ---
	// Stamps a spawned worker with its ROLE (system-prompt overlay),
	// scoped skills, and tool policy at boot. All fields below are
	// additive and optional: empty/nil reproduces today's behavior
	// exactly (no role overlay, all skills, static -policy file only).

	// Role is the role LABEL (short name — e.g. "builder", "tester",
	// "reviewer") this spawn is dispatched under. Reconciled at Wave 2
	// fold: M1 Unit 4's pool leases (pool.go) stamp this for
	// accountability (completionSummary's "role=" line); it carries NO
	// prompt text (see RolePrompt for that). Empty for named-agent
	// dispatch outside the pool.
	Role string `json:"role,omitempty"`

	// RolePrompt is the RESOLVED role system-prompt text (not a role
	// name/id) for this work-item's assigned role — e.g. the builder/
	// tester/reviewer prompt from docs/network/roles/*.yaml. The
	// orchestrator resolves the role name to this prompt text at
	// dispatch time (the simplest delivery option per the build spec)
	// and agentfunnel's composeSystemPrompt prepends it above the
	// (thin) personality. Empty = no role overlay. (M1 Unit 3; renamed
	// from Role at the Wave 2 fold to free that name for the Unit 4
	// role LABEL above.)
	RolePrompt string `json:"role_prompt,omitempty"`

	// WorkItemID identifies the pool work-item this spawn serves
	// (PHASE2-DESIGN §1/§3) — distinct from Ticket, which is the VCS/
	// idempotency key. Informational: carried into Job labels/env for
	// accountability and log correlation.
	WorkItemID string `json:"work_item_id,omitempty"`

	// SkillAllowlist scopes this spawn's .agents/skills materialization
	// (nexus-skills-mcp search_skills/get_skill) to exactly these skill
	// names — the least-privilege skill-gating primitive (ROLE-MODEL §9).
	// Empty = all skills (today's ungated behavior).
	SkillAllowlist []string `json:"skill_allowlist,omitempty"`

	// PolicyFragment is a spawn-supplied funnel.ToolPolicy overlay applied
	// over the static -policy file by agentfunnel's loadToolPolicy — the
	// Tier-B delivery mechanism, but per-spawn rather than per-aspect.
	// Nil = no overlay (today's static-file-only behavior).
	PolicyFragment *funnel.ToolPolicy `json:"policy_fragment,omitempty"`

	// Personality is the thin, display-only label (name/voice/chat
	// attribution) stamped on this spawn — decoration, never load-bearing
	// knowledge (ROLE-MODEL §3). Carried into Job labels/env for
	// accountability; distinct from the broker-resolved personality
	// bundle (res.Personality) that composeSystemPrompt already layers in.
	Personality string `json:"personality,omitempty"`

	// RequestedPersonality is a pool work item's requested pool personality
	// (workgraph.WorkItem.Personality, threaded by the orchestrator's
	// dispatchOne -> dispatch.PoolItem.Personality) — the brain the item
	// wants, honored strictly by the pool lease (tryLeaseWorkerSlot): free
	// -> leased to exactly this personality, busy -> the item queues
	// carrying this field so reserveQueued keeps targeting the same
	// personality on later drains, never substituting a different one.
	// Empty = "any free personality" (today's behavior, unchanged). Unlike
	// Personality (below), this is the REQUEST, present before a lease
	// succeeds; Personality is the resolved, ACTUAL identity a lease
	// stamped once it did.
	RequestedPersonality string `json:"requested_personality,omitempty"`

	// AcceptanceCriteria carries the ledger work item's DoD checklist
	// (workgraph.WorkItem.AcceptanceCriteria, formatted one-per-line) into
	// the spawn — Unit B "verified task_done" (NET-22/23/24): the funnel
	// judges the builder's task_done claim against this text before
	// honoring completion, instead of trusting the model's self-report
	// unconditionally. Empty = no criteria captured on this dispatch (e.g.
	// non-ledger !dispatch), which reproduces today's unconditional-honor
	// behavior exactly — see agentfunnel's builderOnTaskDone.
	AcceptanceCriteria string `json:"acceptance_criteria,omitempty"`

	Task string `json:"-"`
}

// briefConfigMapData builds the Data map for the "brief-<taskID>" ConfigMap
// (mounted at /etc/dispatch — see BuildJob). brief.md always carries the
// task text, unchanged from before role-at-spawn. role.md/policy.json are
// added only when the brief carries a RolePrompt/PolicyFragment, and their
// paths are only passed to agentfunnel (via -role-file/-policy-fragment-file
// in builderArgs) when present — so an empty RolePrompt/PolicyFragment
// reproduces today's ConfigMap and command line exactly.
func briefConfigMapData(b Brief) (map[string]string, error) {
	data := map[string]string{"brief.md": b.Task}
	if b.RolePrompt != "" {
		data[briefRoleFileName] = b.RolePrompt
	}
	if b.AcceptanceCriteria != "" {
		data[briefAcceptanceFileName] = b.AcceptanceCriteria
	}
	if b.PolicyFragment != nil {
		raw, err := json.Marshal(b.PolicyFragment)
		if err != nil {
			return nil, fmt.Errorf("dispatch: marshal policy fragment: %w", err)
		}
		data[briefPolicyFragmentName] = string(raw)
	}
	return data, nil
}

// ParseBrief extracts either a fenced JSON header or a !dispatch command brief.
func ParseBrief(body []byte) (Brief, error) {
	s := string(body)
	if b, ok, err := parseDispatchCommand(s); ok || err != nil {
		return b, err
	}

	open := strings.Index(s, "```json")
	if open < 0 {
		return Brief{}, errors.New("dispatch: no ```json brief header")
	}
	rest := s[open+len("```json"):]
	close := strings.Index(rest, "```")
	if close < 0 {
		return Brief{}, errors.New("dispatch: unterminated ```json header")
	}

	var b Brief
	if err := json.Unmarshal([]byte(strings.TrimSpace(rest[:close])), &b); err != nil {
		return Brief{}, fmt.Errorf("dispatch: bad brief header: %w", err)
	}
	b.Task = strings.TrimSpace(rest[close+3:])
	if b.Agent == "" {
		return Brief{}, errors.New("dispatch: brief.agent required")
	}
	if b.Ticket == "" {
		return Brief{}, errors.New("dispatch: brief.ticket required (idempotency key)")
	}
	if b.Thread == "" {
		b.Thread = b.Ticket
	}
	return b, nil
}

func parseDispatchCommand(s string) (Brief, bool, error) {
	line := strings.TrimSpace(s)
	if line != "!dispatch" && !strings.HasPrefix(line, "!dispatch ") {
		return Brief{}, false, nil
	}
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return Brief{}, true, errors.New("dispatch: !dispatch requires agent and task")
	}
	target := fields[1]
	agent, provider, ok := strings.Cut(target, "%")
	if agent == "" {
		return Brief{}, true, errors.New("dispatch: !dispatch agent required")
	}
	if ok && provider == "" {
		return Brief{}, true, errors.New("dispatch: !dispatch provider required after %")
	}
	// Optional leading directives after the agent token: repo=, branch=,
	// ticket=. Consume them in order; the first non-directive field begins
	// the task. An unknown key=value (or any bare word) is task text — so a
	// task may itself contain "x=1".
	var repo, branch, ticket string
	i := 2
	for ; i < len(fields); i++ {
		key, val, isKV := strings.Cut(fields[i], "=")
		if !isKV {
			break
		}
		switch key {
		case "repo":
			repo = val
		case "branch":
			branch = val
		case "ticket":
			ticket = val
		default:
			isKV = false
		}
		if !isKV {
			break
		}
	}
	task := strings.TrimSpace(strings.Join(fields[i:], " "))
	if task == "" {
		return Brief{}, true, errors.New("dispatch: !dispatch task required")
	}
	// The ticket is the idempotency key + the builder/<ticket> branch name.
	// Use the operator's explicit ticket when given; otherwise derive a stable
	// hash of the command line.
	if ticket == "" {
		ticket = "dispatch-" + fmt.Sprintf("%x", sha256.Sum256([]byte(line)))[:16]
	}
	return Brief{
		Agent:    agent,
		Provider: provider,
		Repo:     repo,
		Branch:   branch,
		Ticket:   ticket,
		Thread:   ticket,
		Task:     task,
	}, true, nil
}
