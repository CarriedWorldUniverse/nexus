// Aspect lifecycle subcommands: retire, resurrect, list, status.
//
// Per agent-network/docs/2026-05-08-nexus-resident-personality-spec.md §9.3 / §9.4.
//
// retire    — sets status='retired'. All keyfiles permanently dead.
//             aspect mint refuses (use resurrect first).
// resurrect — sets status='active' AND bumps keyfile_version. Old
//             keyfile (if any) is invalidated by the bump; operator
//             must re-mint to get a working keyfile.
// list      — table of all aspects: name, status, version, provider,
//             model, updated_at.
// status    — detail view for one aspect: same fields plus pubkey
//             fingerprint and absolute timestamps.

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

// runAspectRetire implements `aspect retire <name>`.
func runAspectRetire(args []string) int {
	fs := flag.NewFlagSet("aspect retire", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		fmt.Fprintln(os.Stderr, "usage: nexus aspect retire <name>")
		return 2
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, db, code := openStore(ctx, *dataDir)
	if code != 0 {
		return code
	}
	defer db.Close()

	// Surface the prior state so operator sees the transition rather
	// than a bare "ok".
	prior, err := store.Get(ctx, name)
	if err != nil {
		if errors.Is(err, aspects.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "aspect retire: %q does not exist\n", name)
			return 1
		}
		fmt.Fprintf(os.Stderr, "aspect retire: lookup: %v\n", err)
		return 1
	}
	if prior.Status == aspects.StatusRetired {
		fmt.Printf("aspect: %s\nstatus: already retired\n", name)
		return 0
	}

	if err := store.SetStatus(ctx, name, aspects.StatusRetired); err != nil {
		fmt.Fprintf(os.Stderr, "aspect retire: %v\n", err)
		return 1
	}
	fmt.Printf("aspect: %s\nstatus: %s → retired\n", name, prior.Status)
	fmt.Printf("keyfile_version: %d (now permanently dead)\n", prior.CurrentKeyfileVersion)
	fmt.Println()
	fmt.Println("All existing keyfiles for this aspect are now invalid.")
	fmt.Println("Use `nexus aspect resurrect <name>` to revive (bumps version + sets active).")
	return 0
}

// runAspectResurrect implements `aspect resurrect <name>`. Sets status
// back to active AND bumps the keyfile version so any keyfile that
// somehow survived retire (it shouldn't) is dead. Replaces aspect_pubkey
// with a placeholder; operator must re-mint immediately to get a
// working keyfile.
func runAspectResurrect(args []string) int {
	fs := flag.NewFlagSet("aspect resurrect", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		fmt.Fprintln(os.Stderr, "usage: nexus aspect resurrect <name>")
		return 2
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, db, code := openStore(ctx, *dataDir)
	if code != 0 {
		return code
	}
	defer db.Close()

	prior, err := store.Get(ctx, name)
	if err != nil {
		if errors.Is(err, aspects.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "aspect resurrect: %q does not exist\n", name)
			return 1
		}
		fmt.Fprintf(os.Stderr, "aspect resurrect: lookup: %v\n", err)
		return 1
	}
	if prior.Status != aspects.StatusRetired {
		fmt.Fprintf(os.Stderr, "aspect resurrect: %q is not retired (status=%q); nothing to do\n",
			name, prior.Status)
		return 1
	}

	// Atomic transition: status flip + version bump + placeholder
	// pubkey in a single transaction (Store.Resurrect). Without this,
	// a race window between SetStatus and BumpKeyfileVersion would
	// let an old keyfile briefly re-validate.
	placeholder := make([]byte, 32)
	if _, err := rand.Read(placeholder); err != nil {
		fmt.Fprintf(os.Stderr, "aspect resurrect: generate placeholder: %v\n", err)
		return 1
	}
	newVer, err := store.Resurrect(ctx, name, placeholder)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect resurrect: %v\n", err)
		return 1
	}

	fmt.Printf("aspect: %s\nstatus: retired → active\n", name)
	fmt.Printf("keyfile_version: %d → %d\n", prior.CurrentKeyfileVersion, newVer)
	fmt.Println()
	fmt.Println("⚠ Run `nexus aspect mint <name> --out <path> --nexus-url ...` NOW.")
	fmt.Println("Until then this aspect cannot validate (pubkey is a placeholder).")
	return 0
}

