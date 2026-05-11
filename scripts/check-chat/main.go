package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

func main() {
	dataDir := flag.String("data-dir", "", "data dir (or NEXUS_DATA_DIR)")
	limit := flag.Int("limit", 15, "rows")
	flag.Parse()
	if *dataDir == "" {
		*dataDir = os.Getenv("NEXUS_DATA_DIR")
	}
	ctx := context.Background()
	db, err := storage.Open(ctx, *dataDir, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	store := chat.NewSQLStore(db)
	rows, _, err := store.ListPage(ctx, 0, 0, *limit)
	if err != nil {
		log.Fatal(err)
	}
	for _, m := range rows {
		preview := m.Content
		if len(preview) > 100 {
			preview = preview[:100] + "…"
		}
		fmt.Printf("id=%-4d from=%-15s reply_to=%-4d %s\n", m.ID, m.From, m.ReplyTo, preview)
	}
}
