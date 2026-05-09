// Operator subcommand. Today this exposes inspection + reset of the
// operator_passkeys table — the shape of "manage devices" without
// committing to a CLI-driven WebAuthn ceremony (the browser handles
// that via /api/operator/register/{begin,finish}).
//
// Verbs:
//
//	nexus operator list                — show registered passkeys
//	nexus operator delete <id>         — remove one passkey by row id
//	nexus operator reset-passkey       — delete ALL passkeys (recovery
//	                                     path when operator loses every
//	                                     device they registered against)
//
// First-device registration ("bootstrap") is browser-driven: with the
// table empty, anyone with network reach can hit the /register
// endpoint without a prior operator JWT. This works because that's
// how the dashboard SPA's first-run flow ends up; no separate CLI
// PIN flow needed in v1.

package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/nexus-cw/nexus/nexus/operator"
	"github.com/nexus-cw/nexus/nexus/storage"
)

// runOperatorSubcommand parses `operator <verb> ...`.
func runOperatorSubcommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: nexus operator <list|delete|reset-passkey>")
		return 2
	}
	switch args[0] {
	case "list":
		return runOperatorList(args[1:])
	case "delete":
		return runOperatorDelete(args[1:])
	case "reset-passkey":
		return runOperatorResetPasskey(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown operator subcommand %q (expected: list, delete, reset-passkey)\n", args[0])
		return 2
	}
}

// runOperatorList prints registered passkeys in a tabular form.
// Helpful for the operator to confirm a registration landed and to
// pick a row id for delete.
func runOperatorList(args []string) int {
	fs := flag.NewFlagSet("operator list", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	store, cleanup, code := openOperatorStore(*dataDir)
	if code != 0 {
		return code
	}
	defer cleanup()

	rows, err := store.List(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		return 1
	}
	if len(rows) == 0 {
		fmt.Println("no passkeys registered")
		fmt.Println("(register one by opening the dashboard register page while the table is empty — bootstrap path)")
		return 0
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tLABEL\tSIGN_COUNT\tREGISTERED\tLAST_USED\tCRED_PREFIX")
	for _, p := range rows {
		credPrefix := base64.RawURLEncoding.EncodeToString(p.CredentialID)
		if len(credPrefix) > 16 {
			credPrefix = credPrefix[:16] + "…"
		}
		lastUsed := p.LastUsedAt
		if lastUsed == "" {
			lastUsed = "(never)"
		}
		fmt.Fprintf(tw, "%d\t%s\t%d\t%s\t%s\t%s\n",
			p.ID, p.Label, p.SignCount, p.RegisteredAt, lastUsed, credPrefix)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "flush: %v\n", err)
		return 1
	}
	return 0
}

// runOperatorDelete removes one passkey by row id. No confirmation —
// CLI subcommand is the confirmation. Use after `nexus operator list`
// to identify the id of the device to revoke (lost phone, retired
// laptop, etc.).
//
// IN-FLIGHT JWT CAVEAT: deleting a passkey row stops it from being
// usable in *future* logins. Any operator JWT minted before this
// deletion remains valid until its `exp` (1h max). To kill an active
// session, restart the broker — that drops every connection and
// invalidates any in-memory state. v1 limitation; tighter
// session-revocation lands later.
func runOperatorDelete(args []string) int {
	fs := flag.NewFlagSet("operator delete", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: nexus operator delete <id>")
		return 2
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintf(os.Stderr, "invalid id %q (must be a positive integer)\n", fs.Arg(0))
		return 2
	}

	store, cleanup, code := openOperatorStore(*dataDir)
	if code != 0 {
		return code
	}
	defer cleanup()

	n, err := store.Delete(context.Background(), id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "delete: %v\n", err)
		return 1
	}
	if n == 0 {
		fmt.Fprintf(os.Stderr, "no passkey with id=%d\n", id)
		return 1
	}
	fmt.Printf("deleted passkey id=%d\n", id)
	fmt.Println("note: any operator JWT minted before this deletion remains valid until exp (1h max).")
	fmt.Println("      restart the broker to drop active sessions immediately.")
	return 0
}

// runOperatorResetPasskey wipes every registered passkey. The
// recovery path the spec calls out: "if the operator loses all
// devices, `nexus operator reset-passkey` from a privileged shell
// on the Nexus host clears the table and re-runs registration."
//
// Requires --confirm to fire — the implicit-confirmation rule we use
// for delete-by-id doesn't extend here; clearing every passkey
// shouldn't be a typo away.
//
// IN-FLIGHT JWT CAVEAT: same as runOperatorDelete. Wiping the
// passkey table stops *future* logins immediately, but any operator
// JWT issued before reset remains valid until its exp (1h max). If
// reset-passkey is being run as a security response (suspected
// compromise), restart the broker to drop active sessions —
// otherwise an attacker holding an unexpired JWT keeps their
// connection.
func runOperatorResetPasskey(args []string) int {
	fs := flag.NewFlagSet("operator reset-passkey", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	confirm := fs.Bool("confirm", false, "required: confirms wiping ALL operator passkeys")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*confirm {
		fmt.Fprintln(os.Stderr, "reset-passkey wipes ALL registered operator passkeys.")
		fmt.Fprintln(os.Stderr, "Re-run with --confirm if that is what you want.")
		return 2
	}

	store, cleanup, code := openOperatorStore(*dataDir)
	if code != 0 {
		return code
	}
	defer cleanup()

	n, err := store.DeleteAll(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "reset: %v\n", err)
		return 1
	}
	fmt.Printf("removed %d passkey(s)\n", n)
	fmt.Println("first-device registration is unlocked again — open the dashboard register page to enrol a passkey")
	fmt.Println()
	fmt.Println("⚠ note: any operator JWT minted BEFORE this reset remains valid until its exp (1h max).")
	fmt.Println("        if you are running reset-passkey as a security response (suspected compromise),")
	fmt.Println("        restart the broker to drop active sessions immediately.")
	return 0
}

// openOperatorStore resolves the data dir, opens the DB, and returns
// the PasskeyStore + cleanup. Mirrors the pattern in admin.go +
// personality.go.
func openOperatorStore(dataDirFlag string) (*operator.PasskeyStore, func(), int) {
	dir := dataDirFlag
	if dir == "" {
		dir = os.Getenv("NEXUS_DATA_DIR")
	}
	if dir == "" {
		dir = "./data"
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "data dir %q does not exist; pass --data-dir or set NEXUS_DATA_DIR\n", dir)
		return nil, func() {}, 1
	}
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		return nil, func() {}, 1
	}
	cleanup := func() { _ = db.Close() }
	return operator.NewPasskeyStore(db), cleanup, 0
}
