// Admin subcommand surface. Currently just the central nexus_md
// editor (Part 9c); future admin operations land here too rather than
// growing the top-level subcommand list.

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

// runAdminSubcommand parses `admin <verb> ...`.
func runAdminSubcommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: nexus admin <nexus-md>")
		return 2
	}
	switch args[0] {
	case "nexus-md":
		return runAdminNexusMD(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown admin subcommand %q (expected: nexus-md)\n", args[0])
		return 2
	}
}

// runAdminNexusMD parses `admin nexus-md <verb> ...`.
func runAdminNexusMD(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: nexus admin nexus-md <edit|show>")
		return 2
	}
	switch args[0] {
	case "edit":
		return runAdminNexusMDEdit(args[1:])
	case "show":
		return runAdminNexusMDShow(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown nexus-md subcommand %q (expected: edit, show)\n", args[0])
		return 2
	}
}

// runAdminNexusMDEdit opens $EDITOR on the current central nexus_md
// content. On save, parses the result and writes via SettingsStore.
//
// Like `nexus personality edit` (Part 7a), this CLI writes directly
// to the DB. A running broker won't pick up the change until the
// operator hits the REST endpoint OR restarts. Documented in the
// success message.
func runAdminNexusMDEdit(args []string) int {
	fs := flag.NewFlagSet("admin nexus-md edit", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	editor := fs.String("editor", "", "editor command (falls back to EDITOR env, then platform default)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	db, err := storage.Open(ctx, *dataDir, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin nexus-md edit: open db: %v\n", err)
		return 1
	}
	defer db.Close()
	settings := aspects.NewSQLSettingsStore(db)

	current, err := settings.Get(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin nexus-md edit: read current: %v\n", err)
		return 1
	}

	// Reuse the personality-edit `openEditor` helper. Pre-fill with
	// the current central content. We normalise both sides by trimming
	// trailing whitespace before comparing — defends against editors
	// that auto-append a trailing newline on save-without-edit (vim
	// with fixendofline, common default), which would otherwise trigger
	// a spurious version bump and refresh subscribers churn for nothing.
	edited, err := openEditor(*editor, "central-nexus-md", current.NexusMD)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin nexus-md edit: editor: %v\n", err)
		return 1
	}
	editedTrim := strings.TrimRight(edited, "\n\r \t")
	currentTrim := strings.TrimRight(current.NexusMD, "\n\r \t")
	if editedTrim == currentTrim {
		fmt.Fprintln(os.Stderr, "admin nexus-md edit: no changes — bailing out")
		return 0
	}

	newVersion, err := settings.SetNexusMD(ctx, editedTrim)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin nexus-md edit: write: %v\n", err)
		return 1
	}

	fmt.Printf("nexus_md: version %d → %d (%d bytes)\n",
		current.Version, newVersion, len(edited))
	fmt.Println()
	fmt.Println("Note: this CLI writes directly to the DB. A running broker will NOT")
	fmt.Println("see the change until it restarts (or until the running broker's REST")
	fmt.Println("endpoint is hit — `PUT /api/admin/nexus-md`).")
	fmt.Println("Remote aspects pick up at next JWT re-validation (default 1h).")
	return 0
}

// runAdminNexusMDShow prints the current central nexus_md to stdout
// and the version + size to stderr. Useful for inspection without
// opening an editor.
func runAdminNexusMDShow(args []string) int {
	fs := flag.NewFlagSet("admin nexus-md show", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := storage.Open(ctx, *dataDir, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin nexus-md show: open db: %v\n", err)
		return 1
	}
	defer db.Close()

	settings := aspects.NewSQLSettingsStore(db)
	current, err := settings.Get(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin nexus-md show: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "version: %d  bytes: %d  updated_at: %s\n",
		current.Version, len(current.NexusMD), current.UpdatedAt)
	if current.NexusMD == "" {
		fmt.Fprintln(os.Stderr, "(empty — run `nexus admin nexus-md edit` to populate)")
		return 0
	}
	fmt.Print(current.NexusMD)
	if !endsWithNewline(current.NexusMD) {
		fmt.Println()
	}
	return 0
}

func endsWithNewline(s string) bool {
	return len(s) > 0 && s[len(s)-1] == '\n'
}
