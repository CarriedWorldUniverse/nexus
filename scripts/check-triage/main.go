// check-triage prints the most recent rows from inbox_triage for
// diligence verification. Run from repo root:
//
//	go run ./scripts/check-triage --data-dir <path>
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
	dataDir := flag.String("data-dir", "", "data dir holding nexus.db (defaults to NEXUS_DATA_DIR)")
	aspect := flag.String("aspect", "", "filter by aspect_name (default: all aspects)")
	limit := flag.Int("limit", 20, "max rows to print")
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
		log.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()

	store := chat.NewSQLTriageStore(db)
	var rows []chat.TriageDecision
	if *aspect != "" {
		rows, err = store.ListByAspect(ctx, *aspect, *limit)
		if err != nil {
			log.Fatalf("ListByAspect: %v", err)
		}
	} else {
		// No "ListAll" yet — query directly for diligence convenience.
		rs, err := db.QueryContext(ctx, `
			SELECT id, aspect_name, msg_id, turn_id, decision, reason, reply_msg_id, created_at
			FROM inbox_triage ORDER BY id DESC LIMIT ?
		`, *limit)
		if err != nil {
			log.Fatalf("query: %v", err)
		}
		defer rs.Close()
		for rs.Next() {
			var dec chat.TriageDecision
			var replyMsgID *int64
			var createdAt string
			if err := rs.Scan(&dec.ID, &dec.AspectName, &dec.MsgID, &dec.TurnID,
				&dec.Decision, &dec.Reason, &replyMsgID, &createdAt); err != nil {
				log.Fatalf("scan: %v", err)
			}
			if replyMsgID != nil {
				dec.ReplyMsgID = *replyMsgID
			}
			rows = append(rows, dec)
		}
	}

	fmt.Printf("%-4s %-12s %-8s %-8s %-30s %s\n", "id", "aspect", "msg_id", "decision", "reason", "turn_id")
	for _, r := range rows {
		reason := r.Reason
		if len(reason) > 28 {
			reason = reason[:28] + "…"
		}
		fmt.Printf("%-4d %-12s %-8d %-8s %-30s %s\n",
			r.ID, r.AspectName, r.MsgID, r.Decision, reason, r.TurnID)
	}
	fmt.Printf("\ntotal rows: %d\n", len(rows))
}
