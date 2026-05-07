// Personality editing subcommand.
//
// `nexus personality edit <name>` opens $EDITOR with the three-section
// edit blob (NEXUS / SOUL / PRIMER), parses the result on save, and
// writes via aspects.EditPersonality. Per spec §8.1.
//
// The post-edit broadcast (personality.refresh push frame for live
// remote aspects, in-process callback for embedded Frame) lives in
// Part 7c. v0.1 of this subcommand prints the new version and exits;
// the running broker picks up the change automatically because keel
// reads via funnel.SystemPromptFn (Part 6) and remote aspects pick up
// at next JWT re-validation cycle (default 1h, spec §6).

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/nexus-cw/nexus/nexus/aspects"
	"github.com/nexus-cw/nexus/nexus/storage"
)

// runPersonalitySubcommand parses `personality <verb> ...`.
func runPersonalitySubcommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: nexus personality <edit>")
		return 2
	}
	switch args[0] {
	case "edit":
		return runPersonalityEdit(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown personality subcommand %q (expected: edit)\n", args[0])
		return 2
	}
}

// runPersonalityEdit implements `personality edit <name>`. Opens $EDITOR
// with the current personality content (or empty if no row yet),
// parses on save, writes to DB.
func runPersonalityEdit(args []string) int {
	fs := flag.NewFlagSet("personality edit", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	editor := fs.String("editor", "", "editor command (falls back to EDITOR env, then platform default)")

	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		fmt.Fprintln(os.Stderr, "usage: nexus personality edit <name> [--data-dir DIR] [--editor CMD]")
		return 2
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "personality edit: unexpected extra args: %v\n", fs.Args())
		return 2
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	db, err := storage.Open(ctx, *dataDir, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "personality edit: open db: %v\n", err)
		return 1
	}
	defer db.Close()
	store := aspects.NewSQLStore(db)

	// Verify the aspect exists before opening the editor — operator
	// shouldn't waste a typed edit on a typo'd name.
	if _, err := store.Get(ctx, name); err != nil {
		if errors.Is(err, aspects.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "personality edit: aspect %q does not exist\n", name)
			fmt.Fprintln(os.Stderr, "mint it first: nexus aspect mint "+name+" --out <path> --nexus-url wss://...")
			return 1
		}
		fmt.Fprintf(os.Stderr, "personality edit: lookup %q: %v\n", name, err)
		return 1
	}

	// Seed editor with current content (or empty if no row yet).
	current, err := store.PersonalityGet(ctx, name)
	if err != nil && !errors.Is(err, aspects.ErrNotFound) {
		fmt.Fprintf(os.Stderr, "personality edit: read current: %v\n", err)
		return 1
	}
	seed := aspects.MarshalEditBlob(current)

	edited, err := openEditor(*editor, name, seed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "personality edit: editor: %v\n", err)
		return 1
	}

	nexusMD, soulMD, primerMD, err := aspects.UnmarshalEditBlob(edited)
	if err != nil {
		fmt.Fprintf(os.Stderr, "personality edit: parse edited blob: %v\n", err)
		fmt.Fprintln(os.Stderr, "(your changes were not saved; section headers must be intact and in order: NEXUS → SOUL → PRIMER)")
		return 1
	}

	// Detect "no semantic change" by round-tripping through unmarshal/
	// marshal before comparing. Catches the common false-positive
	// where vim/nano append a trailing newline on a save-without-edit:
	// raw string compare would treat that as a change and bump version
	// for nothing.
	canonical := aspects.MarshalEditBlob(&aspects.Personality{
		NexusMD: nexusMD, SoulMD: soulMD, PrimerMD: primerMD,
	})
	if canonical == seed {
		fmt.Fprintln(os.Stderr, "personality edit: no changes — bailing out")
		return 0
	}

	change, err := aspects.EditPersonality(ctx, store, name, nexusMD, soulMD, primerMD)
	if err != nil {
		fmt.Fprintf(os.Stderr, "personality edit: write: %v\n", err)
		return 1
	}

	fmt.Printf("aspect: %s\n", change.AspectName)
	fmt.Printf("version: %d → %d\n", change.OldVersion, change.NewVersion)
	fmt.Println()
	fmt.Println("Embedded Frame picks up the new prompt on the next deliberation turn (Part 6 SystemPromptFn).")
	fmt.Println("Remote aspects pick up at next JWT re-validation (default 1h, per spec §6).")
	fmt.Println("(Part 7c will add immediate WS push for connected aspects.)")
	return 0
}

// openEditor runs the operator's editor on a temp file pre-filled with
// `seed` and returns the edited content.
func openEditor(editorFlag, name, seed string) (string, error) {
	editor := editorFlag
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = defaultEditor()
	}
	if editor == "" {
		return "", errors.New("no editor configured (set $EDITOR or pass --editor)")
	}

	// Temp file in $TMPDIR with .md suffix so editors that pick syntax
	// by extension highlight markdown sensibly.
	dir := os.TempDir()
	pattern := "nexus-personality-" + sanitizeName(name) + "-*.md"
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // edit blob may contain prompt content; don't leave it on disk
	if _, err := tmp.WriteString(seed); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write seed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close seed: %w", err)
	}

	// On Windows the editor command might include arguments (e.g.
	// "code --wait"); split on whitespace. Operator with paths
	// containing spaces should pre-quote via $EDITOR=foo or use a
	// wrapper script.
	parts := strings.Fields(editor)
	parts = append(parts, tmpPath)
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("editor exited: %w", err)
	}

	out, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", fmt.Errorf("read edited: %w", err)
	}
	return string(out), nil
}

// defaultEditor returns a sensible fallback when neither --editor nor
// $EDITOR is set. notepad on Windows; vi elsewhere (POSIX guarantees it).
func defaultEditor() string {
	if runtime.GOOS == "windows" {
		return "notepad"
	}
	return "vi"
}

// sanitizeName strips characters that aren't safe in a filename so the
// temp file's pattern doesn't choke on aspect names with slashes or
// spaces. Only letters, digits, and a few separators allowed.
func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "aspect"
	}
	return b.String()
}

