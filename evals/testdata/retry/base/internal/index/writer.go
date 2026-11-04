package index

import (
	"context"
	"database/sql"
)

type Writer struct{ db *sql.DB }

type Batch struct{ Rows []Row }

// Commit writes one batch in a single transaction.
// It returns the database error unchanged.
func (w *Writer) Commit(ctx context.Context, b Batch) error {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, r := range b.Rows {
		if _, err := tx.ExecContext(ctx, insertSQL, r.Key, r.Val); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
