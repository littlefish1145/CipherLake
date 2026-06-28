package events

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Rule matching tests ---

func TestNotificationRule_Matches_EventType(t *testing.T) {
	rule := &NotificationRule{
		ID:     "rule1",
		Events: []string{"s3:ObjectCreated:*"},
	}

	tests := []struct {
		eventType string
		expected  bool
	}{
		{"s3:ObjectCreated:Put", true},
		{"s3:ObjectCreated:Post", true},
		{"s3:ObjectCreated:CompleteMultipartUpload", true},
		{"s3:ObjectRemoved:Delete", false},
		{"s3:ObjectAccessed:Get", false},
	}

	for _, tt := range tests {
		t.Run(tt.eventType, func(t *testing.T) {
			event := &Event{EventType: tt.eventType, Key: "test.txt"}
			assert.Equal(t, tt.expected, rule.Matches(event))
		})
	}
}

func TestNotificationRule_Matches_ExactEventType(t *testing.T) {
	rule := &NotificationRule{
		ID:     "rule2",
		Events: []string{"s3:ObjectRemoved:Delete"},
	}

	event := &Event{EventType: "s3:ObjectRemoved:Delete", Key: "test.txt"}
	assert.True(t, rule.Matches(event))

	event2 := &Event{EventType: "s3:ObjectRemoved:DeleteMarkerCreated", Key: "test.txt"}
	assert.False(t, rule.Matches(event2))
}

func TestNotificationRule_Matches_Prefix(t *testing.T) {
	rule := &NotificationRule{
		ID:     "rule3",
		Events: []string{"s3:ObjectCreated:*"},
		Prefix: "images/",
	}

	assert.True(t, rule.Matches(&Event{EventType: "s3:ObjectCreated:Put", Key: "images/photo.jpg"}))
	assert.False(t, rule.Matches(&Event{EventType: "s3:ObjectCreated:Put", Key: "docs/readme.txt"}))
}

func TestNotificationRule_Matches_Suffix(t *testing.T) {
	rule := &NotificationRule{
		ID:     "rule4",
		Events: []string{"s3:ObjectCreated:*"},
		Suffix: ".jpg",
	}

	assert.True(t, rule.Matches(&Event{EventType: "s3:ObjectCreated:Put", Key: "images/photo.jpg"}))
	assert.False(t, rule.Matches(&Event{EventType: "s3:ObjectCreated:Put", Key: "images/photo.png"}))
}

func TestNotificationRule_Matches_PrefixAndSuffix(t *testing.T) {
	rule := &NotificationRule{
		ID:     "rule5",
		Events: []string{"s3:ObjectCreated:*"},
		Prefix: "images/",
		Suffix: ".jpg",
	}

	assert.True(t, rule.Matches(&Event{EventType: "s3:ObjectCreated:Put", Key: "images/photo.jpg"}))
	assert.False(t, rule.Matches(&Event{EventType: "s3:ObjectCreated:Put", Key: "images/photo.png"}))
	assert.False(t, rule.Matches(&Event{EventType: "s3:ObjectCreated:Put", Key: "docs/photo.jpg"}))
}

func TestNotificationRule_Matches_MultipleEventTypes(t *testing.T) {
	rule := &NotificationRule{
		ID:     "rule6",
		Events: []string{"s3:ObjectCreated:*", "s3:ObjectRemoved:Delete"},
	}

	assert.True(t, rule.Matches(&Event{EventType: "s3:ObjectCreated:Put", Key: "test.txt"}))
	assert.True(t, rule.Matches(&Event{EventType: "s3:ObjectRemoved:Delete", Key: "test.txt"}))
	assert.False(t, rule.Matches(&Event{EventType: "s3:ObjectAccessed:Get", Key: "test.txt"}))
}

// --- Webhook delivery with signature verification tests ---

