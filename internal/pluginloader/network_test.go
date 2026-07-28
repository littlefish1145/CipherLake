package pluginloader

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNetworkEgressValidator_Tier0Denied(t *testing.T) {
	v := NewNetworkEgressValidator(TierUntrusted, []string{"https://example.com/*"})
	err := v.Allow("https://example.com/api")
	require.Error(t, err)
	require.Contains(t, err.Error(), "Tier 0")
}

func TestNetworkEgressValidator_Tier2Allowed(t *testing.T) {
	v := NewNetworkEgressValidator(TierCore, nil)
	err := v.Allow("https://anywhere.example.com/secret")
	require.NoError(t, err)
}

func TestNetworkEgressValidator_Tier1PatternMatch(t *testing.T) {
	v := NewNetworkEgressValidator(TierTrusted, []string{
		"https://api.example.com/*",
		"https://*.example.com/healthz",
	})

	require.NoError(t, v.Allow("https://api.example.com/v1/data"))
	require.NoError(t, v.Allow("https://api.example.com:443/v1/data"))
	require.NoError(t, v.Allow("https://svc.example.com/healthz"))

	err := v.Allow("https://api.example.com:8443/v1/data")
	require.Error(t, err)

	err = v.Allow("https://other.com/")
	require.Error(t, err)
}

func TestNetworkEgressValidator_Tier1EmptyPatterns(t *testing.T) {
	v := NewNetworkEgressValidator(TierTrusted, nil)
	err := v.Allow("https://example.com/api")
	require.Error(t, err)
	require.Contains(t, err.Error(), "network_egress is empty")
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern string
		s       string
		want    bool
	}{
		{"https://api.example.com/*", "https://api.example.com/v1", true},
		{"https://api.example.com/*", "https://api.example.com/", true},
		{"https://*.example.com/*", "https://svc.example.com/foo", true},
		{"https://*.example.com/*", "https://a.b.example.com/foo", true},
		{"*example.com*", "https://example.com/", true},
		{"https://api.example.com/path", "https://api.example.com/path", true},
		{"https://api.example.com/*", "https://other.example.com/", false},
		{"https://api.example.com/foo*", "https://api.example.com/foobar", true},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"_"+tc.s, func(t *testing.T) {
			require.Equal(t, tc.want, globMatch(tc.pattern, tc.s))
		})
	}
}

func TestMatchURLPattern_NormalizesDefaultPort(t *testing.T) {
	require.True(t, matchURLPattern("https://api.example.com/*", "https://api.example.com:443/v1"))
	require.True(t, matchURLPattern("http://api.example.com/*", "http://api.example.com:80/v1"))
	require.False(t, matchURLPattern("https://api.example.com/*", "https://api.example.com:8443/v1"))
}

func TestHTTPFetcher_Fetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Echo-Method", r.Method)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("echo: " + string(body)))
	}))
	defer server.Close()

	fetcher := NewHTTPFetcher()
	fetcher.client.Timeout = 5 * time.Second

	result, err := fetcher.Fetch("POST", server.URL+"/test", map[string]string{"Content-Type": "text/plain"}, []byte("hello"), 0)
	require.NoError(t, err)
	require.Equal(t, 200, result.Status)
	require.Equal(t, "OK", result.StatusText)
	require.Equal(t, "POST", result.Headers["X-Echo-Method"])
	require.Equal(t, "echo: hello", mustDecodeB64(t, result.BodyB64))
}

func TestHTTPFetcher_FetchTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	fetcher := NewHTTPFetcher()
	_, err := fetcher.Fetch("GET", server.URL+"/slow", nil, nil, 50*time.Millisecond)
	require.Error(t, err)
}

func TestHTTPFetcher_RedirectNotFollowed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	fetcher := NewHTTPFetcher()
	result, err := fetcher.Fetch("GET", server.URL+"/redirect", nil, nil, 0)
	require.NoError(t, err)
	require.Equal(t, http.StatusFound, result.Status)
}

func TestHTTPFetcher_EmptyBodyBase64(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	fetcher := NewHTTPFetcher()
	result, err := fetcher.Fetch("GET", server.URL+"/empty", nil, nil, 0)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, result.Status)
	require.Empty(t, result.BodyB64)
}

func mustDecodeB64(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	require.NoError(t, err)
	return string(b)
}
