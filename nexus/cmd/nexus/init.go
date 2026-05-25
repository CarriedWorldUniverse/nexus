// `nexus init` subcommand (NEX-275).
//
// First-boot bootstrap: creates the data dir + subdirectories, opens +
// schema-bootstraps the broker/ledger SQLite databases, seeds a default
// project in the ledger, mints an initial admin-flagged operator token,
// and emits a sample MCP config template. Designed to be idempotent so
// re-running on an already-initialised dir is safe — the admin token
// re-mint resolves to the existing token rather than rotating it.
//
// Output shape (default):
//
//   ▸ data-dir: /Users/jacinta/.nexus
//   ✓ broker.db ready
//   ✓ ledger.db ready (default project: NEX)
//   ✓ keyfiles/, activity/, sessions/ created
//   ✓ sample.mcp.json written to <data-dir>/sample.mcp.json
//
//   ╭─ Operator admin token (Bearer-prefix this for Authorization header) ─╮
//   │ <token-bytes>                                                        │
//   ╰──────────────────────────────────────────────────────────────────────╯
//
//   Next steps:
//     1. nexus serve --data-dir <path>    # start the broker
//     2. set provider credentials:        # ./bin/nexus credential set ...
//     3. open https://localhost:<port>/   # dashboard with admin token
//
// With --quiet, only the token is printed (machine-parsing friendly for
// install scripts that need to capture it).
//
// This subcommand explicitly does NOT spawn aspects or start the
// broker — substrate-only. The umbrella repo's install.sh (NEX-286)
// invokes `nexus init` then `nexus serve` as separate steps.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/CarriedWorldUniverse/ledger"
	"github.com/CarriedWorldUniverse/nexus/nexus/broker"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

// runInitSubcommand is the entry point dispatched from main.go's
// subcommand switch. Returns a process exit code (0 = success).
func runInitSubcommand(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "directory for nexus state (databases, keyfiles, activity logs)")
	quiet := fs.Bool("quiet", false, "print only the admin token (no summary headers)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "init: --data-dir required (no default available on this platform)")
		return 2
	}

	resolved, err := filepath.Abs(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init: resolve data-dir: %v\n", err)
		return 1
	}

	out := os.Stdout
	step := func(msg string) {
		if !*quiet {
			fmt.Fprintln(out, msg)
		}
	}

	if !*quiet {
		fmt.Fprintf(out, "▸ data-dir: %s\n", resolved)
	}

	// 1. Create the data dir + the subdirectories the broker / aspect
	// runtime expect to exist at startup. mkdir-p semantics; safe on
	// re-init.
	for _, sub := range []string{"", "keyfiles", "activity", "sessions"} {
		dir := filepath.Join(resolved, sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "init: mkdir %s: %v\n", dir, err)
			return 1
		}
	}
	step("✓ data-dir + keyfiles/, activity/, sessions/ ready")

	ctx := context.Background()

	// 2. Open + bootstrap the broker database. storage.Open lays out
	// the file under <data-dir>/nexus.db; Bootstrap is idempotent.
	db, err := storage.Open(ctx, resolved, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init: open broker db: %v\n", err)
		return 1
	}
	defer db.Close()
	if err := storage.Bootstrap(ctx, db); err != nil {
		fmt.Fprintf(os.Stderr, "init: bootstrap broker db: %v\n", err)
		return 1
	}
	step("✓ broker.db schema ready")

	// 3. Open + bootstrap the ledger database. Idempotency caveat:
	// ledger.New's applySchema runs the embedded schema.sql as one
	// blob, and the ALTER TABLE migrations (v7→v8 etc.) lack
	// IF-NOT-EXISTS guards, so re-running on a populated DB fails
	// with "duplicate column". Skip the call entirely when ledger.db
	// already exists — the broker's own ledger.New on `nexus serve`
	// hits the same code path and the broker's caller (cmd/nexus
	// main path) likely needs the same fix. Tracked as a separate
	// ledger-repo follow-up.
	ledgerDBPath := filepath.Join(resolved, "ledger.db")
	if _, statErr := os.Stat(ledgerDBPath); statErr == nil {
		step("✓ ledger.db already initialised (skipping)")
	} else {
		ledgerSvc, err := ledger.New(ctx, ledger.Config{DBPath: ledgerDBPath})
		if err != nil {
			fmt.Fprintf(os.Stderr, "init: open ledger db: %v\n", err)
			return 1
		}
		if err := ledgerSvc.CreateProject(ctx, ledger.Project{Key: "NEX", Name: "Nexus"}); err != nil {
			ledgerSvc.Close()
			if !isAlreadyExistsErr(err) {
				fmt.Fprintf(os.Stderr, "init: seed default project: %v\n", err)
				return 1
			}
		}
		ledgerSvc.Close()
		step("✓ ledger.db schema ready (default project: NEX)")
	}

	// 4. Mint the operator admin token. ReconcileFrameTokenFor is the
	// "stored value wins" mint — re-running picks up the existing
	// token rather than rotating it (idempotent by design).
	tokens := broker.NewTokenStore()
	tok, err := tokens.ReconcileFrameTokenFor(ctx, db, "operator")
	if err != nil {
		fmt.Fprintf(os.Stderr, "init: mint admin token: %v\n", err)
		return 1
	}

	// 5. Emit the sample MCP config template. Path placeholders are
	// left as {{ NEXUS_BIN_DIR }} / {{ DATA_DIR }} so the install
	// script in NEX-286 can swap them with concrete values; operator
	// running `nexus init` standalone can hand-edit.
	mcpPath := filepath.Join(resolved, "sample.mcp.json")
	if err := writeMCPSample(mcpPath); err != nil {
		fmt.Fprintf(os.Stderr, "init: write sample.mcp.json: %v\n", err)
		return 1
	}
	step("✓ sample.mcp.json written to " + mcpPath)

	// Final output: quiet emits the bare token; verbose prints the
	// boxed token + next-steps guidance.
	if *quiet {
		fmt.Fprintln(out, tok)
		return 0
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "╭─ Operator admin token (Bearer-prefix for Authorization header) ─╮")
	fmt.Fprintf(out, "│ %s\n", tok)
	fmt.Fprintln(out, "╰─────────────────────────────────────────────────────────────────╯")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Next steps:")
	fmt.Fprintf(out, "  1. nexus serve --data-dir %s\n", resolved)
	fmt.Fprintln(out, "  2. nexus credential set ...     # provider creds (anthropic / openai / jira / imap)")
	fmt.Fprintln(out, "  3. open the dashboard URL printed by `nexus serve`, log in with the token above")
	return 0
}

