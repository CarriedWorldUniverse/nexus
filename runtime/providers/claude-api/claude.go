// Package claudeapi implements the Nexus provider contract against
// Anthropic's Claude API.
//
// Chat is live (Invoke + TokenCount). Embeddings are not supported —
// Anthropic has no embeddings endpoint (spec §9.1). Streaming and
// Compact are stubbed with ErrUnsupported for part 2; part 6 fills
// in Compact. Streaming returns the same until the runtime needs it.
package claudeapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared"

	"github.com/CarriedWorldUniverse/nexus/runtime/providers"
)

// ProviderName is the adapter identifier used in aspect.json.
const ProviderName = "claude-api"

// DefaultModel is used when the caller leaves InvokeRequest.Model empty.
// Matches keel memory: `claude-opus-4-7[1m]` is the production target.
const DefaultModel = "claude-opus-4-7"

// TriageModelID — cheap/fast model for low-stakes turns per spec §5.
const TriageModelID = "claude-haiku-4-5"

// Default maximum output tokens when caller supplies 0.
const DefaultMaxTokens = 4096

// CredentialsFile is the per-aspect credentials file name (spec §7.2).
const CredentialsFile = "claude-api.json"

// Credentials is the on-disk format.
type Credentials struct {
	APIKey string `json:"api_key"`
}

// Provider implements providers.Provider for Claude.
type Provider struct {
	client *anthropic.Client
}

// New constructs a Provider from an API key. Returns ErrAuth if key
// is empty.
func New(apiKey string) (*Provider, error) {
	if apiKey == "" {
		return nil, providers.ErrAuth
	}
	c := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &Provider{client: &c}, nil
}

// NewFromAspectHome loads `<aspectHome>/.credentials/claude-api.json`
// and constructs a Provider. Returns ErrAuth if the file is missing
// or malformed.
func NewFromAspectHome(aspectHome string) (*Provider, error) {
	path := filepath.Join(aspectHome, ".credentials", CredentialsFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s not found", providers.ErrAuth, path)
		}
		return nil, fmt.Errorf("%w: reading %s: %v", providers.ErrAuth, path, err)
	}
	var creds Credentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return nil, fmt.Errorf("%w: parsing %s: %v", providers.ErrAuth, path, err)
	}
	if strings.TrimSpace(creds.APIKey) == "" {
		return nil, fmt.Errorf("%w: %s has empty api_key", providers.ErrAuth, path)
	}
	return New(creds.APIKey)
}

// -------------------------------------------------------------------
// Provider interface
// -------------------------------------------------------------------

// Invoke runs a single turn. Translates Nexus normalised types to the
// Anthropic SDK's MessageNewParams, invokes, normalises the result.
func (p *Provider) Invoke(ctx context.Context, req providers.InvokeRequest) (providers.InvokeResult, error) {
	model := req.Model
	if model == "" {
		model = DefaultModel
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = DefaultMaxTokens
	}

	messages, err := buildMessages(req)
	if err != nil {
		return providers.InvokeResult{}, err
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: int64(maxTokens),
		Messages:  messages,
	}
	if req.SystemPrompt != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.SystemPrompt}}
	}
	if len(req.Tools) > 0 {
		params.Tools = translateTools(req.Tools)
	}

	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return providers.InvokeResult{}, normaliseError(err)
	}

	return normaliseResponse(resp)
}

// Stream — not yet wired up. Part 2's scope is blocking Invoke;
// streaming lands when the dashboard needs live deltas.
func (p *Provider) Stream(ctx context.Context, req providers.InvokeRequest) (providers.StreamIterator, error) {
	return nil, providers.ErrUnsupported
}

// TokenCount calls Anthropic's count-tokens endpoint for a crude
// single-string payload. The compaction trigger (§2.7) uses this to
// decide when to cut.
func (p *Provider) TokenCount(ctx context.Context, model string, payload string) (int, error) {
	if model == "" {
		model = DefaultModel
	}
	resp, err := p.client.Messages.CountTokens(ctx, anthropic.MessageCountTokensParams{
		Model: anthropic.Model(model),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(payload)),
		},
	})
	if err != nil {
		return 0, normaliseError(err)
	}
	return int(resp.InputTokens), nil
}

