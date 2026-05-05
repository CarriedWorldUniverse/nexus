package providers

import (
	"context"
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
