// delete-knowledge removes a single (from_agent, topic) entry.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/CarriedWorldUniverse/nexus/nexus/knowledge"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

func main() {
	dataDir := flag.String("data-dir", "", "data dir (or NEXUS_DATA_DIR)")
	fromAgent := flag.String("from-agent", "", "exact from_agent value")
	topic := flag.String("topic", "", "exact topic value")
	flag.Parse()
	if *dataDir == "" {
		*dataDir = os.Getenv("NEXUS_DATA_DIR")
	}
	if *dataDir == "" || *fromAgent == "" || *topic == "" {
		log.Fatal("--data-dir, --from-agent, --topic all required")
	}
	ctx := context.Background()
	db, err := storage.Open(ctx, *dataDir, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	store := knowledge.New(db, nil)
	n, err := store.Delete(ctx, *fromAgent, *topic)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("deleted %d row(s) where from_agent=%q topic=%q\n", n, *fromAgent, *topic)
}