// Compact summarises prior-context entries using the triage model
// (Haiku) for speed and cost — the summary doesn't need Opus-grade
// reasoning. Emits a CompactionResult carrying the summary text and
// token usage. Per §2.7 the runtime decides when to compact; this
// method only executes the summarisation.
func (p *Provider) Compact(ctx context.Context, entries []providers.Entry, hint string) (providers.CompactionResult, error) {
	if len(entries) == 0 {
		return providers.CompactionResult{}, errors.New("claude-api.Compact: no entries to summarise")
	}

	// Serialise the entries into a transcript the summariser can read.
	var transcript strings.Builder
	inputTokensBefore := 0
	for _, e := range entries {
		role := compactionRoleFor(e.Kind)
		text := entryText(e)
		if text == "" {
			continue
		}
		transcript.WriteString(role)
		transcript.WriteString(": ")
		transcript.WriteString(text)
		transcript.WriteString("\n\n")
	}

	// Crude pre-count — reasonable approximation; real count comes
	// from the provider response.
	inputTokensBefore = estimateTokens(transcript.String())

	prompt := buildCompactionPrompt(hint, transcript.String())

	resp, err := p.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(TriageModelID),
		MaxTokens: 2048,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return providers.CompactionResult{}, normaliseError(err)
	}

	var summary strings.Builder
	for _, block := range resp.Content {
		if text, ok := block.AsAny().(anthropic.TextBlock); ok {
			summary.WriteString(text.Text)
		}
	}

	return providers.CompactionResult{
		Summary:      strings.TrimSpace(summary.String()),
		Model:        TriageModelID,
		TokensBefore: inputTokensBefore,
		TokensAfter:  int(resp.Usage.OutputTokens),
	}, nil
}

// compactionRoleFor maps an entry kind to a role label for the
// summarisation transcript.
func compactionRoleFor(k providers.EntryKind) string {
	switch k {
	case providers.EntryTurnUser:
		return "user"
	case providers.EntryTurnAssistant:
		return "assistant"
	case providers.EntryTurnToolResult:
		return "tool_result"
	case providers.EntrySystemPrompt:
		return "system"
	case providers.EntryCompaction:
		return "prior_summary"
	case providers.EntryBranchSummary:
		return "branch_summary"
	default:
		return string(k)
	}
}

// buildCompactionPrompt wraps the transcript in a summarisation
// instruction. Kept deliberately simple — provider-specific prompt
// tuning is forge territory.
func buildCompactionPrompt(hint, transcript string) string {
	var b strings.Builder
	b.WriteString("Summarise the following conversation transcript into a concise briefing that preserves:\n")
	b.WriteString("- the goals and constraints discussed\n")
	b.WriteString("- decisions made and their justifications\n")
	b.WriteString("- any outstanding questions or action items\n")
	b.WriteString("- concrete facts (names, dates, paths, error messages) that would be hard to reconstruct\n\n")
	if hint != "" {
		b.WriteString("Additional instruction: ")
		b.WriteString(hint)
		b.WriteString("\n\n")
	}
	b.WriteString("TRANSCRIPT:\n")
	b.WriteString(transcript)
	b.WriteString("\nSUMMARY:\n")
	return b.String()
}

// estimateTokens is a rough char-count ÷ 4 heuristic for the input
// side of the summarisation call. Exact counts can come from
// TokenCount; this is only used for the Result's TokensBefore field
// which is informational.
func estimateTokens(s string) int { return len(s) / 4 }

// Embed — Anthropic has no embeddings endpoint. Fixed per spec §9.1.
func (p *Provider) Embed(ctx context.Context, req providers.EmbedRequest) (providers.EmbedResult, error) {
	return providers.EmbedResult{}, providers.ErrUnsupported
}

// Capabilities — spec §9.1, §10.
func (p *Provider) Capabilities() providers.Capabilities {
	return providers.Capabilities{
		Streaming:          false, // flip true when Stream is implemented
		ToolUse:            true,
		Vision:             true,
		LongContext:        true, // [1m] variant supported via model id suffix
		InSessionModelSwap: false,
		ThinkingLevels:     []string{"off", "minimal", "low", "medium", "high", "xhigh"},
		MaxContextTokens:   200_000, // default model window; [1m] variants override at model-select time
		SupportsTriage:     true,

		Embeddings:     false,
		EmbeddingModel: "",
		EmbeddingDim:   0,
		Chat:           true,
	}
}

