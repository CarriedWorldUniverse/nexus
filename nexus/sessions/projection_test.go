package sessions

import (
	"context"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

func openProj(t *testing.T) *Projection {
	t.Helper()
	db, err := storage.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db)
}

func TestWriteEntry(t *testing.T) {
	p := openProj(t)
	ctx := context.Background()
	err := p.WriteEntry(ctx, Entry{
		Aspect:    "harrow",
		SessionID: "sess-1",
		EntryID:   "entry-1",
		ParentID:  "",
		EntryKind: "turn.user",
		EntryTS:   "2026-04-24T00:00:00Z",
		Payload:   map[string]any{"text": "hi"},
	})
	if err != nil {
		t.Fatalf("WriteEntry: %v", err)
	}
	n, err := p.Count(ctx, "harrow", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Count = %d, want 1", n)
	}
}

func TestWriteEntryIdempotent(t *testing.T) {
	p := openProj(t)
	ctx := context.Background()
	e := Entry{
		Aspect:    "wren",
		SessionID: "sess-1",
		EntryID:   "entry-dup",
		EntryKind: "turn.user",
		EntryTS:   "2026-04-24T00:00:00Z",
		Payload:   map[string]any{"text": "x"},
	}
	for i := 0; i < 3; i++ {
		if err := p.WriteEntry(ctx, e); err != nil {
			t.Fatalf("WriteEntry iteration %d: %v", i, err)
		}
	}
	n, _ := p.Count(ctx, "wren", "sess-1")
	if n != 1 {
		t.Errorf("Count after 3 writes = %d, want 1 (UNIQUE constraint)", n)
	}
}

func TestWriteEntryValidation(t *testing.T) {
	p := openProj(t)
	ctx := context.Background()

	cases := map[string]Entry{
		"empty aspect":  {SessionID: "s", EntryID: "e", EntryKind: "turn.user"},
		"empty entryID": {Aspect: "a", SessionID: "s", EntryKind: "turn.user"},
		"empty kind":    {Aspect: "a", SessionID: "s", EntryID: "e"},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			if err := p.WriteEntry(ctx, e); err == nil {
				t.Error("expected error for missing required field")
			}
		})
	}
}

func TestWriteEntryNilPayloadOK(t *testing.T) {
	p := openProj(t)
	ctx := context.Background()
	err := p.WriteEntry(ctx, Entry{
		Aspect:    "a",
		SessionID: "s",
		EntryID:   "no-payload",
		EntryKind: "turn.user",
		EntryTS:   "2026-04-24T00:00:00Z",
		// Payload: nil
	})
	if err != nil {
		t.Errorf("WriteEntry with nil payload: %v", err)
	}
}
