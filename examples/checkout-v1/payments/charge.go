// Package payments is a made-up payment service used to show logbreak.
package payments

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Service charges orders.
type Service struct {
	log *slog.Logger
}

// New returns a Service that logs to log.
func New(log *slog.Logger) *Service { return &Service{log: log} }

var errDeclined = errors.New("card declined")

// Charge charges an order.
func (s *Service) Charge(ctx context.Context, orderID string, cents int64, attempt int) error {
	start := time.Now()
	if cents <= 0 {
		s.log.WarnContext(ctx, "invalid amount", "order_id", orderID, "amount_cents", cents)
		return errors.New("invalid amount")
	}
	if attempt > 3 {
		s.log.Error("payment failed", "order_id", orderID, "attempt", attempt, "err", errDeclined)
		return errDeclined
	}
	s.log.Info("payment captured",
		slog.String("order_id", orderID),
		slog.Int64("amount_cents", cents),
		slog.Duration("elapsed", time.Since(start)),
	)
	return nil
}

// Refund refunds an order.
func (s *Service) Refund(orderID string, cents int64, status int) {
	if status >= 500 {
		s.log.Error("refund provider error", "order_id", orderID, "status", status)
		return
	}
	s.log.Info("refund issued", "order_id", orderID, "amount_cents", cents)
}
