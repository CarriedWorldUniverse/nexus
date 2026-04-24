// Package providers defines the Nexus Provider interface and the
// normalised types that flow across the adapter boundary. Provider
// adapters (claude-api, ollama-local, gemini-api, etc.) live in
// sibling sub-packages and implement this interface.
//
// See `docs/2026-04-24-provider-adapter-spec.md` §3 for the contract.
package providers

import (
	"context"
	"errors"
	"time"
)

// Error taxonomy — adapters surface normalised errors so the runtime
// can apply uniform retry and fallback policy regardless of provider.
// Wrap with %w when returning.
var (
	ErrAuth          = errors.New("provider: auth")
	ErrRateLimit     = errors.New("provider: rate limit")
	ErrContextWindow = errors.New("provider: context window exceeded")
	ErrUnsupported   = errors.New("provider: feature unsupported")
	ErrProvider      = errors.New("provider: upstream error")
	ErrTimeout       = errors.New("provider: timeout")
)

// Provider is the contract every model backend adapter satisfies.
// An adapter may implement only a subset (e.g. a pure-embeddings
// adapter like ollama-local returns ErrUnsupported from Invoke); the
// runtime reads Capabilities before dispatch.
type Provider interface {
	// Invoke runs a single request/response turn.
	Invoke(ctx context.Context, req InvokeRequest) (InvokeResult, error)

	// Stream returns a delta iterator for a turn. Optional — adapters
	// without streaming support return ErrUnsupported and the runtime
	// falls back to Invoke.
	Stream(ctx context.Context, req InvokeRequest) (StreamIterator, error)

	// TokenCount returns the token count for a given payload under a
	// given context window / model. Used by the compaction trigger.
	TokenCount(ctx context.Context, model string, payload string) (int, error)

	// Compact summarises prior context into a CompactionEntry. Called
	// by the runtime when tokens exceed window - reserve.
	Compact(ctx context.Context, entries []Entry, hint string) (CompactionResult, error)

	// Embed produces a fixed-length vector for a single text. Optional
	// — adapters without an embeddings endpoint return ErrUnsupported.
	Embed(ctx context.Context, req EmbedRequest) (EmbedResult, error)

	// Capabilities advertises what this adapter supports. Static for
	// the lifetime of the adapter instance.
	Capabilities() Capabilities

	// Models returns the current model list. May hit the provider API
	// at first call and cache; or be static.
	Models(ctx context.Context) ([]Model, error)

	// TriageModel is the cheap/fast model name for low-stakes turns.
	// Empty string means triage is unsupported for this adapter.
	TriageModel() string
}

// InvokeRequest is the normalised input (spec §3.1).
type InvokeRequest struct {
	// Context is the tree-structured session entries, already replayed
	// along the active branch by the runtime. Adapter composes the
	// provider-native conversation history from these.
	Context []Entry

	// Prompt is the new user/invoker turn content (text).
	Prompt string

	// SystemPrompt is SOUL + CLAUDE.md + aspect directives, composed.
	SystemPrompt string

	// Tools are Nexus tool definitions. Adapter translates to provider
	// function-calling dialect.
	Tools []ToolDefinition

	// Model is the provider-scoped model id. Adapter uses its default
	// if empty.
	Model string

	// ThinkingLevel — off/minimal/low/medium/high/xhigh. Adapter
	// ignores if unsupported. Empty string = adapter default.
	ThinkingLevel string

	// Timeout caps the call. Zero means adapter default.
	Timeout time.Duration

	// MaxTokens caps output length. Zero means adapter default.
	MaxTokens int
}

// InvokeResult is the normalised output (spec §3.2).
type InvokeResult struct {
	Output     string
	ToolCalls  []ToolCall
	StopReason StopReason
	Cost       CostRecord
	Tokens     TokenCounts
	// UpdatedContext are the entries to append to the session tree
	// (the new assistant turn at minimum).
	UpdatedContext []Entry
	// ProviderRaw is opaque — for audit and debugging, not for Nexus logic.
	ProviderRaw any
}

// StopReason mirrors spec §3.2 — the normalised set.
type StopReason string

const (
	StopEndTurn     StopReason = "end_turn"
	StopToolUse     StopReason = "tool_use"
	StopMaxTokens   StopReason = "max_tokens"
	StopTimeout     StopReason = "timeout"
	StopErrorReason StopReason = "error"
)

// TokenCounts reports usage for a single invocation.
type TokenCounts struct {
	Input  int
	Output int
	Total  int
}

// CostRecord carries dollar cost if the provider reports it; zero-
// valued if unknown.
type CostRecord struct {
	InputTokens  int
	OutputTokens int
	USD          float64
}

// ToolDefinition is the Nexus tool shape (spec §4.1). Adapters
// translate this to Anthropic / Gemini / OpenAI dialects.
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema map[string]any // JSON Schema draft-2020-12
	OutputHint  string
	Readonly    bool
}

// ToolCall is the normalised tool-call shape returned from the model
// (spec §4.2).
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}

// Entry is a session tree entry (spec §2.6). Runtime owns the tree;
// adapters only see a replayed active branch.
type Entry struct {
	ID       string         // ULID
	ParentID string         // empty for root
	Kind     EntryKind
	TS       time.Time
	Payload  map[string]any
}

// EntryKind enumerates the tree entry types. Detail in registration
// spec §2.6.
type EntryKind string

const (
	EntryTurnUser       EntryKind = "turn.user"
	EntryTurnAssistant  EntryKind = "turn.assistant"
	EntryTurnToolResult EntryKind = "turn.tool_result"
	EntrySystemPrompt   EntryKind = "system.prompt"
	EntryCompaction     EntryKind = "compaction"
	EntryBranchSummary  EntryKind = "branch_summary"
)

// CompactionResult is what the runtime gets back from Compact — the
// summary and token accounting the CompactionEntry needs.
type CompactionResult struct {
	Summary      string
	Model        string
	TokensBefore int
	TokensAfter  int
}

// EmbedRequest — provider-adapter spec §3.3.
type EmbedRequest struct {
	Text  string
	Model string // provider-scoped embedding model id (empty = default)
}

// EmbedResult — provider-adapter spec §3.3.
type EmbedResult struct {
	Vector []float32
	Model  string
	Dim    int
}

// Capabilities advertises adapter features (spec §10).
type Capabilities struct {
	Streaming          bool
	ToolUse            bool
	Vision             bool
	LongContext        bool
	InSessionModelSwap bool
	ThinkingLevels     []string
	MaxContextTokens   int
	SupportsTriage     bool

	Embeddings     bool
	EmbeddingModel string
	EmbeddingDim   int
	Chat           bool // false for pure-embedding adapters
}

// Model describes a single concrete model exposed by a provider.
type Model struct {
	ID               string
	DisplayName      string
	MaxContextTokens int
}

// StreamIterator yields deltas for a streaming invocation. Adapters
// that don't stream return ErrUnsupported from Stream and the runtime
// falls back to Invoke.
type StreamIterator interface {
	Next(ctx context.Context) (Delta, error)
	Close() error
}

// Delta is a single streaming chunk — partial text, partial tool call,
// or a terminal marker. StopReason is set on the last delta only.
type Delta struct {
	Text     string
	ToolCall *ToolCall
	Done     bool
	Stop     StopReason
	Usage    *TokenCounts
}
