// Migration subcommand: bring on-disk aspect homes into the DB.
//
// `nexus migrate personality-from-disk --aspect-dir <path>` per spec §12.
//
// For each subdir of <aspect-dir> that contains aspect.json:
//   - Parse aspect.json for name, provider, model, capabilities, metadata
//   - Read NEXUS.md (or CLAUDE.md as fallback for back-compat), SOUL.md, PRIMER.md
//   - Insert into `aspects` table (status=active, version=1, placeholder pubkey)
//   - Insert into `aspect_personalities` table
//   - Operator MUST run `nexus aspect mint <name>` afterwards to get a
//     real keypair — until then the aspect's pubkey is a placeholder
//     and no keyfile can validate against it.
//
// Idempotent: if an aspect row already exists, skip unless --overwrite.
// One-shot: aspect.json files stay on disk; this is a one-way ETL.

package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/nexus-cw/nexus/nexus/aspects"
	"github.com/nexus-cw/nexus/shared/schemas"
)

// runMigrateSubcommand parses `migrate <verb> ...`.
func runMigrateSubcommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: nexus migrate <personality-from-disk>")
		return 2
	}
	switch args[0] {
	case "personality-from-disk":
		return runMigratePersonalityFromDisk(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown migrate subcommand %q\n", args[0])
		return 2
	}
}

// runMigratePersonalityFromDisk implements the spec §12 walk.
func runMigratePersonalityFromDisk(args []string) int {
	fs := flag.NewFlagSet("migrate personality-from-disk", flag.ContinueOnError)
	aspectDir := fs.String("aspect-dir", "", "directory containing per-aspect homes (each with aspect.json)")
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	overwrite := fs.Bool("overwrite", false, "overwrite existing aspects/personality rows in the DB")
	dryRun := fs.Bool("dry-run", false, "report what would be migrated without writing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *aspectDir == "" {
		fmt.Fprintln(os.Stderr, "migrate: --aspect-dir required")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	homes, err := scanAspectDir(*aspectDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: scan %s: %v\n", *aspectDir, err)
		return 1
	}
	if len(homes) == 0 {
		fmt.Fprintf(os.Stderr, "migrate: no aspect homes found under %s (looking for aspect.json subdirs)\n", *aspectDir)
		return 1
	}

	if *dryRun {
		fmt.Println("DRY RUN — no DB writes")
	}

	store, db, code := openStore(ctx, *dataDir)
	if code != 0 {
		return code
	}
	defer db.Close()

	var (
		migrated int
		skipped  int
		errored  int
	)

	for _, home := range homes {
		summary, err := migrateOne(ctx, store, home, *overwrite, *dryRun, logger)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", home.name, err)
			errored++
			continue
		}
		switch summary.action {
		case "inserted":
			fmt.Printf("  ✓ %s: inserted (provider=%s, model=%s, personality_bytes=%d)\n",
				summary.name, summary.provider, summary.model, summary.personalityBytes)
			migrated++
		case "overwritten":
			fmt.Printf("  ↻ %s: overwritten (provider=%s, model=%s, personality_bytes=%d)\n",
				summary.name, summary.provider, summary.model, summary.personalityBytes)
			migrated++
		case "skipped":
			fmt.Printf("  · %s: already exists, skipping (use --overwrite to replace)\n", summary.name)
			skipped++
		}
	}

	fmt.Println()
	fmt.Printf("migrated=%d  skipped=%d  errored=%d\n", migrated, skipped, errored)
	if migrated > 0 && !*dryRun {
		fmt.Println()
		fmt.Println("⚠ Each migrated aspect has a PLACEHOLDER pubkey — no keyfile validates yet.")
		fmt.Println("Run `nexus aspect mint <name> --out <path> --nexus-url ...` for each to generate a working keyfile.")
	}
	if errored > 0 {
		return 1
	}
	return 0
}

type migrateSummary struct {
	name             string
	action           string // "inserted" | "overwritten" | "skipped"
	provider         string
	model            string
	personalityBytes int
}

// aspectHomeInfo captures the on-disk aspect.json + the path to its
// home dir. Used by migrateOne to read sibling md files.
type aspectHomeInfo struct {
	name string
	path string
	cfg  schemas.AspectConfig
}

// scanAspectDir walks aspectDir's first level, looking for subdirs
// with aspect.json. Returns one info per matching subdir.
func scanAspectDir(aspectDir string) ([]aspectHomeInfo, error) {
	entries, err := os.ReadDir(aspectDir)
	if err != nil {
		return nil, err
	}
	var out []aspectHomeInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		home := filepath.Join(aspectDir, e.Name())
		cfgPath := filepath.Join(home, "aspect.json")
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			// No aspect.json → not a migratable home. Silently skip.
			continue
		}
		var cfg schemas.AspectConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("%s: parse aspect.json: %w", e.Name(), err)
		}
		if cfg.Name == "" {
			return nil, fmt.Errorf("%s: aspect.json missing name", e.Name())
		}
		out = append(out, aspectHomeInfo{name: cfg.Name, path: home, cfg: cfg})
	}
	return out, nil
}

