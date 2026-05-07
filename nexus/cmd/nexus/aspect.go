// Aspect lifecycle subcommands for nexus.
//
// `nexus aspect mint <name> [flags]` — first-mint or re-mint of an
// aspect's keyfile. Generates a fresh Ed25519 aspect keypair, encrypts
// the inner payload (containing the aspect privkey) with the Nexus's
// server pubkey via NaCl crypto_box_seal, and writes the resulting JSON
// keyfile to disk mode 0o600.
//
// Per agent-network/docs/2026-05-08-nexus-resident-personality-spec.md §9.1.
//
// Flow:
//
//   1. Load nexus_identity (must already be initialised — `nexus identity init`)
//   2. Look up the aspect:
//        - missing            → INSERT row with status=active, version=1
//        - active, connected  → reject unless --force (would supersede live)
//        - active, idle       → bump version + replace pubkey (re-mint)
//        - retired            → reject; resurrect first
//   3. Generate fresh aspect Ed25519 keypair
//   4. Mint the keyfile (encrypts privkey against server pubkey)
//   5. Write to --out path with mode 0o600
//   6. Print fingerprint + summary
//
// "Currently connected" detection is deferred to Part 4 (validation
// endpoint owns connection roster). v0.1 of mint trusts the operator
// to know whether the aspect is in flight; --force is a documented
// override either way.

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/nexus-cw/nexus/nexus/aspects"
	"github.com/nexus-cw/nexus/nexus/identity"
	"github.com/nexus-cw/nexus/nexus/storage"
)

// runAspectSubcommand parses `aspect <verb> [...]` from os.Args[2:] and
// dispatches.
func runAspectSubcommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: nexus aspect <mint>")
		return 2
	}
	switch args[0] {
	case "mint":
		return runAspectMint(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown aspect subcommand %q (expected: mint)\n", args[0])
		return 2
	}
}

