package pluginloader

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func makeTestPluginDir(t *testing.T) string {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{
		"manifest_version": "1.0",
		"api_version": ">=1.0.0 <2.0.0",
		"name": "demo",
		"version": "1.0.0",
		"trust_tier_requested": 0
	}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plugin.wasm"), []byte("wasm bytes"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "signature.sig"), []byte("sig"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "key_id"), []byte("key-1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Demo"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "assets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "assets", "icon.png"), []byte("png"), 0o644))
	return dir
}

func TestOCIPackager_PackFromDir(t *testing.T) {
	dir := makeTestPluginDir(t)
	p := NewOCIPackager()
	artifact, err := p.PackFromDir(dir)
	require.NoError(t, err)
	require.NotNil(t, artifact.Manifest)
	require.Equal(t, []byte("wasm bytes"), artifact.WASM)
	require.Equal(t, []byte("sig"), artifact.Signature)
	require.Equal(t, "key-1", artifact.KeyID)
	require.Equal(t, []byte("# Demo"), artifact.README)
	require.Equal(t, []byte("png"), artifact.Assets["icon.png"])
}

func TestOCIPackager_LayoutRoundTrip(t *testing.T) {
	dir := makeTestPluginDir(t)
	p := NewOCIPackager()
	artifact, err := p.PackFromDir(dir)
	require.NoError(t, err)

	layoutDir := t.TempDir()
	_, err = p.PackToLayout(artifact, layoutDir)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(layoutDir, "index.json"))
	require.FileExists(t, filepath.Join(layoutDir, "oci-layout"))

	pulled, err := p.PullFromLayout(layoutDir)
	require.NoError(t, err)
	require.Equal(t, artifact.Manifest, pulled.Manifest)
	require.Equal(t, artifact.WASM, pulled.WASM)
	require.Equal(t, artifact.Signature, pulled.Signature)
	require.Equal(t, artifact.KeyID, pulled.KeyID)
	require.Equal(t, artifact.README, pulled.README)
	require.Equal(t, artifact.Assets, pulled.Assets)
}

func TestOCIPackager_ParseOCIURL(t *testing.T) {
	r, repo, ref, err := parseOCIURL("registry.example.com/nexus/plugins/demo:v1.0.0")
	require.NoError(t, err)
	require.Equal(t, "registry.example.com", r)
	require.Equal(t, "nexus/plugins/demo", repo)
	require.Equal(t, "v1.0.0", ref)

	r, repo, ref, err = parseOCIURL("oci://registry.example.com/nexus/plugins/demo@sha256:abc")
	require.NoError(t, err)
	require.Equal(t, "registry.example.com", r)
	require.Equal(t, "nexus/plugins/demo", repo)
	require.Equal(t, "sha256:abc", ref)
}

func TestOCIPackager_RegistryRoundTrip(t *testing.T) {
	dir := makeTestPluginDir(t)
	p := NewOCIPackager()
	artifact, err := p.PackFromDir(dir)
	require.NoError(t, err)

	// In-memory blob store keyed by digest.
	blobs := make(map[string][]byte)
	manifestBlob := []byte{}
	manifestDigest := ""

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/blobs/uploads/"):
			w.Header().Set("Location", "/upload?uuid=1")
			w.WriteHeader(http.StatusAccepted)
		case strings.HasPrefix(r.URL.Path, "/upload"):
			digest := r.URL.Query().Get("digest")
			if r.Method == "PUT" && digest != "" {
				body, _ := io.ReadAll(r.Body)
				blobs[digest] = body
				w.WriteHeader(http.StatusCreated)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(r.URL.Path, "/blobs/"):
			digest := r.URL.Query().Get("digest")
			if digest == "" {
				digest = strings.TrimPrefix(r.URL.Path, "/v2/demo/blobs/")
			}
			if r.Method == "GET" {
				data, ok := blobs[digest]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Write(data)
				return
			}
			if r.Method == "PUT" {
				body, _ := io.ReadAll(r.Body)
				blobs[digest] = body
				w.WriteHeader(http.StatusCreated)
			}
		case strings.HasSuffix(r.URL.Path, "/manifests/v1.0.0"):
			if r.Method == "GET" {
				w.Header().Set("Content-Type", mediaTypePluginArtifact)
				w.Write(manifestBlob)
				return
			}
			if r.Method == "PUT" {
				body, _ := io.ReadAll(r.Body)
				manifestBlob = body
				manifestDigest = "sha256:" + sha256String(body)
				blobs[manifestDigest] = body
				w.WriteHeader(http.StatusCreated)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	// Push.
	ociURL := strings.TrimPrefix(server.URL, "http://") + "/demo:v1.0.0"
	err = p.PushToRegistry(context.Background(), artifact, ociURL, nil)
	require.NoError(t, err)
	require.NotEmpty(t, manifestBlob)

	// Pull.
	pulled, err := p.PullFromRegistry(context.Background(), ociURL, nil)
	require.NoError(t, err)
	require.Equal(t, artifact.Manifest, pulled.Manifest)
	require.Equal(t, artifact.WASM, pulled.WASM)
	require.Equal(t, artifact.KeyID, pulled.KeyID)
}

func sha256String(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestOCIPackager_PackToTarGz(t *testing.T) {
	dir := makeTestPluginDir(t)
	p := NewOCIPackager()

	var buf bytes.Buffer
	require.NoError(t, p.PackToTarGz(dir, &buf))
	require.NotZero(t, buf.Len())

	gr, err := gzip.NewReader(&buf)
	require.NoError(t, err)
	defer gr.Close()
	tr := tar.NewReader(gr)

	found := make(map[string]bool)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		found[hdr.Name] = true
	}
	require.True(t, found["manifest.json"])
	require.True(t, found["plugin.wasm"])
	require.True(t, found["assets/icon.png"])
}

func TestOCIPackager_SetHTTPClient(t *testing.T) {
	p := NewOCIPackager()
	client := &http.Client{Timeout: 5 * time.Second}
	p.SetHTTPClient(client)
	require.Equal(t, client, p.httpClient)
}

func TestOCIAuth_AuthorizationHeader(t *testing.T) {
	basic := &OCIAuth{Type: "basic", Username: "user", Password: "pass"}
	require.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("user:pass")), basic.AuthorizationHeader())

	bearer := &OCIAuth{Type: "bearer", Token: "tok"}
	require.Equal(t, "Bearer tok", bearer.AuthorizationHeader())

	require.Equal(t, "", (&OCIAuth{Type: "none"}).AuthorizationHeader())
	require.Equal(t, "", (*OCIAuth)(nil).AuthorizationHeader())
}
