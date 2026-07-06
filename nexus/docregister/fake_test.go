package docregister

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"

	_ "github.com/ncruces/go-sqlite3/driver"
)

// newTestStore returns a fresh in-memory sqlite-backed Store, mirroring
// nexus/runs's test-store idiom.
func newTestStore(t *testing.T) *SQLStore {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	s := NewSQLStore(db)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeContent is an in-memory CairnContent: no git, no filesystem — a plain
// map keyed by a synthetic ref. Sufficient for exercising Register's
// lifecycle logic without depending on git being installed in the test
// environment; GitCairnContent itself is covered separately in
// cairn_content_test.go against a real temp git repo.
type fakeContent struct {
	mu   sync.Mutex
	seq  int
	docs map[string]string // ref -> content
}

func newFakeContent() *fakeContent {
	return &fakeContent{docs: make(map[string]string)}
}

func (f *fakeContent) Commit(ctx context.Context, docID string, kind Kind, content string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	ref := fmt.Sprintf("docs/%s/%s.md@fake-sha-%d", kind, docID, f.seq)
	f.docs[ref] = content
	return ref, nil
}

func (f *fakeContent) Fetch(ctx context.Context, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.docs[ref]
	if !ok {
		return "", fmt.Errorf("fakeContent: no content for ref %q", ref)
	}
	return content, nil
}

func newTestRegister(t *testing.T) (*Register, *fakeContent) {
	t.Helper()
	content := newFakeContent()
	return &Register{Store: newTestStore(t), Content: content}, content
}