// Models returns a static list of currently-useful models. Live-fetch
// from the API is deferred — the set rarely changes and the list above
// drives dashboard filter UX, not dispatch logic.
func (p *Provider) Models(ctx context.Context) ([]providers.Model, error) {
	return []providers.Model{
		{ID: "claude-opus-4-7", DisplayName: "Claude Opus 4.7", MaxContextTokens: 200_000},
		{ID: "claude-opus-4-7[1m]", DisplayName: "Claude Opus 4.7 (1M context)", MaxContextTokens: 1_000_000},
		{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6", MaxContextTokens: 200_000},
		{ID: "claude-haiku-4-5", DisplayName: "Claude Haiku 4.5", MaxContextTokens: 200_000},
	}, nil
}

// TriageModel — spec §5.
func (p *Provider) TriageModel() string { return TriageModelID }

// -------------------------------------------------------------------
// Translation helpers (Nexus normalised ↔ Anthropic SDK)
// -------------------------------------------------------------------

// buildMessages replays the active-branch entries into Anthropic
// MessageParams plus the new user Prompt turn at the end.
//
// Tool calls and tool results are NOT YET supported — part 7 wires
// them up end-to-end. Until then, encountering a tool-result entry is
// a runtime bug (a history with tool interactions was handed to a
// tool-unaware adapter); we fail loudly rather than drop them
// silently, because silent drops cause the model to hallucinate or
// misalign and the resulting behaviour is painful to debug.
func buildMessages(req providers.InvokeRequest) ([]anthropic.MessageParam, error) {
	out := make([]anthropic.MessageParam, 0, len(req.Context)+1)

	for _, entry := range req.Context {
		switch entry.Kind {
		case providers.EntryTurnUser:
			if text := entryText(entry); text != "" {
				out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(text)))
			}
		case providers.EntryTurnAssistant:
			if text := entryText(entry); text != "" {
				out = append(out, anthropic.NewAssistantMessage(anthropic.NewTextBlock(text)))
			}
			// TODO(part7): also reconstruct tool_use blocks from Payload
			// when the assistant turn included tool calls.
		case providers.EntryTurnToolResult:
			return nil, fmt.Errorf("%w: tool-result entries not supported yet (landing in part 7); entry id=%s",
				providers.ErrUnsupported, entry.ID)
		case providers.EntrySystemPrompt:
			// System prompts get composed separately by the runtime and
			// land in MessageNewParams.System, not in Messages.
		case providers.EntryCompaction, providers.EntryBranchSummary:
			if text := entryText(entry); text != "" {
				// Treat as a synthetic user message so the model sees
				// the summary as context, not as a prior assistant turn.
				out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(text)))
			}
		}
	}

	if req.Prompt != "" {
		out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(req.Prompt)))
	}
	return out, nil
}

// entryText extracts a text payload from an entry. Entries store their
// content under a `text` or `content` key in Payload; callers that need
// richer structure (images, tool blocks) compose their own block sets.
func entryText(e providers.Entry) string {
	if e.Payload == nil {
		return ""
	}
	if v, ok := e.Payload["text"].(string); ok {
		return v
	}
	if v, ok := e.Payload["content"].(string); ok {
		return v
	}
	return ""
}

// translateTools converts Nexus tool definitions to Anthropic's
// function-calling schema. Anthropic's shape is the closest of the
// three major providers to Nexus's normalised form.
//
// For flat object schemas (the common case: `{type: object, properties,
// required}`) we extract `properties` and `required` directly. For
// schemas using `$ref`, `oneOf`, `anyOf`, or any non-standard root
// shape, we pass the full schema under `Properties` — Anthropic's API
// accepts arbitrary JSON Schema, and the model is more likely to
// handle a rich schema correctly than an empty one. A tighter
// normaliser is forge territory once we have concrete multi-provider
// schema-dialect test cases.
func translateTools(tools []providers.ToolDefinition) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		props, required := extractSchema(t.InputSchema)
		out = append(out, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Name,
				Description: anthropic.String(t.Description),
				InputSchema: anthropic.ToolInputSchemaParam{
					Properties: props,
					Required:   required,
				},
			},
		})
	}
	return out
}

// extractSchema returns (properties, required) for an Anthropic tool
// definition. For a standard flat-object schema, returns the inlined
// properties and required keys. For anything else (ref-using schemas,
// oneOf/anyOf-rooted schemas, or nil), returns the whole schema as
// properties and nil required — better than silently passing an empty
// shape to the model.
func extractSchema(schema map[string]any) (any, []string) {
	if schema == nil {
		return map[string]any{}, nil
	}

	// Recognise the flat-object case: type=object + properties key.
	isObject := false
	if t, ok := schema["type"].(string); ok && t == "object" {
		isObject = true
	}
	props, hasProps := schema["properties"]
	if isObject && hasProps {
		return props, extractRequired(schema)
	}

	// Non-flat case — pass the whole schema and let Anthropic handle it.
	return schema, extractRequired(schema)
}

