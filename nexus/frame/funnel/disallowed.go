// Disallowed claude-native tools for funnel-driven aspects. Ported from
// the agent-network harness (code/harness/index.js — same list, same
// reasoning) and applied at the bridle claudecode.Provider construction
// site so every aspect's `claude -p` subprocess starts with these tools
// explicitly blocked via --disallowedTools.
//
// Why these specifically: a funnel-driven aspect's response channel is
// the turn's plain stdout — bridle parses claude-code's stream-json,
// the funnel auto-posts assistant text blocks to chat. The native tools
// listed here are session-orchestration primitives that either
//
//   - spawn long-lived children that orphan the moment the per-turn
//     `claude -p` exits (Agent, SendMessage, Task*, Team*, Monitor,
//     EnterWorktree, Cron*, ScheduleWakeup, PushNotification,
//     RemoteTrigger), or
//   - create a parallel response channel that bypasses chat auto-post
//     entirely (SendMessage → another agent over named pipe;
//     AskUserQuestion / ExitPlanMode → the local CLI's stdout, which
//     no operator ever sees).
//
// Trigger: 2026-05-15, harrow used SendMessage 22× to "reply" to shadow
// with the ACP survey instead of emitting prose for the funnel auto-post.
// claude-code had auto-loaded the full default toolkit because nothing
// was passing --disallowedTools.

package funnel

// DisallowedNativeTools is the kill list applied to every funnel-driven
// claudecode subprocess. Kept in sync with agent-network's
// code/harness/index.js DISALLOWED_TOOLS by intent — when the agent-
// network list grows, this list should grow with it.
var DisallowedNativeTools = []string{
	// Comms tools that don't fit the funnel substrate (held over from
	// agent-network — kept because nexus-comms-mcp may still expose
	// these surfaces in future and we want them blocked by default).
	"mcp__comms__set_status",
	"mcp__comms__set_tier",
	"mcp__comms__set_wake",
	"mcp__comms__check_wake",
	"mcp__comms__schedule_wake",
	"mcp__comms__create_watch",
	"mcp__comms__list_watches",
	"mcp__comms__acknowledge_watch",
	"mcp__comms__list_alarms",

	// Session-orchestration: background agents + teams that die when
	// the per-turn `claude -p` ends. Tasks/teams/subagents only make
	// sense in an interactive session with a long-lived parent.
	"Agent",
	"TaskCreate",
	"TaskGet",
	"TaskList",
	"TaskOutput",
	"TaskStop",
	"TaskUpdate",
	"TeamCreate",
	"TeamDelete",
	"SendMessage",
	"Monitor",

	// Worktree / scheduling / cron / remote-trigger / push notifications:
	// all session-scoped or operator-CLI-scoped. Funnel aspects have no
	// operator CLI to push to, and any cron/wakeup state evaporates when
	// the subprocess exits.
	"EnterWorktree",
	"ExitWorktree",
	"CronCreate",
	"CronDelete",
	"CronList",
	"ScheduleWakeup",
	"PushNotification",
	"RemoteTrigger",

	// Native operator-question / plan-mode tools. nexus aspects route
	// operator questions via chat (the funnel auto-posts), not via the
	// local CLI's stdout. AskUserQuestion / ExitPlanMode would emit
	// content to a stdout no operator is reading.
	"AskUserQuestion",
	"ExitPlanMode",
}
