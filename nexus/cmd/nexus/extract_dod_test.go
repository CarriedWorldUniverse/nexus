package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/ledger"
)

func TestExtractDoD(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "no DoD marker",
			content: "just a regular message",
			want:    "",
		},
		{
			name:    "marker without colon",
			content: "Some preamble\nDefinition of Done\n- [ ] task one\n- [ ] task two",
			want:    "- [ ] task one\n- [ ] task two",
		},
		{
			name:    "marker with colon",
			content: "Some preamble\nDefinition of Done:\n- [ ] task one\n- [ ] task two",
			want:    "- [ ] task one\n- [ ] task two",
		},
		{
			name:    "marker with colon and space",
			content: "Some preamble\nDefinition of Done: \n- [ ] task one",
			want:    "- [ ] task one",
		},
		{
			name:    "marker with ## prefix",
			content: "Some preamble\n## Definition of Done\n- [ ] task one",
			want:    "- [ ] task one",
		},
		{
			name:    "DoD terminated by H2",
			content: "Preamble\nDefinition of Done:\n- [ ] task one\n- [ ] task two\n\n## Next Section\nmore content",
			want:    "- [ ] task one\n- [ ] task two",
		},
		{
			name:    "DoD terminated by H1",
			content: "Preamble\nDefinition of Done:\n- [ ] task one\n\n# Next Section\nmore content",
			want:    "- [ ] task one",
		},
		{
			name:    "DoD at end of content",
			content: "Preamble\nDefinition of Done:\n- [ ] final task\n",
			want:    "- [ ] final task",
		},
		{
			name:    "DoD body with content then H2 terminator",
			content: "Preamble\nDefinition of Done:\n- [ ] task\n\n## Next Section",
			want:    "- [ ] task",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractDoD(tt.content)
			if got != tt.want {
				t.Errorf("extractDoD() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsTicketKey(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"NEX-226", true},
		{"NEX-1", true},
		{"ABC-12345", true},
		{"Z-0", false},      // too short (3 chars)
		{"nex-226", false},  // lowercase project
		{"NEX-", false},     // no digits after dash
		{"-226", false},     // no project prefix
		{"NEX226", false},   // no dash
		{"", false},         // empty
		{"NEX-abc", false},  // non-digit suffix
		{"NE-226", true},    // short project prefix
		{"NEX--226", false}, // double dash
		{"NEX", false},      // no dash at all
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isTicketKey(tt.input)
			if got != tt.want {
				t.Errorf("isTicketKey(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestResolveTicketID(t *testing.T) {
	// Shared logger that writes to test log.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	t.Run("empty topic", func(t *testing.T) {
		got := resolveTicketID(context.Background(), "", nil, log)
		if got != "unknown" {
			t.Errorf("resolveTicketID with empty topic = %q, want 'unknown'", got)
		}
	})

	t.Run("non-ticket-key topic", func(t *testing.T) {
		got := resolveTicketID(context.Background(), "keel-creds-fix", nil, log)
		if got != "unknown" {
			t.Errorf("resolveTicketID with non-ticket-key topic = %q, want 'unknown'", got)
		}
	})

	t.Run("ticket-key topic with nil ledger", func(t *testing.T) {
		got := resolveTicketID(context.Background(), "NEX-226", nil, log)
		if got != "unknown" {
			t.Errorf("resolveTicketID with nil ledger = %q, want 'unknown'", got)
		}
	})

	t.Run("ticket-key topic not found in ledger", func(t *testing.T) {
		svc, err := ledger.New(context.Background(), ledger.Config{DBPath: ":memory:"})
		if err != nil {
			t.Fatalf("ledger.New: %v", err)
		}
		defer svc.Close()
		got := resolveTicketID(context.Background(), "NEX-999", svc, log)
		if got != "unknown" {
			t.Errorf("resolveTicketID with unknown key = %q, want 'unknown'", got)
		}
	})

	t.Run("ticket-key topic found in ledger", func(t *testing.T) {
		svc, err := ledger.New(context.Background(), ledger.Config{DBPath: ":memory:"})
		if err != nil {
			t.Fatalf("ledger.New: %v", err)
		}
		defer svc.Close()
		if err := svc.CreateProject(context.Background(), ledger.Project{
			Key:  "NEX",
			Name: "Nexus",
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		issue, err := svc.CreateIssue(context.Background(), ledger.IssueDraft{
			Project:          "NEX",
			Type:             "Story",
			Summary:          "Test ticket for resolveTicketID",
			DefinitionOfDone: "test passes",
			Reporter:         "keel",
		})
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		got := resolveTicketID(context.Background(), issue.Key, svc, log)
		if got != issue.Key {
			t.Errorf("resolveTicketID with valid key = %q, want %q", got, issue.Key)
		}
	})
}

func TestExtractDoD_RealisticQueueManagerDispatch(t *testing.T) {
	// Simulates the content shape the queue manager injects via
	// HandleChatSend / f.Receive — a chat message with [TICKET NEX-XXX]
	// prefix and a Definition of Done section.
	content := `@keel [TICKET NEX-226] Replace topic-as-ticketID heuristic with ledger ticket lookup

Priority: High
Status: To Do

Definition of Done:
- extractDoD becomes pure DoD parser
- resolveTicketID looks up ticket via ledger
- go vet clean
- tests cover all paths`

	dod := extractDoD(content)
	if dod == "" {
		t.Fatal("extractDoD returned empty for realistic queue-manager dispatch content")
	}
	if !strings.Contains(dod, "extractDoD becomes pure DoD parser") {
		t.Errorf("DoD missing expected item; got: %s", dod)
	}
	if !strings.Contains(dod, "go vet clean") {
		t.Errorf("DoD missing expected item; got: %s", dod)
	}
}