// runAspectList implements `aspect list`. Tabular output sorted by
// name (which is what aspects.Store.List returns).
func runAspectList(args []string) int {
	fs := flag.NewFlagSet("aspect list", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, db, code := openStore(ctx, *dataDir)
	if code != 0 {
		return code
	}
	defer db.Close()

	rows, err := store.List(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect list: %v\n", err)
		return 1
	}
	if len(rows) == 0 {
		fmt.Println("(no aspects registered)")
		return 0
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tVERSION\tPROVIDER\tMODEL\tUPDATED")
	for _, a := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n",
			a.Name, a.Status, a.CurrentKeyfileVersion,
			a.Provider, a.Model, a.UpdatedAt)
	}
	_ = tw.Flush()
	return 0
}

// runAspectStatus implements `aspect status <name>`. Detail view —
// same row as list, plus pubkey fingerprint and a hint pointing at
// keyfile lifecycle commands.
func runAspectStatus(args []string) int {
	fs := flag.NewFlagSet("aspect status", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		fmt.Fprintln(os.Stderr, "usage: nexus aspect status <name>")
		return 2
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, db, code := openStore(ctx, *dataDir)
	if code != 0 {
		return code
	}
	defer db.Close()

	a, err := store.Get(ctx, name)
	if err != nil {
		if errors.Is(err, aspects.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "aspect status: %q does not exist\n", name)
			return 1
		}
		fmt.Fprintf(os.Stderr, "aspect status: %v\n", err)
		return 1
	}

	fingerprint := "(empty)"
	if len(a.AspectPubkey) > 0 {
		sum := sha256.Sum256(a.AspectPubkey)
		fingerprint = hex.EncodeToString(sum[:8]) // first 16 hex chars; identifies without dumping the whole key
	}

	fmt.Printf("name:             %s\n", a.Name)
	fmt.Printf("status:           %s\n", a.Status)
	fmt.Printf("keyfile_version:  %d\n", a.CurrentKeyfileVersion)
	fmt.Printf("pubkey (sha256):  %s...\n", fingerprint)
	fmt.Printf("provider:         %s\n", a.Provider)
	fmt.Printf("model:            %s\n", a.Model)
	if a.Capabilities != "" {
		fmt.Printf("capabilities:     %s\n", a.Capabilities)
	}
	if a.Metadata != "" {
		fmt.Printf("metadata:         %s\n", a.Metadata)
	}
	fmt.Printf("created_at:       %s\n", a.CreatedAt)
	fmt.Printf("updated_at:       %s\n", a.UpdatedAt)

	// Personality summary (presence + version) without dumping content.
	if p, perr := store.PersonalityGet(ctx, name); perr == nil {
		fmt.Printf("personality:      version %d (updated %s)\n", p.Version, p.UpdatedAt)
	} else if errors.Is(perr, aspects.ErrNotFound) {
		fmt.Println("personality:      (not set — run `nexus personality edit " + name + "`)")
	} else {
		fmt.Printf("personality:      (error: %v)\n", perr)
	}

	return 0
}

// openStore opens nexus.db and returns a SQLStore + the underlying
// *sql.DB so the caller can defer Close. Returns exit code 1 on
// failure with an error message already printed.
func openStore(ctx context.Context, dataDir string) (*aspects.SQLStore, dbCloser, int) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	db, err := storage.Open(ctx, dataDir, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		return nil, dbCloser{}, 1
	}
	return aspects.NewSQLStore(db), dbCloser{db: db}, 0
}

// dbCloser wraps a *sql.DB so the caller can defer Close without
// importing database/sql at the call site. Tiny, but keeps the
// subcommand bodies focused on their own logic.
type dbCloser struct {
	db interface{ Close() error }
}

func (d dbCloser) Close() {
	if d.db != nil {
		_ = d.db.Close()
	}
}
