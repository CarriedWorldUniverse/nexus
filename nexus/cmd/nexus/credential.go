// Credential management subcommands for nexus.
//
//	nexus credential set    <name> --kind <kind> --bundle <json> [--mode ...] [--allowed-aspects ...] [--description ...]
//	nexus credential get    <name>                                              — print metadata (no secrets)
//	nexus credential list   [--kind <kind>]                                     — list metadata, optional kind filter
//	nexus credential delete <name>
//	nexus credential audit  <name> [--limit N]
//	nexus credential aspect-default <aspect> [--anthropic <name>] [--openai <name>] [--jira <name>] [--imap <name>]
//	nexus credential aspect-default <aspect>                                    — read current defaults
//
// All subcommands operate against the local nexus.db (resolved via
// --data-dir / NEXUS_DATA_DIR / ./data). The CLI is the operator-
// facing wrapper around the admin REST endpoints in
// nexus/broker/admin_credentials.go; both ride on the same store.
//
// Filed alongside NEX-76 (admin REST extension); NEX-78 (the CLI)
// ships in the same PR per chat #1019 — REST without CLI leaves the
// operator stuck on curl, so they land together.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/identity"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

// runCredentialSubcommand dispatches `nexus credential <verb> [...]`.
func runCredentialSubcommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: nexus credential <set|get|list|delete|audit|aspect-default>")
		return 2
	}
	switch args[0] {
	case "set":
		return runCredentialSet(args[1:])
	case "get":
		return runCredentialGet(args[1:])
	case "list":
		return runCredentialList(args[1:])
	case "delete":
		return runCredentialDelete(args[1:])
	case "audit":
		return runCredentialAudit(args[1:])
	case "aspect-default":
		return runCredentialAspectDefault(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown credential subcommand %q (expected: set, get, list, delete, audit, aspect-default)\n", args[0])
		return 2
	}
}

// openCredentialsStore is the shared init: opens nexus.db, loads the
// identity (for the data-encryption key), constructs a Store. Used by
// every credential subcommand that needs the live store.
func openCredentialsStore(ctx context.Context, dataDir string) (*credentials.Store, func(), int) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	db, err := storage.Open(ctx, dataDir, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "credential: open db: %v\n", err)
		return nil, nil, 1
	}
	id, err := identity.Load(ctx, db)
	if err != nil {
		db.Close()
		if errors.Is(err, identity.ErrNotInitialized) {
			fmt.Fprintln(os.Stderr, "credential: nexus identity not initialised")
			fmt.Fprintln(os.Stderr, "run: nexus identity init")
			return nil, nil, 1
		}
		fmt.Fprintf(os.Stderr, "credential: load identity: %v\n", err)
		return nil, nil, 1
	}
	store, err := credentials.NewStore(db, id.SessionSigningSecret)
	if err != nil {
		db.Close()
		fmt.Fprintf(os.Stderr, "credential: init store: %v\n", err)
		return nil, nil, 1
	}
	cleanup := func() { db.Close() }
	return store, cleanup, 0
}

// commonDataDirFlag adds the --data-dir flag every credential subcommand
// supports. Default falls through to NEXUS_DATA_DIR env, then ./data.
func commonDataDirFlag(fs *flag.FlagSet) *string {
	return fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
}

