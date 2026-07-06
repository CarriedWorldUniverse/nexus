package docregister

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Store is the register's lifecycle index: metadata + status + approvals.
// Content (the MD body) is NOT here — see CairnContent. Mirrors nexus/runs's
// Store idiom (a narrow interface + a sqlite-backed implementation) rather
// than nexus/workgraph's ledger adapter — see README.md for why a dedicated
// store, not a ledger issue-kind, is the leaner fit here.
type Store interface {
	Migrate(ctx context.Context) error

	// Create inserts a new document row. d.ID, d.CreatedAt, d.UpdatedAt are
	// set by the caller (Register.CreateDoc) before Create is invoked.
	Create(ctx context.Context, d Document) error

	// Get fetches one document by id, including its approvals. Returns
	// ErrNotFound if id doesn't exist.
	Get(ctx context.Context, id string) (Document, error)

	// List returns documents matching filter, newest-first.
	List(ctx context.Context, filter ListFilter) ([]Document, error)

	// UpdateStatus sets status (and updated_at) on id. Returns ErrNotFound
	// if id doesn't exist.
	UpdateStatus(ctx context.Context, id string, status Status, at time.Time) error

	// SetCairnRef updates cairn_ref and bumps version on id (used by
	// ApproveWithChanges after committing a new cairn version of the MD).
	SetCairnRef(ctx context.Context, id string, cairnRef string, version int, at time.Time) error

	// AddApproval appends an approval record to id's history.
	AddApproval(ctx context.Context, id string, a Approval) error
}

// SQLStore is the sqlite-backed Store, mirroring nexus/runs.SQLStore.
type SQLStore struct{ DB *sql.DB }

func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{DB: db} }

func (s *SQLStore) Migrate(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS docregister_docs (
			id            TEXT PRIMARY KEY,
			kind          TEXT NOT NULL,
			title         TEXT NOT NULL,
			version       INTEGER NOT NULL DEFAULT 1,
			status        TEXT NOT NULL,
			work_item_id  TEXT NOT NULL,
			cairn_ref     TEXT NOT NULL DEFAULT '',
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_docregister_docs_kind ON docregister_docs(kind);
		CREATE INDEX IF NOT EXISTS idx_docregister_docs_status ON docregister_docs(status);
		CREATE INDEX IF NOT EXISTS idx_docregister_docs_work_item ON docregister_docs(work_item_id);

		CREATE TABLE IF NOT EXISTS docregister_approvals (
			doc_id    TEXT NOT NULL,
			by        TEXT NOT NULL,
			verdict   TEXT NOT NULL,
			comments  TEXT NOT NULL DEFAULT '',
			at        INTEGER NOT NULL,
			seq       INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_docregister_approvals_doc ON docregister_approvals(doc_id, seq);
	`)
	if err != nil {
		return fmt.Errorf("docregister.Migrate: %w", err)
	}
	return nil
}

func (s *SQLStore) Create(ctx context.Context, d Document) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO docregister_docs (id, kind, title, version, status, work_item_id, cairn_ref, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, string(d.Kind), d.Title, d.Version, string(d.Status), d.WorkItemID, d.CairnRef,
		d.CreatedAt.UnixMilli(), d.UpdatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("docregister.Create: %w", err)
	}
	return nil
}

func (s *SQLStore) Get(ctx context.Context, id string) (Document, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT id, kind, title, version, status, work_item_id, cairn_ref, created_at, updated_at
		FROM docregister_docs WHERE id = ?`, id)
	d, err := scanDoc(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return Document{}, ErrNotFound
		}
		return Document{}, fmt.Errorf("docregister.Get: %w", err)
	}
	approvals, err := s.approvals(ctx, id)
	if err != nil {
		return Document{}, fmt.Errorf("docregister.Get: %w", err)
	}
	d.Approvals = approvals
	return d, nil
}

func (s *SQLStore) approvals(ctx context.Context, id string) ([]Approval, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT by, verdict, comments, at FROM docregister_approvals
		WHERE doc_id = ? ORDER BY seq ASC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var a Approval
		var verdict string
		var atMs int64
		if err := rows.Scan(&a.By, &verdict, &a.Comments, &atMs); err != nil {
			return nil, err
		}
		a.Verdict = Verdict(verdict)
		a.At = time.UnixMilli(atMs)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *SQLStore) List(ctx context.Context, filter ListFilter) ([]Document, error) {
	query := `SELECT id, kind, title, version, status, work_item_id, cairn_ref, created_at, updated_at
		FROM docregister_docs WHERE 1=1`
	var args []any
	if filter.Kind != "" {
		query += " AND kind = ?"
		args = append(args, string(filter.Kind))
	}
	if filter.Status != "" {
		query += " AND status = ?"
		args = append(args, string(filter.Status))
	}
	if filter.Stream != "" {
		query += " AND work_item_id = ?"
		args = append(args, filter.Stream)
	}
	query += " ORDER BY created_at DESC"

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("docregister.List: %w", err)
	}
	defer rows.Close()
	var out []Document
	for rows.Next() {
		d, err := scanDoc(rows)
		if err != nil {
			return nil, fmt.Errorf("docregister.List: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("docregister.List: %w", err)
	}
	// Approvals aren't needed for the list surface (kept cheap); callers
	// that need them call Get.
	return out, nil
}

func (s *SQLStore) UpdateStatus(ctx context.Context, id string, status Status, at time.Time) error {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE docregister_docs SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), at.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("docregister.UpdateStatus: %w", err)
	}
	return checkRowsAffected(res, "docregister.UpdateStatus")
}

func (s *SQLStore) SetCairnRef(ctx context.Context, id string, cairnRef string, version int, at time.Time) error {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE docregister_docs SET cairn_ref = ?, version = ?, updated_at = ? WHERE id = ?`,
		cairnRef, version, at.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("docregister.SetCairnRef: %w", err)
	}
	return checkRowsAffected(res, "docregister.SetCairnRef")
}

func (s *SQLStore) AddApproval(ctx context.Context, id string, a Approval) error {
	var seq int
	if err := s.DB.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(seq), 0) + 1 FROM docregister_approvals WHERE doc_id = ?`, id).Scan(&seq); err != nil {
		return fmt.Errorf("docregister.AddApproval: next seq: %w", err)
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO docregister_approvals (doc_id, by, verdict, comments, at, seq)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, a.By, string(a.Verdict), a.Comments, a.At.UnixMilli(), seq)
	if err != nil {
		return fmt.Errorf("docregister.AddApproval: %w", err)
	}
	return nil
}

func checkRowsAffected(res sql.Result, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

type scanner interface{ Scan(...any) error }

func scanDoc(sc scanner) (Document, error) {
	var d Document
	var kind, status string
	var createdMs, updatedMs int64
	if err := sc.Scan(&d.ID, &kind, &d.Title, &d.Version, &status, &d.WorkItemID, &d.CairnRef,
		&createdMs, &updatedMs); err != nil {
		return Document{}, err
	}
	d.Kind = Kind(kind)
	d.Status = Status(status)
	d.CreatedAt = time.UnixMilli(createdMs)
	d.UpdatedAt = time.UnixMilli(updatedMs)
	return d, nil
}
