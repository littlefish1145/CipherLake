package pluginloader

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cipherlake/internal/logger"

	"go.uber.org/zap"
)

// NetworkEgressValidator enforces the outbound network whitelist for a
// plugin instance (spec §3.10, P2-3). It is initialized from the plugin's
// effective trust tier and manifest network_egress patterns.
//
// Rules:
//   - Tier 0 (untrusted): all outbound traffic is denied.
//   - Tier 1 (trusted signed): allowed only when the URL matches one of the
//     patterns declared in manifest.network_egress.
//   - Tier 2 (core signed): allowed to any URL (still audited).
type NetworkEgressValidator struct {
	tier     int
	patterns []string
}

// NewNetworkEgressValidator creates a validator for the given tier and
// manifest patterns. Patterns are copied from the manifest.
func NewNetworkEgressValidator(tier int, patterns []string) *NetworkEgressValidator {
	p := make([]string, len(patterns))
	copy(p, patterns)
	return &NetworkEgressValidator{tier: tier, patterns: p}
}

// Allow reports whether the plugin may fetch the given URL. It returns an
// error describing the policy violation when the fetch is denied.
func (v *NetworkEgressValidator) Allow(rawURL string) error {
	switch v.tier {
	case TierUntrusted:
		return fmt.Errorf("outbound network denied for Tier %d plugin", v.tier)
	case TierCore:
		return nil
	case TierTrusted:
		if len(v.patterns) == 0 {
			return fmt.Errorf("outbound network denied: manifest.network_egress is empty")
		}
		for _, pat := range v.patterns {
			if matchURLPattern(pat, rawURL) {
				return nil
			}
		}
		return fmt.Errorf("outbound URL %q does not match any manifest.network_egress pattern", rawURL)
	default:
		return fmt.Errorf("outbound network denied for unknown trust tier %d", v.tier)
	}
}

// matchURLPattern performs glob matching on the full URL string. '*' matches
// any sequence of characters (including '/'). Patterns are matched exactly
// against the normalized URL string (scheme://host/path...).
func matchURLPattern(pattern, rawURL string) bool {
	// Normalize the URL so that patterns don't have to handle default ports.
	normalized, err := normalizeURL(rawURL)
	if err != nil {
		// Fallback to raw string matching if parsing fails.
		normalized = rawURL
	}
	return globMatch(pattern, normalized)
}

// normalizeURL strips default ports so a manifest pattern like
// "https://api.example.com/*" also matches "https://api.example.com:443/".
func normalizeURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("missing scheme or host")
	}
	host := u.Hostname()
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		host = host + ":" + port
	}
	out := u.Scheme + "://" + host + u.Path
	if u.RawQuery != "" {
		out = out + "?" + u.RawQuery
	}
	return out, nil
}

// globMatch matches s against pattern where '*' matches any run of chars.
func globMatch(pattern, s string) bool {
	// Fast path: exact match.
	if pattern == s {
		return true
	}
	// Use a simple two-pointer scan for patterns containing only '*' wildcards.
	for pattern != "" && s != "" {
		switch pattern[0] {
		case '*':
			// Consume consecutive '*'.
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if pattern == "" {
				return true
			}
			// Find the next literal segment after '*'.
			seg := ""
			for len(pattern) > 0 && pattern[0] != '*' {
				seg += string(pattern[0])
				pattern = pattern[1:]
			}
			// s must contain seg; advance to the last occurrence to keep greediness simple.
			idx := strings.LastIndex(s, seg)
			if idx < 0 {
				return false
			}
			s = s[idx+len(seg):]
		default:
			if pattern[0] != s[0] {
				return false
			}
			pattern = pattern[1:]
			s = s[1:]
		}
	}
	// Accept trailing '*' in pattern.
	for len(pattern) > 0 && pattern[0] == '*' {
		pattern = pattern[1:]
	}
	return pattern == "" && s == ""
}

// HTTPFetcher performs outbound HTTP requests on behalf of a plugin.
// It is separated from HostImports so tests can inject a fake transport.
type HTTPFetcher struct {
	client *http.Client
	logger *zap.Logger
}

// NewHTTPFetcher creates a fetcher with sensible defaults. Callers may
// replace client.Transport in tests.
func NewHTTPFetcher() *HTTPFetcher {
	return &HTTPFetcher{
		client: &http.Client{
			Timeout: 30 * time.Second,
			// No redirects followed automatically; plugins must handle 3xx themselves.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger: zap.L(),
	}
}

// FetchResult is the JSON shape returned to the WASM guest by http.fetch.
type FetchResult struct {
	Status     int               `json:"status"`
	StatusText string            `json:"status_text"`
	Headers    map[string]string `json:"headers"`
	BodyB64    string            `json:"body_b64"`
}

// Fetch executes an HTTP request and returns a structured result.
func (f *HTTPFetcher) Fetch(method, rawURL string, headers map[string]string, body []byte, timeout time.Duration) (*FetchResult, error) {
	if timeout > 0 {
		client := *f.client
		client.Timeout = timeout
		f.client = &client
	}

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = strings.NewReader(string(body))
	}
	req, err := http.NewRequest(method, rawURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := f.client.Do(req)
	latency := time.Since(start)
	if err != nil {
		logger.AuditLogger("http.fetch", rawURL, "", "denied", map[string]interface{}{
			"method":   method,
			"error":    err.Error(),
			"latency":  latency.String(),
		})
		return nil, fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	respHeaders := make(map[string]string, len(resp.Header))
	for k, v := range resp.Header {
		if len(v) > 0 {
			respHeaders[k] = v[0]
		}
	}

	logger.AuditLogger("http.fetch", rawURL, "", "allowed", map[string]interface{}{
		"method":      method,
		"status":      resp.StatusCode,
		"bytes_out":   len(body),
		"bytes_in":    len(respBody),
		"latency":     latency.String(),
		"headers_out": len(headers),
	})

	return &FetchResult{
		Status:     resp.StatusCode,
		StatusText: http.StatusText(resp.StatusCode),
		Headers:    respHeaders,
		BodyB64:    base64ForNetwork(respBody),
	}, nil
}

// base64ForNetwork encodes bytes as standard base64 for the FetchResult body.
func base64ForNetwork(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// SetLogger replaces the fetcher logger. Useful in tests.
func (f *HTTPFetcher) SetLogger(l *zap.Logger) {
	f.logger = l
}
