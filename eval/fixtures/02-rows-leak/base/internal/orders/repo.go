package orders

import (
	"context"
	"database/sql"
)

type Repo struct{ db *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

type Order struct {
	ID     int64
	UserID int64
	Total  int64
	State  string
}

func (r *Repo) Get(ctx context.Context, id int64) (*Order, error) {
	var o Order
	err := r.db.QueryRowContext(ctx,
		`SELECT id, user_id, total, state FROM orders WHERE id = $1`, id).
		Scan(&o.ID, &o.UserID, &o.Total, &o.State)
	if err != nil {
		return nil, err
	}
	return &o, nil
}
