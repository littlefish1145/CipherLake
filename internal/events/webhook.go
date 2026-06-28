package events

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// DeliveryResult represents the outcome of a webhook delivery attempt.
type DeliveryResult struct {
	DeliveryID string
	StatusCode int
	Retryable  bool
	Success    bool
	Error      error
}

// WebhookSender delivers events via HTTP webhook with optional HMAC-SHA256 signing.
type WebhookSender struct {
	client        *http.Client
	ssrfBypass    bool // When true, skip SSRF validation (for testing only)
}

// NewWebhookSender creates a new WebhookSender with the given timeout.
func NewWebhookSender(timeout time.Duration) *WebhookSender {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &WebhookSender{
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

// Send delivers the event to the configured webhook URL.
// It returns a DeliveryResult indicating success/failure and retryability.
func (s *WebhookSender) Send(event *Event, dest DestinationConfig) DeliveryResult {
	deliveryID := uuid.New().String()

	// SSRF protection: validate the URL before making the request
	// Skip validation if ssrfBypass is set (for internal testing only)
	if !s.ssrfBypass {
		if err := validateWebhookURL(dest.URL); err != nil {
			return DeliveryResult{
				DeliveryID: deliveryID,
				Success:    false,
				Retryable:  false,
				Error:      fmt.Errorf("webhook URL validation failed: %w", err),
			}
		}
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return DeliveryResult{
			DeliveryID: deliveryID,
			Success:    false,
			Retryable:  false,
			Error:      fmt.Errorf("failed to marshal event: %w", err),
		}
	}

	req, err := http.NewRequest("POST", dest.URL, bytes.NewReader(payload))
	if err != nil {
		return DeliveryResult{
			DeliveryID: deliveryID,
			Success:    false,
			Retryable:  false,
			Error:      fmt.Errorf("failed to create request: %w", err),
		}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Nexus-Delivery", deliveryID)
	req.Header.Set("X-Nexus-Event-Type", event.EventType)

	// Sign payload if signing config is provided
	if dest.Signing != nil && dest.Signing.Secret != "" {
		sig := signPayload(payload, dest.Signing.Secret)
		req.Header.Set("X-Nexus-Signature", sig)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return DeliveryResult{
			DeliveryID: deliveryID,
			Success:    false,
			Retryable:  true,
			Error:      fmt.Errorf("request failed: %w", err),
		}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	retryable := false

	if !success {
		// 5xx is retryable
		if resp.StatusCode >= 500 {
			retryable = true
		}
		// 429 (Too Many Requests) is retryable
		if resp.StatusCode == 429 {
			retryable = true
		}
		// Other 4xx are non-retryable
	}

	return DeliveryResult{
		DeliveryID: deliveryID,
		StatusCode: resp.StatusCode,
		Retryable:  retryable,
		Success:    success,
	}
}

// signPayload computes the HMAC-SHA256 signature of the payload using the given secret.
func signPayload(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature verifies the HMAC-SHA256 signature of a payload.
func VerifySignature(payload []byte, secret, signature string) bool {
	expected := signPayload(payload, secret)
	return hmac.Equal([]byte(expected), []byte(signature))
}

// validateWebhookURL checks that a webhook URL does not target internal
// network addresses (SSRF protection). Only HTTP/HTTPS schemes are allowed,
// and the resolved IP must not be a loopback, link-local, or private address.
func validateWebhookURL(rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("URL is empty")
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	// Only allow HTTP and HTTPS schemes
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("invalid scheme %s: only http and https are allowed", parsed.Scheme)
	}

	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("URL has no host")
	}

	// Resolve the hostname to IP addresses
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("failed to resolve hostname %s: %w", host, err)
	}

	for _, ip := range ips {
		if isPrivateIP(ip) {
			return fmt.Errorf("webhook URL resolves to private/internal IP %s (SSRF protection)", ip)
		}
	}

	return nil
}

// isPrivateIP checks if an IP address is a loopback, link-local, or private address.
func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() {
		return true
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.IsPrivate() {
		return true
	}
	if ip.IsUnspecified() {
		return true
	}
	return false
}
