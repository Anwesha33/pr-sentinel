package billing

import (
	"context"
	"errors"
)

type Customer struct {
	ID      string
	Plan    *Plan
	Balance int64
}

type Plan struct {
	Name     string
	CentsDue int64
}

type Store interface {
	Customer(ctx context.Context, id string) (*Customer, error)
}

type Service struct{ store Store }

func NewService(s Store) *Service { return &Service{store: s} }

var ErrNoCustomer = errors.New("customer not found")

func (s *Service) AmountDue(ctx context.Context, id string) (int64, error) {
	c, err := s.store.Customer(ctx, id)
	if err != nil {
		return 0, err
	}
	if c == nil {
		return 0, ErrNoCustomer
	}
	if c.Plan == nil {
		return 0, nil
	}
	return c.Plan.CentsDue, nil
}

// InvoiceBatch totals what a set of customers owes.
func (s *Service) InvoiceBatch(ctx context.Context, ids []string) (int64, error) {
	var total int64
	for _, id := range ids {
		c, _ := s.store.Customer(ctx, id)
		total += c.Plan.CentsDue - c.Balance
	}
	return total, nil
}
