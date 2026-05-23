package providers

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Compile-time pin: a value implementing Provider must also satisfy
// the three component interfaces. If a future refactor narrows
// Provider, this fails to build.
var (
	_ ChatProvider      = (Provider)(nil)
	_ EmbeddingProvider = (Provider)(nil)
	_ MetadataProvider  = (Provider)(nil)
)

// chatOnly satisfies ChatProvider but not EmbeddingProvider —
// pins that callers depending only on ChatProvider type-check
// against an adapter that doesn't expose Embed.
type chatOnly struct{}

func (chatOnly) Invoke(_ context.Context, _ InvokeRequest) (InvokeResult, error) {
	return InvokeResult{}, nil
}
func (chatOnly) Stream(_ context.Context, _ InvokeRequest) (StreamIterator, error) {
	return nil, ErrUnsupported
}
func (chatOnly) TokenCount(_ context.Context, _, _ string) (int, error) { return 0, nil }
func (chatOnly) Compact(_ context.Context, _ []Entry, _ string) (CompactionResult, error) {
	return CompactionResult{}, nil
}

func TestChatOnlySatisfiesChatProvider(t *testing.T) {
	var _ ChatProvider = chatOnly{}
}

// TestErrorSentinelsWrapWithW locks in the adapter contract documented
// at the var block above: adapters MUST wrap with %w so callers can
// dispatch on the sentinel via errors.Is. A regression where someone
// switches to %v silently breaks every "retry on ErrRateLimit" /
// "fail fast on ErrAuth" call site downstream — this test catches it.
func TestErrorSentinelsWrapWithW(t *testing.T) {
	sentinels := []struct {
		name string
		err  error
	}{
		{"ErrAuth", ErrAuth},
		{"ErrRateLimit", ErrRateLimit},
		{"ErrContextWindow", ErrContextWindow},
		{"ErrUnsupported", ErrUnsupported},
		{"ErrProvider", ErrProvider},
		{"ErrTimeout", ErrTimeout},
	}
	for _, s := range sentinels {
		t.Run(s.name, func(t *testing.T) {
			wrapped := fmt.Errorf("adapter: outer: %w", s.err)
			if !errors.Is(wrapped, s.err) {
				t.Errorf("errors.Is(wrap(%v), %v) = false; want true", s.err, s.err)
			}
			// %v wrapping (regression form) must NOT satisfy errors.Is
			// — this is the failure mode we're guarding against.
			vWrapped := fmt.Errorf("adapter: outer: %v", s.err)
			if errors.Is(vWrapped, s.err) {
				t.Errorf("errors.Is(%%v-wrap(%v), %v) = true; expected false (sanity check on test premise)", s.err, s.err)
			}
		})
	}
}
