// check-knowledge dumps row counts + a sample from knowledge in the
// current nexus.db. Diligence helper.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

func main() {
	dataDir := flag.String("data-dir", "", "data dir (or NEXUS_DATA_DIR)")
	flag.Parse()
	if *dataDir == "" {
		*dataDir = os.Getenv("NEXUS_DATA_DIR")
	}
	if *dataDir == "" {
		log.Fatal("--data-dir or NEXUS_DATA_DIR required")
	}
	ctx := context.Background()
	db, err := storage.Open(ctx, *dataDir, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	var total int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM knowledge").Scan(&total); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("total rows: %d\n\n", total)

	rs, err := db.QueryContext(ctx,
		`SELECT id, from_agent, topic, length(content), shared, created_at
		 FROM knowledge ORDER BY id ASC`)
	if err != nil {
		log.Fatal(err)
	}
	defer rs.Close()
	for rs.Next() {
		var id, bytes int64
		var fromAgent, topic, createdAt string
		var shared int
		if err := rs.Scan(&id, &fromAgent, &topic, &bytes, &shared, &createdAt); err != nil {
			log.Fatal(err)
		}
		if len(topic) > 50 {
			topic = topic[:50] + "…"
		}
		fmt.Printf("id=%-4d from=%-10s shared=%d bytes=%-6d topic=%q\n",
			id, fromAgent, shared, bytes, topic)
	}
}
