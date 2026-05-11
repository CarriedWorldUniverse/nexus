// migrate-knowledge copies the agent-network broker's `knowledge` rows
// into nexus.db's `knowledge` table. One-shot cutover utility.
//
// Source schema (agent-network/code/broker/comms.db):
//
//	knowledge(id, from_agent, topic, content, created_at, updated_at)
//
// Destination schema (nexus.db, see nexus/storage/schema.sql §knowledge):
//
//	knowledge(id, from_agent, topic, content, shared, created_at,
//	          updated_at, embedding, embed_model, embed_dim)
//	UNIQUE(from_agent, topic)
//
// The destination has a UNIQUE(from_agent, topic) constraint that the
// source lacks. We feed source rows in created_at ASC order so the
// upsert keeps the latest content for each (from_agent, topic) pair.
//
// Embedding columns are left NULL — the embedding pipeline is deferred
// per §2.8 of the registration spec; backfill will populate them when
// vector retrieval lands.
//
// Usage:
//
//	go run ./scripts/migrate-knowledge \
//	  --source C:\src\agent-network\code\broker\comms.db \
//	  --data-dir C:\src\nexus-cw\data \
//	  [--topic-glob "v3/*"] \
//	  [--from-agent keel] \
//	  [--dry-run] \
//	  [--shared]
//
// Filters compose with AND. With no filters, every row migrates.
//
//	--dry-run reports what would migrate without touching the destination.
//	--shared marks all migrated rows shared=1 (default 0).
//
// Exit codes:
//
//	0  migration complete (or dry-run finished)
//	1  source/destination open failed, query failed, write failed
//	2  flag / argument problem
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/CarriedWorldUniverse/nexus/nexus/knowledge"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

type sourceRow struct {
	ID        int64
	FromAgent string
	Topic     string
	Content   string
	CreatedAt string
	UpdatedAt string
}