// migrateOne handles a single aspect home: insert (or overwrite) the
// aspects row + personality row.
func migrateOne(ctx context.Context, store *aspects.SQLStore, home aspectHomeInfo, overwrite, dryRun bool, log *slog.Logger) (*migrateSummary, error) {
	model := pickModelFromConfig(home.cfg)

	// NEXUS.md preferred; CLAUDE.md is the legacy back-compat name
	// (spec §10 — one-shot rename in transit).
	nexusMD := readMDFile(home.path, "NEXUS.md", "CLAUDE.md")
	soulMD := readMDFile(home.path, "SOUL.md")
	primerMD := readMDFile(home.path, "PRIMER.md")

	personalityBytes := len(nexusMD) + len(soulMD) + len(primerMD)

	// Detect existing.
	existing, err := store.Get(ctx, home.name)
	exists := err == nil
	if err != nil && !errors.Is(err, aspects.ErrNotFound) {
		return nil, fmt.Errorf("lookup: %w", err)
	}
	if exists && !overwrite {
		return &migrateSummary{name: home.name, action: "skipped"}, nil
	}

	if dryRun {
		action := "inserted"
		if exists {
			action = "overwritten"
		}
		return &migrateSummary{
			name: home.name, action: action,
			provider: home.cfg.Provider, model: model,
			personalityBytes: personalityBytes,
		}, nil
	}

	// Capabilities + metadata serialise as JSON; the aspects table
	// stores them as opaque strings.
	capsJSON, err := json.Marshal(home.cfg.Capabilities)
	if err != nil {
		return nil, fmt.Errorf("marshal capabilities: %w", err)
	}
	var metaJSON []byte
	if home.cfg.Metadata != nil {
		if metaJSON, err = json.Marshal(home.cfg.Metadata); err != nil {
			return nil, fmt.Errorf("marshal metadata: %w", err)
		}
	}

	// Placeholder pubkey — operator runs `aspect mint` after migrate
	// to generate a real keypair (the mint path bumps version, replaces
	// pubkey atomically). Random bytes rather than zeros so a
	// hypothetical "is this aspect minted yet?" check can compare
	// against a stable sentinel.
	placeholder := make([]byte, 32)
	if _, err := rand.Read(placeholder); err != nil {
		return nil, fmt.Errorf("generate placeholder pubkey: %w", err)
	}

	row := aspects.Aspect{
		Name:         home.name,
		Status:       aspects.StatusActive,
		AspectPubkey: placeholder,
		Provider:     home.cfg.Provider,
		Model:        model,
		Capabilities: string(capsJSON),
		Metadata:     string(metaJSON),
	}

	action := "inserted"
	if exists {
		// Overwrite: replace the row's content fields (provider/model/
		// caps/metadata/personality), but preserve the keyfile bits
		// (version + pubkey) untouched. Operator's intent for
		// --overwrite is "re-import from disk", not "rotate the
		// keyfile" — that's what `aspect mint` is for. Without this
		// the placeholder pubkey above would clobber a real one and
		// silently break a working keyfile (no version bump means the
		// validator's revocation check passes; the key-mismatch check
		// fails on every connection).
		row.CurrentKeyfileVersion = existing.CurrentKeyfileVersion
		row.AspectPubkey = existing.AspectPubkey
		if err := store.Update(ctx, row); err != nil {
			return nil, fmt.Errorf("update aspect: %w", err)
		}
		action = "overwritten"
	} else {
		row.CurrentKeyfileVersion = 1
		if err := store.Insert(ctx, row); err != nil {
			return nil, fmt.Errorf("insert aspect: %w", err)
		}
	}

	if personalityBytes > 0 {
		if err := store.PersonalitySet(ctx, aspects.Personality{
			AspectName: home.name,
			NexusMD:    nexusMD,
			SoulMD:     soulMD,
			PrimerMD:   primerMD,
		}); err != nil {
			return nil, fmt.Errorf("set personality: %w", err)
		}
	}

	return &migrateSummary{
		name: home.name, action: action,
		provider: home.cfg.Provider, model: model,
		personalityBytes: personalityBytes,
	}, nil
}

// readMDFile tries each filename in order under home, returning the
// first that exists. Returns "" if none are present (allowed — not
// every aspect has every file).
func readMDFile(home string, names ...string) string {
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(home, n))
		if err == nil {
			return string(raw)
		}
	}
	return ""
}

// pickModelFromConfig pulls the model string from aspect.json's
// provider_config. Mirrors the runtime/cmd/aspect logic; default
// is empty (operator can fix via UPDATE or re-migrate with a fixed
// aspect.json).
func pickModelFromConfig(cfg schemas.AspectConfig) string {
	if cfg.ProviderConfig != nil {
		if m, ok := cfg.ProviderConfig["model"].(string); ok && m != "" {
			return m
		}
	}
	return ""
}