func extractRequired(schema map[string]any) []string {
	raw, ok := schema["required"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// normaliseResponse converts the Anthropic SDK's Message response into
// the Nexus InvokeResult shape.
func normaliseResponse(resp *anthropic.Message) (providers.InvokeResult, error) {
	out := providers.InvokeResult{
		StopReason: normaliseStopReason(string(resp.StopReason)),
		Tokens: providers.TokenCounts{
			Input:  int(resp.Usage.InputTokens),
			Output: int(resp.Usage.OutputTokens),
			Total:  int(resp.Usage.InputTokens + resp.Usage.OutputTokens),
		},
		Cost: providers.CostRecord{
			InputTokens:  int(resp.Usage.InputTokens),
			OutputTokens: int(resp.Usage.OutputTokens),
		},
		ProviderRaw: resp,
	}

	var textBuilder strings.Builder
	for _, block := range resp.Content {
		switch variant := block.AsAny().(type) {
		case anthropic.TextBlock:
			textBuilder.WriteString(variant.Text)
		case anthropic.ToolUseBlock:
			args := map[string]any{}
			if len(variant.Input) > 0 {
				if err := json.Unmarshal(variant.Input, &args); err != nil {
					// A malformed tool-use Input means the model intended
					// arguments we can't recover. Forwarding empty args
					// silently dispatches a tool call divergent from the
					// model's intent — surface the failure instead.
					return providers.InvokeResult{}, fmt.Errorf("%w: tool_use %q input parse: %v",
						providers.ErrProvider, variant.Name, err)
				}
			}
			out.ToolCalls = append(out.ToolCalls, providers.ToolCall{
				ID:        variant.ID,
				Name:      variant.Name,
				Arguments: args,
			})
		}
	}
	out.Output = textBuilder.String()
	return out, nil
}

func normaliseStopReason(raw string) providers.StopReason {
	switch raw {
	case "end_turn":
		return providers.StopEndTurn
	case "tool_use":
		return providers.StopToolUse
	case "max_tokens":
		return providers.StopMaxTokens
	case "stop_sequence":
		// Model hit a caller-supplied stop string. Nexus doesn't yet
		// distinguish that from natural end-of-turn; fold into EndTurn
		// until a caller actually uses stop_sequences.
		return providers.StopEndTurn
	default:
		return providers.StopErrorReason
	}
}

// normaliseError maps SDK errors to the Nexus error taxonomy. Prefers
// the SDK's typed ErrorType discriminator over string-matching the
// human-readable message. Falls back to status code, then to phrase
// matches that are specific enough not to collide with unrelated 400s.
func normaliseError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		msg := apiErr.Error()
		switch apiErr.Type() {
		case shared.ErrorTypeAuthenticationError, shared.ErrorTypePermissionError:
			return fmt.Errorf("%w: %s", providers.ErrAuth, msg)
		case shared.ErrorTypeRateLimitError, shared.ErrorTypeOverloadedError:
			return fmt.Errorf("%w: %s", providers.ErrRateLimit, msg)
		case shared.ErrorTypeTimeoutError:
			return fmt.Errorf("%w: %s", providers.ErrTimeout, msg)
		case shared.ErrorTypeInvalidRequestError:
			// 400s from Anthropic come back as invalid_request_error for
			// many reasons. Context-window overflow is the one case the
			// runtime recovers from (force compaction + retry). Use a
			// specific phrase, not the word "token" alone — 400s about
			// bad tool schemas routinely mention tokens.
			lower := strings.ToLower(msg)
			if strings.Contains(lower, "prompt is too long") ||
				strings.Contains(lower, "context window") ||
				strings.Contains(lower, "max tokens exceeded") {
				return fmt.Errorf("%w: %s", providers.ErrContextWindow, msg)
			}
			return fmt.Errorf("%w: %s", providers.ErrProvider, msg)
		default:
			// Fall back on status code for error types we haven't
			// enumerated (keep-forward compatible with new types).
			switch apiErr.StatusCode {
			case 401, 403:
				return fmt.Errorf("%w: %s", providers.ErrAuth, msg)
			case 429:
				return fmt.Errorf("%w: %s", providers.ErrRateLimit, msg)
			default:
				return fmt.Errorf("%w: %s", providers.ErrProvider, msg)
			}
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", providers.ErrTimeout, err)
	}
	return fmt.Errorf("%w: %v", providers.ErrProvider, err)
}