func main() {
	source := flag.String("source", "", "path to agent-network comms.db (the source)")
	dataDir := flag.String("data-dir", "", "nexus data dir holding nexus.db (defaults to NEXUS_DATA_DIR)")
	topicGlob := flag.String("topic-glob", "", "optional LIKE-pattern filter on topic (use SQL '%' wildcards; e.g. 'v3/%')")
	fromAgent := flag.String("from-agent", "", "optional exact-match filter on from_agent")
	dryRun := flag.Bool("dry-run", false, "print what would migrate, write nothing")
	shared := flag.Bool("shared", false, "mark migrated rows shared=1 (default 0)")
	verbose := flag.Bool("verbose", false, "log every row instead of just summary")
	flag.Parse()

	if *source == "" {
		fmt.Fprintln(os.Stderr, "migrate-knowledge: --source required")
		os.Exit(2)
	}
	if *dataDir == "" {
		*dataDir = os.Getenv("NEXUS_DATA_DIR")
	}
	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "migrate-knowledge: --data-dir (or NEXUS_DATA_DIR env) required")
		os.Exit(2)
	}

	// Resolve to absolute paths so log lines are unambiguous when the
	// operator pastes them later.
	sourceAbs, err := filepath.Abs(*source)
	if err != nil {
		log.Fatalf("resolve source path: %v", err)
	}
	dataAbs, err := filepath.Abs(*dataDir)
	if err != nil {
		log.Fatalf("resolve data-dir: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	src, err := openSource(sourceAbs)
	if err != nil {
		log.Fatalf("open source %s: %v", sourceAbs, err)
	}
	defer src.Close()

	rows, err := queryRows(ctx, src, *fromAgent, *topicGlob)
	if err != nil {
		log.Fatalf("query source: %v", err)
	}
	fmt.Printf("source rows matched: %d  (source=%s)\n", len(rows), sourceAbs)
	if len(rows) == 0 {
		fmt.Println("nothing to migrate; exiting.")
		return
	}

	// Group by (from_agent, topic) so we can report the dedup picture
	// before any writes land. The Put-upsert handles dedup correctness
	// inside Bootstrap, but a pre-run summary helps operator decide
	// whether the result is what they expected.
	type key struct{ from, topic string }
	groups := make(map[key]int)
	for _, r := range rows {
		groups[key{r.FromAgent, r.Topic}]++
	}
	dupCount := 0
	for _, n := range groups {
		if n > 1 {
			dupCount++
		}
	}
	fmt.Printf("unique (from_agent, topic) targets: %d  (groups with >1 row: %d)\n",
		len(groups), dupCount)
	if dupCount > 0 {
		fmt.Println("  → dedup policy: feed rows in created_at ASC order; upsert keeps the latest content per pair.")
	}

	if *dryRun {
		fmt.Println("\n[dry-run] no writes performed.")
		if *verbose {
			printSample(rows, 10)
		}
		return
	}

	dst, err := storage.Open(ctx, dataAbs, nil)
	if err != nil {
		log.Fatalf("open destination data-dir %s: %v", dataAbs, err)
	}
	defer dst.Close()

	store := knowledge.New(dst, nil)
	migrated := 0
	skipped := 0
	failed := 0
	for _, r := range rows {
		if strings.TrimSpace(r.FromAgent) == "" || strings.TrimSpace(r.Topic) == "" || r.Content == "" {
			// knowledge.Put rejects empty fields. Source rows that
			// somehow have them shouldn't fail the whole migration —
			// skip and tally.
			skipped++
			if *verbose {
				fmt.Printf("  skip empty: id=%d from=%q topic=%q\n", r.ID, r.FromAgent, r.Topic)
			}
			continue
		}
		id, err := store.Put(ctx, r.FromAgent, r.Topic, r.Content, knowledge.PutOptions{Shared: *shared})
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  FAIL: src_id=%d from=%s topic=%q: %v\n", r.ID, r.FromAgent, r.Topic, err)
			continue
		}
		migrated++
		if *verbose {
			fmt.Printf("  ✓ src_id=%d → dst_id=%d  from=%s topic=%q (%d bytes)\n",
				r.ID, id, r.FromAgent, r.Topic, len(r.Content))
		}
	}

	fmt.Printf("\nmigration complete: %d written, %d skipped, %d failed.\n", migrated, skipped, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func openSource(path string) (*sql.DB, error) {
	// Read-only: agent-network may still be running. The shared cache
	// + immutable URI flags would be safer but modernc/sqlite's URI
	// support is partial; instead we just rely on SQLite's MVCC.
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("source path %s: %w", path, err)
	}
	// Read-only DSN flag; the ncruces driver honors the `mode=ro` query
	// arg. agent-network may still be running; SQLite's MVCC tolerates
	// a concurrent reader even on a WAL-mode source.
	dsn := "file:" + path + "?mode=ro"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping source: %w", err)
	}
	return db, nil
}

func queryRows(ctx context.Context, db *sql.DB, fromAgent, topicGlob string) ([]sourceRow, error) {
	q := `SELECT id, from_agent, topic, content,
	             COALESCE(created_at, '') as created_at,
	             COALESCE(updated_at, '') as updated_at
	      FROM knowledge`
	var clauses []string
	var args []any
	if fromAgent != "" {
		clauses = append(clauses, "from_agent = ?")
		args = append(args, fromAgent)
	}
	if topicGlob != "" {
		clauses = append(clauses, "topic LIKE ?")
		args = append(args, topicGlob)
	}
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	// Order ASC by created_at so the upsert keeps the latest content
	// per (from_agent, topic). Fallback to id ASC for rows with
	// identical created_at.
	q += " ORDER BY created_at ASC, id ASC"

	rs, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()

	var out []sourceRow
	for rs.Next() {
		var r sourceRow
		if err := rs.Scan(&r.ID, &r.FromAgent, &r.Topic, &r.Content, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func printSample(rows []sourceRow, n int) {
	if len(rows) < n {
		n = len(rows)
	}
	fmt.Printf("\nsample (first %d of %d):\n", n, len(rows))
	for _, r := range rows[:n] {
		preview := strings.ReplaceAll(r.Content, "\n", " ")
		if len(preview) > 80 {
			preview = preview[:80] + "…"
		}
		fmt.Printf("  src_id=%d  from=%-10s  topic=%-30s  %s\n",
			r.ID, r.FromAgent, truncate(r.Topic, 30), preview)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
