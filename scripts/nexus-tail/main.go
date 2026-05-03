// Command nexus-tail prints new chat_messages rows as they land in
// the nexus.db SQLite store. Used for the F2.6 smoke and any other
// situation where a UI doesn't exist (or isn't running) and the
// operator wants real-time visibility into routing + replies.
//
// Usage:
//
//	nexus-tail -db <path-to-nexus.db>
//
// The tail polls every 250ms with a since-id cursor and prints any
// new rows. Format mirrors the shape forge asked for at #9754:
// from, content, reply_to. This is the minimum needed to verify Lock
// 2 routing (recipient was correct) + reply continuity (replies
// thread to the right parent).
//
// Output one row per message:
//
//	#42 [@forge] reply_to=#41 :: ack — running checks
//
// Standalone — no nexus binary needed; reads the SQLite directly.
// SQLite handles concurrent reads alongside the nexus process's
// writes (WAL mode by default in nexus.db).

package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
)

func main() {
	dbPath := flag.String("db", "./data/nexus.db", "path to nexus.db")
	intervalMs := flag.Int("interval", 250, "poll interval in ms")
	startFrom := flag.Int64("from", 0, "start from msg_id (default 0 = print everything)")
	flag.Parse()

	db, err := sql.Open("sqlite3", *dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		fmt.Fprintln(os.Stderr, "ping:", err)
		os.Exit(1)
	}

	cursor := *startFrom
	tick := time.NewTicker(time.Duration(*intervalMs) * time.Millisecond)
	defer tick.Stop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	fmt.Fprintf(os.Stderr, "tailing %s (cursor=%d, %dms poll). Ctrl+C to stop.\n",
		*dbPath, cursor, *intervalMs)

	for {
		select {
		case <-sig:
			return
		case <-tick.C:
			cursor = pollOnce(db, cursor)
		}
	}
}

// pollOnce reads any chat_messages rows with id > cursor, prints them,
// and returns the new high-water cursor. Errors are logged and the
// cursor stays put (so a transient DB-locked moment doesn't drop rows).
func pollOnce(db *sql.DB, cursor int64) int64 {
	rows, err := db.Query(`
		SELECT id, from_agent, content, reply_to, created_at
		FROM chat_messages
		WHERE id > ?
		ORDER BY id ASC`, cursor)
	if err != nil {
		fmt.Fprintln(os.Stderr, "query:", err)
		return cursor
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var from string
		var content string
		var replyTo sql.NullInt64
		var createdAt string
		if err := rows.Scan(&id, &from, &content, &replyTo, &createdAt); err != nil {
			fmt.Fprintln(os.Stderr, "scan:", err)
			continue
		}
		// One-line content preview — keep multi-line bodies on one
		// line so each message is visually one row in the tail. Strip
		// CRs to handle Windows-line-ending content.
		preview := strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", " | "), "\n", " | ")

		replyStr := ""
		if replyTo.Valid && replyTo.Int64 > 0 {
			replyStr = fmt.Sprintf(" reply_to=#%d", replyTo.Int64)
		}
		fmt.Printf("#%d [@%s]%s :: %s\n", id, from, replyStr, preview)
		cursor = id
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "rows.Err:", err)
	}
	return cursor
}
