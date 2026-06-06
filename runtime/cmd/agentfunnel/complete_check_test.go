package main

import (
	"errors"
	"log/slog"
	"testing"
)

func TestBuilderCompleteCheck(t *testing.T) {
	orig := prExistsFn
	defer func() { prExistsFn = orig }()
	log := slog.Default()

	cases := []struct {
		name     string
		fn       func(repo, ticket string) (bool, error)
		wantStop bool
		wantRet  bool
	}{
		{"pr exists -> stop", func(_, _ string) (bool, error) { return true, nil }, true, true},
		{"no pr -> continue", func(_, _ string) (bool, error) { return false, nil }, false, false},
		{"check error -> continue", func(_, _ string) (bool, error) { return false, errors.New("boom") }, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prExistsFn = tc.fn
			stopped := false
			got := builderCompleteCheck(func() { stopped = true }, log, "plumb", "org/repo", "NEX-1")()
			if got != tc.wantRet {
				t.Errorf("return = %v, want %v", got, tc.wantRet)
			}
			if stopped != tc.wantStop {
				t.Errorf("stopped = %v, want %v", stopped, tc.wantStop)
			}
		})
	}
}

func TestPrExistsRequiresRepoTicket(t *testing.T) {
	if _, err := prExists("", "NEX-1"); err == nil {
		t.Error("empty repo should be unverifiable (error)")
	}
	if _, err := prExists("org/repo", ""); err == nil {
		t.Error("empty ticket should be unverifiable (error)")
	}
}