// runCredentialSet — `nexus credential set <name> --kind <kind> --bundle <json> ...`
//
// Bundle is a JSON object whose shape is kind-specific:
//
//	--kind=provider  --bundle='{"api_shape":"anthropic","base_url":"https://api.anthropic.com","key":"sk-..."}'
//	--kind=jira      --bundle='{"atlassian_email":"...","atlassian_token":"...","atlassian_subdomain":"..."}'
//	--kind=imap      --bundle='{"host":"...","port":993,"user":"...","password":"...","ssl":true}'
func runCredentialSet(args []string) int {
	fs := flag.NewFlagSet("credential set", flag.ContinueOnError)
	dataDir := commonDataDirFlag(fs)
	kind := fs.String("kind", "", "credential kind (provider|jira|imap)")
	bundleStr := fs.String("bundle", "", "credential bundle as JSON object (kind-specific shape)")
	mode := fs.String("mode", "", "access mode: proxy|fetch|both (default: proxy)")
	desc := fs.String("description", "", "human-readable description")
	allowed := fs.String("allowed-aspects", "*", "comma-separated aspect names, or '*' for all")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "credential set: <name> required")
		fs.Usage()
		return 2
	}
	name := fs.Arg(0)
	if *kind == "" {
		fmt.Fprintln(os.Stderr, "credential set: --kind required (provider|jira|imap)")
		return 2
	}
	if *bundleStr == "" {
		fmt.Fprintln(os.Stderr, "credential set: --bundle required (JSON object)")
		return 2
	}
	var bundle map[string]any
	if err := json.Unmarshal([]byte(*bundleStr), &bundle); err != nil {
		fmt.Fprintf(os.Stderr, "credential set: --bundle is not valid JSON: %v\n", err)
		return 2
	}
	credMode := credentials.Mode(*mode)
	if credMode == "" {
		credMode = credentials.ModeProxy
	}
	allowedList := splitCSV(*allowed)
	if len(allowedList) == 0 {
		allowedList = []string{"*"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, cleanup, rc := openCredentialsStore(ctx, *dataDir)
	if rc != 0 {
		return rc
	}
	defer cleanup()

	if err := store.Set(ctx, credentials.UpsertParams{
		Name:           name,
		Description:    *desc,
		Kind:           credentials.Kind(*kind),
		Bundle:         bundle,
		AllowedAspects: allowedList,
		Mode:           credMode,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "credential set: %v\n", err)
		return 1
	}
	c, err := store.Get(ctx, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "credential set: read-back: %v\n", err)
		return 1
	}
	return printMetadataJSON(c.ToMetadata())
}

func runCredentialGet(args []string) int {
	fs := flag.NewFlagSet("credential get", flag.ContinueOnError)
	dataDir := commonDataDirFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "credential get: <name> required")
		return 2
	}
	name := fs.Arg(0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, cleanup, rc := openCredentialsStore(ctx, *dataDir)
	if rc != 0 {
		return rc
	}
	defer cleanup()
	c, err := store.Get(ctx, name)
	if err != nil {
		if errors.Is(err, credentials.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "credential get: %q not found\n", name)
			return 1
		}
		fmt.Fprintf(os.Stderr, "credential get: %v\n", err)
		return 1
	}
	return printMetadataJSON(c.ToMetadata())
}

func runCredentialList(args []string) int {
	fs := flag.NewFlagSet("credential list", flag.ContinueOnError)
	dataDir := commonDataDirFlag(fs)
	kind := fs.String("kind", "", "filter to one kind (provider|jira|imap); empty = all")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, cleanup, rc := openCredentialsStore(ctx, *dataDir)
	if rc != 0 {
		return rc
	}
	defer cleanup()
	ms, err := store.List(ctx, credentials.Kind(*kind))
	if err != nil {
		fmt.Fprintf(os.Stderr, "credential list: %v\n", err)
		return 1
	}
	if ms == nil {
		ms = []credentials.Metadata{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{"credentials": ms}); err != nil {
		fmt.Fprintf(os.Stderr, "credential list: encode: %v\n", err)
		return 1
	}
	return 0
}

func runCredentialDelete(args []string) int {
	fs := flag.NewFlagSet("credential delete", flag.ContinueOnError)
	dataDir := commonDataDirFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "credential delete: <name> required")
		return 2
	}
	name := fs.Arg(0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, cleanup, rc := openCredentialsStore(ctx, *dataDir)
	if rc != 0 {
		return rc
	}
	defer cleanup()
	if err := store.Delete(ctx, name); err != nil {
		if errors.Is(err, credentials.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "credential delete: %q not found\n", name)
			return 1
		}
		fmt.Fprintf(os.Stderr, "credential delete: %v\n", err)
		return 1
	}
	fmt.Printf("deleted %q\n", name)
	return 0
}