// runAspectMint implements `aspect mint <name>`. Returns exit code.
func runAspectMint(args []string) int {
	fs := flag.NewFlagSet("aspect mint", flag.ContinueOnError)
	out := fs.String("out", "", "path to write the keyfile JSON (required)")
	force := fs.Bool("force", false, "re-mint even if a current keyfile may be in use (invalidates the previous one)")
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	provider := fs.String("provider", "", "AI provider for new aspect rows (e.g. claude-api, claude-code). Ignored on re-mint.")
	model := fs.String("model", "", "model name for new aspect rows (e.g. claude-opus-4-7). Ignored on re-mint.")
	nexusURL := fs.String("nexus-url", "", "wss:// URL agentfunnel uses to dial this Nexus (required)")
	// Positional <name> must come first: `mint <name> [flags]`. Standard
	// CLI shape; avoids the ambiguity of a permissive parser stealing a
	// space-separated flag value (e.g. `--out plumb` would otherwise be
	// read as name=plumb).
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		fmt.Fprintln(os.Stderr, "usage: nexus aspect mint <name> --out <path> --nexus-url wss://...")
		return 2
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "aspect mint: unexpected extra args: %v\n", fs.Args())
		return 2
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "aspect mint: --out is required")
		return 2
	}
	if *nexusURL == "" {
		fmt.Fprintln(os.Stderr, "aspect mint: --nexus-url is required (the wss:// URL agentfunnel will dial)")
		return 2
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := storage.Open(ctx, *dataDir, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect mint: open db: %v\n", err)
		return 1
	}
	defer db.Close()

	id, err := identity.Load(ctx, db)
	if err != nil {
		if errors.Is(err, identity.ErrNotInitialized) {
			fmt.Fprintln(os.Stderr, "aspect mint: nexus identity not initialised")
			fmt.Fprintln(os.Stderr, "run: nexus identity init")
			return 1
		}
		fmt.Fprintf(os.Stderr, "aspect mint: load identity: %v\n", err)
		return 1
	}

	store := aspects.NewSQLStore(db)
	existing, err := store.Get(ctx, name)
	switch {
	case errors.Is(err, aspects.ErrNotFound):
		// First mint — handled below after we generate the keypair.
	case err != nil:
		fmt.Fprintf(os.Stderr, "aspect mint: lookup %q: %v\n", name, err)
		return 1
	default:
		// Existing row — validate state.
		if existing.Status == aspects.StatusRetired {
			fmt.Fprintf(os.Stderr, "aspect mint: %q is retired; resurrect it first (deferred Part 8)\n", name)
			return 1
		}
		// "currently connected" check is Part 4 territory (validation
		// endpoint owns the connection roster). v0.1 takes the operator
		// at their word — re-mint always proceeds. The warning is
		// informational so an operator who didn't realise the row
		// existed sees that the previous keyfile is now invalid.
		// --force is reserved for future use (it will gate the
		// connected-aspect supersede flow once Part 4 lands); today it
		// only suppresses this warning.
		if !*force {
			fmt.Fprintf(os.Stderr, "WARNING: %q already exists at version %d. Re-minting now — previous keyfile is permanently invalid after this bump.\n",
				name, existing.CurrentKeyfileVersion)
			fmt.Fprintln(os.Stderr, "Pass --force to suppress this notice in scripted use.")
		}
	}

	aspectPub, aspectPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect mint: generate aspect keypair: %v\n", err)
		return 1
	}

	// Persist the new pubkey + version atomically. INSERT for first
	// mint, BumpKeyfileVersion for re-mint.
	var version int64
	if existing == nil {
		row := aspects.Aspect{
			Name:         name,
			AspectPubkey: aspectPub,
			Provider:     *provider,
			Model:        *model,
		}
		if err := store.Insert(ctx, row); err != nil {
			fmt.Fprintf(os.Stderr, "aspect mint: insert aspects row: %v\n", err)
			return 1
		}
		version = 1
	} else {
		v, err := store.BumpKeyfileVersion(ctx, name, aspectPub)
		if err != nil {
			fmt.Fprintf(os.Stderr, "aspect mint: bump version: %v\n", err)
			return 1
		}
		version = v
	}

	// Mint the keyfile (encrypts privkey against server pubkey).
	now := time.Now().UTC()
	kf, fingerprint, err := aspects.Mint(aspects.MintInput{
		AspectName:     name,
		KeyfileVersion: version,
		AspectPrivkey:  aspectPriv,
		ServerPubkey:   id.ServerPublicKey,
		NexusID:        id.NexusID,
		NexusURL:       *nexusURL,
		MintedAt:       now,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect mint: mint keyfile: %v\n", err)
		return 1
	}

	body, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect mint: marshal keyfile: %v\n", err)
		return 1
	}

	if err := writeKeyfile(*out, body); err != nil {
		fmt.Fprintf(os.Stderr, "aspect mint: write %s: %v\n", *out, err)
		return 1
	}

	fmt.Printf("aspect: %s\n", name)
	fmt.Printf("keyfile_version: %d\n", version)
	fmt.Printf("fingerprint (sha256 of sealed payload): %s\n", fingerprint)
	fmt.Printf("nexus_id: %s\n", id.NexusID)
	fmt.Printf("written: %s (mode 0600)\n", *out)
	fmt.Println()
	fmt.Println("Distribute this file like an SSH private key.")
	fmt.Println("Re-minting bumps the version and invalidates this file.")
	return 0
}

// writeKeyfile writes the JSON body to path with mode 0o600 atomically:
// create a sibling temp file with mode 0o600, write+sync, chmod (defends
// against umask masking off bits at create time), then os.Rename onto
// the destination. The destination either exists at full content + final
// mode, or doesn't exist — never observable mid-write at a permissive
// mode.
//
// On Windows os.Rename across an existing target works since Go 1.5;
// NTFS ACLs are the real access control there, the 0o600 is best-effort.
// Operators distributing keyfiles on Windows should pair this with
// directory ACLs.
func writeKeyfile(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".keyfile-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if runtime.GOOS != "windows" {
		// Chmod before write so the file is never observable at a wider
		// mode than 0o600 (CreateTemp uses 0o600 already on Unix, but
		// be explicit — the umask doesn't apply to Chmod).
		if err := os.Chmod(tmpPath, 0o600); err != nil {
			_ = tmp.Close()
			cleanup()
			return fmt.Errorf("chmod temp: %w", err)
		}
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