func TestWebhookSender_Send_Success(t *testing.T) {
	var receivedPayload []byte
	var receivedSig string
	var receivedDelivery string
	var receivedEventType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedSig = r.Header.Get("X-Nexus-Signature")
		receivedDelivery = r.Header.Get("X-Nexus-Delivery")
		receivedEventType = r.Header.Get("X-Nexus-Event-Type")
		receivedPayload, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	secret := "test-secret-key"
	event := &Event{
		EventID:   "evt-123",
		EventType: "s3:ObjectCreated:Put",
		Bucket:    "mybucket",
		Key:       "test.txt",
		ETag:      "abc123",
		Size:      1024,
	}

	sender := NewWebhookSender(5 * time.Second)
	sender.ssrfBypass = true // bypass SSRF for localhost test server
	dest := DestinationConfig{
		Type: "webhook",
		URL:  server.URL,
		Signing: &SigningConfig{
			Algorithm: "hmac-sha256",
			Secret:    secret,
		},
	}

	result := sender.Send(event, dest)
	assert.True(t, result.Success)
	assert.NotEmpty(t, result.DeliveryID)
	assert.Equal(t, "s3:ObjectCreated:Put", receivedEventType)
	assert.NotEmpty(t, receivedDelivery)

	// Verify signature
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(receivedPayload)
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	assert.Equal(t, expectedSig, receivedSig)

	// Verify payload can be unmarshaled
	var decoded Event
	err := json.Unmarshal(receivedPayload, &decoded)
	require.NoError(t, err)
	assert.Equal(t, "evt-123", decoded.EventID)
	assert.Equal(t, "mybucket", decoded.Bucket)
}

func TestVerifySignature(t *testing.T) {
	payload := []byte(`{"event_id":"test"}`)
	secret := "my-secret"

	sig := signPayload(payload, secret)
	assert.True(t, VerifySignature(payload, secret, sig))
	assert.False(t, VerifySignature(payload, "wrong-secret", sig))
	assert.False(t, VerifySignature(payload, secret, "badsignature"))
}

func TestWebhookSender_Send_5xxRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	sender := NewWebhookSender(5 * time.Second)
	sender.ssrfBypass = true // bypass SSRF for localhost test server
	dest := DestinationConfig{Type: "webhook", URL: server.URL}
	event := &Event{EventType: "s3:ObjectCreated:Put", Key: "test.txt"}

	result := sender.Send(event, dest)
	assert.False(t, result.Success)
	assert.True(t, result.Retryable)
}

func TestWebhookSender_Send_4xxNonRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	sender := NewWebhookSender(5 * time.Second)
	sender.ssrfBypass = true // bypass SSRF for localhost test server
	dest := DestinationConfig{Type: "webhook", URL: server.URL}
	event := &Event{EventType: "s3:ObjectCreated:Put", Key: "test.txt"}

	result := sender.Send(event, dest)
	assert.False(t, result.Success)
	assert.False(t, result.Retryable)
}

func TestWebhookSender_Send_429Retryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	sender := NewWebhookSender(5 * time.Second)
	sender.ssrfBypass = true // bypass SSRF for localhost test server
	dest := DestinationConfig{Type: "webhook", URL: server.URL}
	event := &Event{EventType: "s3:ObjectCreated:Put", Key: "test.txt"}

	result := sender.Send(event, dest)
	assert.False(t, result.Success)
	assert.True(t, result.Retryable)
}

// --- Event publishing and subscription tests ---

func TestEventBus_PublishAndSubscribe(t *testing.T) {
	var received atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	bus := NewEventBus(4, 5*time.Second, t.TempDir(), 3, 100)
	bus.Start()
	bus.SetSSRFBypass(true) // bypass SSRF for localhost test server
	defer bus.Stop()

	rule := &NotificationRule{
		ID:     "test-rule",
		Events: []string{"s3:ObjectCreated:*"},
		Destination: DestinationConfig{
			Type: "webhook",
			URL:  server.URL,
		},
	}

	err := bus.Subscribe("mybucket", rule)
	require.NoError(t, err)

	bus.Publish(context.Background(), &Event{
		EventType: "s3:ObjectCreated:Put",
		Bucket:    "mybucket",
		Key:       "test.txt",
	})

	// Wait for async delivery
	assert.Eventually(t, func() bool {
		return received.Load() == 1
	}, 2*time.Second, 50*time.Millisecond)
}

