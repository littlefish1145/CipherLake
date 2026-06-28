package events

import "sync/atomic"

// Metrics holds Prometheus-style counters for event delivery.
type Metrics struct {
	DeadLetterTotal          atomic.Int64 // nexus_deadletter_total
	EventDeliveryFailedTotal atomic.Int64 // nexus_event_delivery_failed_total
	EventDeliverySuccessTotal atomic.Int64 // nexus_event_delivery_total{status="success"}
	EventDeliveryRetryTotal  atomic.Int64 // nexus_event_delivery_total{status="retry"}
	EventDroppedTotal        atomic.Int64 // nexus_event_dropped_total (events dropped due to full queues)
}

// NewMetrics creates a new Metrics instance.
func NewMetrics() *Metrics {
	return &Metrics{}
}

// IncDeadLetter increments the dead letter counter.
func (m *Metrics) IncDeadLetter() {
	m.DeadLetterTotal.Add(1)
}

// IncDeliveryFailed increments the delivery failed counter.
func (m *Metrics) IncDeliveryFailed() {
	m.EventDeliveryFailedTotal.Add(1)
}

// IncDeliverySuccess increments the successful delivery counter.
func (m *Metrics) IncDeliverySuccess() {
	m.EventDeliverySuccessTotal.Add(1)
}

// IncDeliveryRetry increments the retry delivery counter.
func (m *Metrics) IncDeliveryRetry() {
	m.EventDeliveryRetryTotal.Add(1)
}

// IncDropped increments the dropped event counter.
func (m *Metrics) IncDropped() {
	m.EventDroppedTotal.Add(1)
}

// GetDeadLetterTotal returns the dead letter counter value.
func (m *Metrics) GetDeadLetterTotal() int64 {
	return m.DeadLetterTotal.Load()
}

// GetDeliveryFailedTotal returns the delivery failed counter value.
func (m *Metrics) GetDeliveryFailedTotal() int64 {
	return m.EventDeliveryFailedTotal.Load()
}

// GetDeliverySuccessTotal returns the successful delivery counter value.
func (m *Metrics) GetDeliverySuccessTotal() int64 {
	return m.EventDeliverySuccessTotal.Load()
}

// GetDeliveryRetryTotal returns the retry delivery counter value.
func (m *Metrics) GetDeliveryRetryTotal() int64 {
	return m.EventDeliveryRetryTotal.Load()
}

// GetDroppedTotal returns the dropped event counter value.
func (m *Metrics) GetDroppedTotal() int64 {
	return m.EventDroppedTotal.Load()
}
