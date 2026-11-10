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
	logger *slog.Logger
}

// New returns a Service that logs to logger.
func New(logger *slog.Logger) *Service { return &Service{logger: logger} }

var errDeclined = errors.New("card declined")

const (
	msgCaptured = "payment captured"
	keyOrder    = "order_id"
)

// Charge charges an order.
func (s *Service) Charge(ctx context.Context, orderID string, cents int64, attempt int) error {
	start := time.Now()
	if err := validate(ctx, s.logger, orderID, cents); err != nil {
		return err
	}
	if attempt > 3 {
		s.logger.LogAttrs(ctx, slog.LevelError, "payment failed",
			slog.String(keyOrder, orderID), slog.Int("attempt", attempt), slog.Any("err", errDeclined))
		return errDeclined
	}
	s.logger.Info(msgCaptured, keyOrder, orderID, "amount_cents", cents, "elapsed", time.Since(start), "currency", "EUR")
	return nil
}

func validate(ctx context.Context, logger *slog.Logger, orderID string, cents int64) error {
	if cents > 0 {
		return nil
	}
	logger.Log(ctx, slog.LevelWarn, "invalid amount", slog.Group("", keyOrder, orderID, "amount_cents", cents))
	return errors.New("invalid amount")
}
