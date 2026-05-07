// Application-layer identity bootstrap subcommand for nexus.
//
// `nexus identity init [--data-dir DIR] [--force]` populates the
// nexus_identity row: a stable nexus_id UUID, an Ed25519 server
// keypair (used to decrypt aspect keyfiles), and an HMAC secret for
// signing session JWTs.
//
// Per agent-network/docs/2026-05-08-nexus-resident-personality-spec.md
// §3.3 + §14 part 1.
//
// Distinct from `nexus cert init`. That is transport-layer (TLS).
// This is application-layer (keyfile decryption + JWT signing). Both
// are required for full broker startup; both have their own init.
//
// Workflow:
//
//   1. Operator runs `nexus cert init` once per host (TLS).
//   2. Operator runs `nexus identity init` once per Nexus instance.
//      The nexus_id and server keypair persist for the lifetime of
//      this Nexus's database. Don't re-init unless you accept that
//      every existing keyfile will be invalidated.
//   3. Boot proceeds normally.
//
// --force regenerates everything. Use only when the existing identity
// is compromised or being deliberately retired. All keyfiles minted
// against the old identity stop validating immediately; all in-flight
// session JWTs fail signature; aspects must be re-minted.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nexus-cw/nexus/nexus/identity"
	"github.com/nexus-cw/nexus/nexus/storage"
)

// runIdentitySubcommand parses `identity <verb> [...]` from
// os.Args[2:] and dispatches. Returns process exit code; main() calls
// os.Exit on it.
func runIdentitySubcommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: nexus identity <init>")
		return 2
	}
	switch args[0] {
	case "init":
		return runIdentityInit(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown identity subcommand %q (expected: init)\n", args[0])
		return 2
	}
}

// runIdentityInit implements `identity init`. Returns exit code.
func runIdentityInit(args []string) int {
	fs := flag.NewFlagSet("identity init", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	force := fs.Bool("force", false, "regenerate identity even if one already exists (invalidates all keyfiles)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := storage.Open(ctx, *dataDir, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "identity init: open db: %v\n", err)
		return 1
	}
	defer db.Close()

	id, err := identity.Init(ctx, db, *force)
	if err != nil {
		if errors.Is(err, identity.ErrAlreadyInitialized) {
			fmt.Fprintln(os.Stderr, "identity init: already initialised")
			fmt.Fprintln(os.Stderr, "use --force to regenerate (warning: invalidates all existing keyfiles)")
			return 1
		}
		fmt.Fprintf(os.Stderr, "identity init: %v\n", err)
		return 1
	}

	if *force {
		fmt.Println("WARNING: regenerated identity. All existing keyfiles are now invalid.")
		fmt.Println("Re-mint every aspect that needs to keep working: nexus aspect mint <name> --out <path>")
		fmt.Println()
	}
	fmt.Printf("nexus_id: %s\n", id.NexusID)
	fmt.Printf("server pubkey (hex): %x\n", id.ServerPublicKey)
	fmt.Println()
	fmt.Println("Identity persisted. Subsequent boots load this row automatically.")
	fmt.Println("To inspect: SELECT nexus_id FROM nexus_identity WHERE id = 1;")
	return 0
}
