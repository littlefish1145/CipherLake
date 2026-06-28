package events

import "strings"

// NotificationRule defines a bucket-level event subscription rule.
type NotificationRule struct {
	ID          string
	Events      []string        // e.g., "s3:ObjectCreated:*", "s3:ObjectRemoved:Delete"
	Prefix      string
	Suffix      string
	Destination DestinationConfig
}

// DestinationConfig describes where matching events should be delivered.
type DestinationConfig struct {
	Type    string        // "webhook", "kafka", "nats", "amqp"
	URL     string
	Signing *SigningConfig
}

// SigningConfig holds HMAC signing parameters for webhook delivery.
type SigningConfig struct {
	Algorithm string // "hmac-sha256"
	Secret    string
}

// Matches checks whether an event matches this rule's filters (event type, prefix, suffix).
func (r *NotificationRule) Matches(event *Event) bool {
	if !r.matchEventType(event.EventType) {
		return false
	}
	if r.Prefix != "" && !strings.HasPrefix(event.Key, r.Prefix) {
		return false
	}
	if r.Suffix != "" && !strings.HasSuffix(event.Key, r.Suffix) {
		return false
	}
	return true
}

// matchEventType checks if the event type matches any of the rule's event patterns.
// Supports wildcard matching: "s3:ObjectCreated:*" matches "s3:ObjectCreated:Put", etc.
func (r *NotificationRule) matchEventType(eventType string) bool {
	for _, pattern := range r.Events {
		if matchEventTypePattern(pattern, eventType) {
			return true
		}
	}
	return false
}

// matchEventTypePattern matches a single event type pattern against an event type.
func matchEventTypePattern(pattern, eventType string) bool {
	if pattern == eventType {
		return true
	}
	if strings.HasSuffix(pattern, ":*") {
		prefix := pattern[:len(pattern)-1] // "s3:ObjectCreated:"
		return strings.HasPrefix(eventType, prefix)
	}
	return false
}