func TestEventBus_Unsubscribe(t *testing.T) {
	var received atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	bus := NewEventBus(4, 5*time.Second, t.TempDir(), 3, 100)
	bus.Start()
	bus.SetSSRFBypass(true) // bypass SSRF for localhost test server
	defer bus.Stop()

	rule := &NotificationRule{
		ID:     "test-rule",
		Events: []string{"s3:ObjectCreated:*"},
		Destination: DestinationConfig{
			Type: "webhook",
			URL:  server.URL,
		},
	}

	bus.Subscribe("mybucket", rule)
	bus.Unsubscribe("mybucket", "test-rule")

	bus.Publish(context.Background(), &Event{
		EventType: "s3:ObjectCreated:Put",
		Bucket:    "mybucket",
		Key:       "test.txt",
	})

	// Wait a bit and check no delivery happened
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int32(0), received.Load())
}

func TestEventBus_NoMatchingSubscribers(t *testing.T) {
	var received atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	bus := NewEventBus(4, 5*time.Second, t.TempDir(), 3, 100)
	bus.Start()
	bus.SetSSRFBypass(true) // bypass SSRF for localhost test server
	defer bus.Stop()

	rule := &NotificationRule{
		ID:     "test-rule",
		Events: []string{"s3:ObjectCreated:*"},
		Destination: DestinationConfig{
			Type: "webhook",
			URL:  server.URL,
		},
	}

	bus.Subscribe("otherbucket", rule)

	bus.Publish(context.Background(), &Event{
		EventType: "s3:ObjectCreated:Put",
		Bucket:    "mybucket",
		Key:       "test.txt",
	})

	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int32(0), received.Load())
}

// --- Dead letter queue tests ---

func TestDeadLetterQueue_WriteToDisk(t *testing.T) {
	dir := t.TempDir()
	metrics := NewMetrics()
	dlq := NewDeadLetterQueue(dir, 0, 1000, metrics) // maxRetries=0 so it goes to disk immediately

	event := &Event{
		EventID:   "evt-dlq",
		EventType: "s3:ObjectCreated:Put",
		Bucket:    "mybucket",
		Key:       "test.txt",
	}

	rule := &NotificationRule{
		ID:     "rule-dlq",
		Events: []string{"s3:ObjectCreated:*"},
		Destination: DestinationConfig{
			Type: "webhook",
			URL:  "http://localhost:9999/unreachable",
		},
	}

	dlq.Enqueue(event, rule, 5, "max retries exceeded")

	// Check that a file was written
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, files, 1)

	// Verify file content
	data, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	require.NoError(t, err)

	var entry DeadLetterEntry
	err = json.Unmarshal(data, &entry)
	require.NoError(t, err)
	assert.Equal(t, "evt-dlq", entry.Event.EventID)
	assert.Equal(t, 5, entry.Attempts)
}

func TestDeadLetterQueue_RetryThenDeadLetter(t *testing.T) {
	dir := t.TempDir()
	metrics := NewMetrics()

	callCount := int32(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&callCount, 1)
		if count <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	dlq := NewDeadLetterQueue(dir, 3, 100, metrics)
	sender := NewWebhookSender(5 * time.Second)
	sender.ssrfBypass = true // bypass SSRF for localhost test server
	dlq.Start(sender)
	defer dlq.Stop()

	event := &Event{
		EventID:   "evt-retry",
		EventType: "s3:ObjectCreated:Put",
		Bucket:    "mybucket",
		Key:       "test.txt",
	}

	rule := &NotificationRule{
		ID:     "rule-retry",
		Events: []string{"s3:ObjectCreated:*"},
		Destination: DestinationConfig{
			Type: "webhook",
			URL:  server.URL,
		},
	}

	// Enqueue with 0 attempts (first failure)
	dlq.Enqueue(event, rule, 0, "initial failure")

	// Wait for retries to succeed
	assert.Eventually(t, func() bool {
		return atomic.LoadInt32(&callCount) >= 3
	}, 5*time.Second, 100*time.Millisecond)
}

// --- Key-based ordering guarantee tests ---

