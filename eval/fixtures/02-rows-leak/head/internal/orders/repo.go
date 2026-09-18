package orders

import (
	"context"
	"database/sql"
	"fmt"
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

// ListByState returns every order in a given state for a user.
func (r *Repo) ListByState(ctx context.Context, userID int64, state string) ([]Order, error) {
	query := fmt.Sprintf(
		`SELECT id, user_id, total, state FROM orders WHERE user_id = %d AND state = '%s'`,
		userID, state)
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}

	var out []Order
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.UserID, &o.Total, &o.State); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}
