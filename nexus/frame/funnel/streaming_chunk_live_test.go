package funnel

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/bridle/provider/openai"
)

// TestLive_StreamingChatSink_OpenAIPerTokenFanout — diagnostic for
// the plumb chunking pathology (operator reported ~40 chat rows for
// a single multi-sentence reply on deepseek-v4-pro).
//
// Hypothesis: streamingChatSink.Emit posts every bridle.ModelChunk
// as a separate ChatGateway.SendChat call. Designed for claudecode's
// block-shaped stream events (one ModelChunk == one text block),
// the same code path on openai-shape providers hits per-token
// content deltas — every token becomes a chat row.
//
// This test wires:
//   1. real openai bridle provider against DeepSeek /v1
//   2. funnel.Config with StreamTextToChat=true (mirrors agentfunnel's
//      production config — runtime/cmd/agentfunnel/main.go:384)
//   3. recordingChatGateway to count SendChat invocations
//
// Asserts len(gateway.sends) <= 3 — generous upper bound for a
// single multi-sentence reply. A failure with ~N (tens) of sends
// reproduces the pathology operator sees on plumb.
//
// Env-gated: needs DEEPSEEK_OPENAI_API_KEY (or DEEPSEEK_REASONER_KEY)
// and DEEPSEEK_REASONER_MODEL (defaults to deepseek-reasoner).
func TestLive_StreamingChatSink_OpenAIPerTokenFanout(t *testing.T) {
	key := os.Getenv("DEEPSEEK_REASONER_KEY")
	if key == "" {
		key = os.Getenv("DEEPSEEK_OPENAI_API_KEY")
	}
	if key == "" {
		t.Skip("DEEPSEEK_REASONER_KEY (or DEEPSEEK_OPENAI_API_KEY) not set; skipping live streaming-sink diagnostic")
	}
	model := os.Getenv("DEEPSEEK_REASONER_MODEL")
	if model == "" {
		model = "deepseek-reasoner"
	}

	chat := &recordingChatGateway{}
	prov := openai.NewWithBaseURL(key, "https://api.deepseek.com/v1")

	f, err := New(Config{
		AspectID: "plumb-repro",
		SystemPrompt: `You are plumb, an aspect of Convergence in a multi-agent network.

Style: loose, sketchy, comfortable being wrong on the way to right. Write the way someone thinks aloud.

You communicate by replying naturally — your output text is the chat reply.`,
		Harness:          bridle.NewHarness(prov),
		Provider:         bridle.ProviderOpenAI,
		Model:            model,
		Runner:           NullRunner{},
		ChatGateway:      chat,
		StreamTextToChat: true,
		// No Tools — force the model down the natural-text path so
		// streamingChatSink is the only thing translating output to
		// chat. (Mirrors plumb's path when there's no tool call.)
	})
	if err != nil {
		t.Fatalf("funnel.New: %v", err)
	}

	f.ReceiveWithMsgID(bridle.InboxItem{
		From:       "operator",
		Content:    "Hey plumb — give me your two-sentence take on whether worktrees-by-default is a good idea for agent tasks.",
		ThreadRoot: 1000,
	}, 1001)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	if _, err := f.Deliberate(ctx, ""); err != nil {
		t.Fatalf("Deliberate: %v", err)
	}

	// Always log the raw send shape so the diagnostic is visible even
	// when the assertion fires.
	t.Logf("streamingChatSink emitted %d SendChat call(s) for a single multi-sentence reply:", len(chat.sends))
	for i, s := range chat.sends {
		preview := s.Text
		if len(preview) > 60 {
			preview = preview[:60] + "…"
		}
		t.Logf("  [%d] reply_to=%d text=%q", i, s.ReplyTo, preview)
	}

	if len(chat.sends) == 0 {
		t.Fatalf("expected at least 1 chat send; got 0 (auto-post may have been suppressed without streaming)")
	}
	// Generous upper bound: a two-sentence reply chunked into
	// paragraph-shaped blocks shouldn't exceed 3 posts. Tens of posts
	// = the per-token fanout pathology.
	if len(chat.sends) > 3 {
		t.Errorf("streamingChatSink produced %d chat rows for one reply — per-token fanout pathology reproduced", len(chat.sends))
	}
}
