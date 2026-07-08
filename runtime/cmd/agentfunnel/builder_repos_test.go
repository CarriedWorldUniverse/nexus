package main

import "testing"

func TestRepoRemoteURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		// git@ SSH URLs pass through unchanged.
		{`git@github.com:owner/repo.git`, `git@github.com:owner/repo.git`},
		// Trailing slash gets trimmed and the default github.com remote is built.
		{`owner/repo/`, `https://github.com/owner/repo.git`},
		// Explicit http URL passes through unchanged.
		{`https://example.com/owner/repo.git`, `https://example.com/owner/repo.git`},
		// Normal shorthand still works.
		{`owner/repo`, `https://github.com/owner/repo.git`},
		// Single-name shorthand still resolves against the default owner.
		{`single`, `https://github.com/CarriedWorldUniverse/single.git`},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			got := repoRemoteURL(tc.in)
			if got != tc.want {
				t.Fatalf("repoRemoteURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}