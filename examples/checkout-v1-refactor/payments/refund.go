package payments

// Refund refunds an order.
func (s *Service) Refund(orderID string, cents uint32, status int16) {
	if status >= 500 {
		s.logger.With(keyOrder, orderID).Error("refund provider error", "status", status)
		return
	}
	s.logger.Info("refund issued", keyOrder, orderID, "amount_cents", cents)
}