func runCredentialAudit(args []string) int {
	fs := flag.NewFlagSet("credential audit", flag.ContinueOnError)
	dataDir := commonDataDirFlag(fs)
	limit := fs.Int("limit", 100, "max audit rows to return (1-1000)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "credential audit: <name> required")
		return 2
	}
	name := fs.Arg(0)
	if *limit < 1 || *limit > 1000 {
		fmt.Fprintf(os.Stderr, "credential audit: --limit must be 1..1000, got %d\n", *limit)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, cleanup, rc := openCredentialsStore(ctx, *dataDir)
	if rc != 0 {
		return rc
	}
	defer cleanup()
	rows, err := store.ListAudit(ctx, name, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "credential audit: %v\n", err)
		return 1
	}
	if rows == nil {
		rows = []credentials.AuditRow{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{"audit": rows}); err != nil {
		fmt.Fprintf(os.Stderr, "credential audit: encode: %v\n", err)
		return 1
	}
	return 0
}

// runCredentialAspectDefault reads or writes per-aspect default-
// credential columns:
//
//	nexus credential aspect-default forge                                   — read current defaults
//	nexus credential aspect-default forge --anthropic anth-prod             — set one
//	nexus credential aspect-default forge --jira "" --imap mail-default     — clear + set in one call
//
// Empty string clears a default (column → NULL); credential name sets it.
// Flags that aren't passed leave the column untouched.
func runCredentialAspectDefault(args []string) int {
	fs := flag.NewFlagSet("credential aspect-default", flag.ContinueOnError)
	dataDir := commonDataDirFlag(fs)
	// Each pointer-string lets us distinguish "not passed" (nil) from
	// "passed with empty value" (pointer to ""). flag.String can't do
	// that, so we use a custom Var that records presence.
	anth := newOptionalString(fs, "anthropic", "set anthropic-shape default credential (empty = clear)")
	oai := newOptionalString(fs, "openai", "set openai-shape default credential (empty = clear)")
	jira := newOptionalString(fs, "jira", "set jira default credential (empty = clear)")
	imap := newOptionalString(fs, "imap", "set imap default credential (empty = clear)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "credential aspect-default: <aspect> required")
		return 2
	}
	aspect := fs.Arg(0)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, cleanup, rc := openCredentialsStore(ctx, *dataDir)
	if rc != 0 {
		return rc
	}
	defer cleanup()

	// Apply each flag that was actually passed.
	type update struct {
		col   string
		value *string
	}
	for _, u := range []update{
		{"anthropic", anth.get()},
		{"openai", oai.get()},
		{"jira", jira.get()},
		{"imap", imap.get()},
	} {
		if u.value == nil {
			continue
		}
		if err := store.SetAspectDefault(ctx, aspect, u.col, *u.value); err != nil {
			fmt.Fprintf(os.Stderr, "credential aspect-default: %v\n", err)
			return 1
		}
	}

	// Print current state (read-back).
	ad, err := store.GetAspectDefaults(ctx, aspect)
	if err != nil {
		fmt.Fprintf(os.Stderr, "credential aspect-default: read-back: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(ad); err != nil {
		fmt.Fprintf(os.Stderr, "credential aspect-default: encode: %v\n", err)
		return 1
	}
	return 0
}

// -------------------------------------------------------------------
// helpers
// -------------------------------------------------------------------

func printMetadataJSON(m credentials.Metadata) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		fmt.Fprintf(os.Stderr, "encode metadata: %v\n", err)
		return 1
	}
	return 0
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// optionalString tracks "was the flag actually passed" alongside the
// value. Used for aspect-default updates so the operator can:
//
//   - clear via `--jira ""`     (empty string → "clear column")
//   - set   via `--jira name`   (string → "set to that credential")
//   - skip  by omitting `--jira`  (no change)
//
// flag.String can't tell skip from set-to-empty; this Var does.
type optionalString struct {
	set bool
	val string
}

func newOptionalString(fs *flag.FlagSet, name, usage string) *optionalString {
	o := &optionalString{}
	fs.Var(o, name, usage)
	return o
}

func (o *optionalString) String() string {
	if o == nil {
		return ""
	}
	return o.val
}

func (o *optionalString) Set(s string) error {
	o.set = true
	o.val = s
	return nil
}

func (o *optionalString) get() *string {
	if !o.set {
		return nil
	}
	v := o.val
	return &v
}

