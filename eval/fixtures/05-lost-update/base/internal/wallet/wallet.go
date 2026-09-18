package wallet

import (
	"context"
	"database/sql"
)

type Wallet struct{ db *sql.DB }

func New(db *sql.DB) *Wallet { return &Wallet{db: db} }

func (w *Wallet) Balance(ctx context.Context, userID int64) (int64, error) {
	var cents int64
	err := w.db.QueryRowContext(ctx,
		`SELECT balance_cents FROM wallets WHERE user_id = $1`, userID).Scan(&cents)
	return cents, err
}