// defaultDataDir returns the platform-appropriate default for the
// nexus state directory. ~/.nexus on Unix-likes, %LOCALAPPDATA%/nexus
// on Windows. Falls back to empty string when neither is resolvable —
// caller surfaces that as a hard error rather than guessing.
func defaultDataDir() string {
	// Prefer LOCALAPPDATA on Windows (set by the OS); HOME elsewhere.
	if lad := os.Getenv("LOCALAPPDATA"); lad != "" {
		return filepath.Join(lad, "nexus")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".nexus")
}

// isAlreadyExistsErr tries to recognise the "this row is already there"
// flavour of error so init's re-run path stays idempotent. The ledger
// returns a wrapped sqlite3 constraint error for the duplicate-project
// case; substring match is fragile but the alternative requires the
// ledger to expose a sentinel, which is more invasive scope than this
// ticket warrants.
func isAlreadyExistsErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return contains(s, "already exists") || contains(s, "UNIQUE constraint") || contains(s, "duplicate")
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// sampleMCP is the MCP config template emitted by `nexus init`. The
// install script in NEX-286 swaps the {{ ... }} placeholders for
// concrete paths; operators running init standalone hand-edit.
var sampleMCP = map[string]any{
	"_comment": "nexus sample MCP config. Replace {{ NEXUS_BIN_DIR }} and {{ DATA_DIR }} with absolute paths for your install. Drop into a project's .mcp.json to expose nexus MCPs to Claude Code.",
	"mcpServers": map[string]any{
		"nexus-comms": map[string]any{
			"command": "{{ NEXUS_BIN_DIR }}/nexus-comms-mcp",
			"args":    []string{"-keyfile", "{{ DATA_DIR }}/keyfiles/keel.keyfile.json"},
		},
		"nexus-jira": map[string]any{
			"command": "{{ NEXUS_BIN_DIR }}/nexus-jira-mcp",
			"args":    []string{"-keyfile", "{{ DATA_DIR }}/keyfiles/keel.keyfile.json"},
		},
		"nexus-imap": map[string]any{
			"command": "{{ NEXUS_BIN_DIR }}/nexus-imap-mcp",
			"args":    []string{"-keyfile", "{{ DATA_DIR }}/keyfiles/keel.keyfile.json"},
		},
	},
}

func writeMCPSample(path string) error {
	buf, err := json.MarshalIndent(sampleMCP, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	return os.WriteFile(path, buf, 0o644)
}

// Sentinel returned by some init sub-steps so callers can distinguish
// "this failed in a way we should report" from "this was already done"
// without depending on string matching everywhere. Unused today but
// reserved so the API surface doesn't churn when --force / --reset
// land in follow-up.
var errAlreadyInitialised = errors.New("init: already initialised")

var _ = errAlreadyInitialised