func TestEventBus_KeyBasedOrdering(t *testing.T) {
	var mu sync.Mutex
	var order []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event Event
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &event)
		mu.Lock()
		order = append(order, event.Key+"-"+event.EventID)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	bus := NewEventBus(4, 5*time.Second, t.TempDir(), 3, 100)
	bus.Start()
	bus.SetSSRFBypass(true) // bypass SSRF for localhost test server
	defer bus.Stop()

	rule := &NotificationRule{
		ID:     "ordering-rule",
		Events: []string{"s3:ObjectCreated:*"},
		Destination: DestinationConfig{
			Type: "webhook",
			URL:  server.URL,
		},
	}

	bus.Subscribe("mybucket", rule)

	// Publish multiple events for the same key
	key := "same-key.txt"
	for i := 0; i < 5; i++ {
		bus.Publish(context.Background(), &Event{
			EventID:   fmt.Sprintf("evt-%d", i),
			EventType: "s3:ObjectCreated:Put",
			Bucket:    "mybucket",
			Key:       key,
		})
	}

	// Wait for all deliveries
	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 5
	}, 3*time.Second, 50*time.Millisecond)

	// Verify order is preserved for same key
	mu.Lock()
	defer mu.Unlock()
	for i, o := range order {
		expected := fmt.Sprintf("%s-evt-%d", key, i)
		assert.Equal(t, expected, o, "events for same key should be delivered in order")
	}
}

func TestEventBus_DifferentKeysCanProcessConcurrently(t *testing.T) {
	var processed sync.Map

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event Event
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &event)
		processed.Store(event.Key, true)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	bus := NewEventBus(16, 5*time.Second, t.TempDir(), 3, 100)
	bus.Start()
	bus.SetSSRFBypass(true) // bypass SSRF for localhost test server
	defer bus.Stop()

	rule := &NotificationRule{
		ID:     "concurrent-rule",
		Events: []string{"s3:ObjectCreated:*"},
		Destination: DestinationConfig{
			Type: "webhook",
			URL:  server.URL,
		},
	}

	bus.Subscribe("mybucket", rule)

	// Publish events for different keys
	for i := 0; i < 10; i++ {
		bus.Publish(context.Background(), &Event{
			EventID:   fmt.Sprintf("evt-%d", i),
			EventType: "s3:ObjectCreated:Put",
			Bucket:    "mybucket",
			Key:       fmt.Sprintf("key-%d.txt", i),
		})
	}

	// Wait for all deliveries
	assert.Eventually(t, func() bool {
		count := 0
		processed.Range(func(_, _ interface{}) bool {
			count++
			return true
		})
		return count == 10
	}, 3*time.Second, 50*time.Millisecond)
}

// --- Metrics tests ---

func TestMetrics_Increment(t *testing.T) {
	m := NewMetrics()

	m.IncDeadLetter()
	m.IncDeadLetter()
	m.IncDeliveryFailed()
	m.IncDeliverySuccess()
	m.IncDeliverySuccess()
	m.IncDeliverySuccess()
	m.IncDeliveryRetry()

	assert.Equal(t, int64(2), m.GetDeadLetterTotal())
	assert.Equal(t, int64(1), m.GetDeliveryFailedTotal())
	assert.Equal(t, int64(3), m.GetDeliverySuccessTotal())
	assert.Equal(t, int64(1), m.GetDeliveryRetryTotal())
}

// --- matchEventTypePattern tests ---

func TestMatchEventTypePattern(t *testing.T) {
	assert.True(t, matchEventTypePattern("s3:ObjectCreated:*", "s3:ObjectCreated:Put"))
	assert.True(t, matchEventTypePattern("s3:ObjectCreated:*", "s3:ObjectCreated:Post"))
	assert.True(t, matchEventTypePattern("s3:ObjectCreated:Put", "s3:ObjectCreated:Put"))
	assert.False(t, matchEventTypePattern("s3:ObjectCreated:Put", "s3:ObjectCreated:Post"))
	assert.False(t, matchEventTypePattern("s3:ObjectCreated:*", "s3:ObjectRemoved:Delete"))
}
