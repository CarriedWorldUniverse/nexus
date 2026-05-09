package handexec

import (
	"errors"
	"fmt"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/runtime/providers"
)

// Regression for issue #35: provider error strings must not round-trip
// across the dispatch boundary. providerErrorCode maps every known
// provider sentinel to a small set of opaque codes; rich detail stays
// in local logs.
func TestProviderErrorCodeRedactsToSentinel(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"auth", fmt.Errorf("%w: api-key sk-ant-xxx not authorised", providers.ErrAuth), "provider_auth"},
		{"rate_limit", fmt.Errorf("%w: too many requests", providers.ErrRateLimit), "provider_rate_limit"},
		{"context_window", fmt.Errorf("%w: prompt is too long: 250000 tokens", providers.ErrContextWindow), "provider_context_window"},
		{"timeout", fmt.Errorf("%w: deadline exceeded", providers.ErrTimeout), "provider_timeout"},
		{"unsupported", fmt.Errorf("%w: streaming not implemented", providers.ErrUnsupported), "provider_unsupported"},
		{"provider", fmt.Errorf("%w: 500 internal server error from upstream", providers.ErrProvider), "provider_error"},
		{"unknown", errors.New("plain error with prompt fragment 'secret'"), "provider_internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := providerErrorCode(tc.err)
			if got != tc.want {
				t.Errorf("providerErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
			}
			// The code must NEVER include any of the rich error text.
			// This is the load-bearing security property.
			msg := tc.err.Error()
			if got != "" && len(msg) > len(got) && containsAny(got, "sk-ant-xxx", "secret", "prompt is too long") {
				t.Errorf("code %q leaked detail from error %q", got, msg)
			}
		})
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) == 0 {
			continue
		}
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}
