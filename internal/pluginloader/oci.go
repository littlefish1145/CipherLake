package pluginloader

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// OCIPackager implements the plugin OCI artifact format (spec §3.16).
// It supports packing a local plugin directory into an in-memory artifact,
// writing/reading an OCI layout directory, and pushing/pulling to a
// registry over HTTP(S) with basic or bearer authentication.
type OCIPackager struct {
	httpClient *http.Client
}

// NewOCIPackager creates a packager with the default HTTP client.
func NewOCIPackager() *OCIPackager {
	return &OCIPackager{httpClient: http.DefaultClient}
}

// OCIArtifact holds the files that make up a plugin package.
type OCIArtifact struct {
	Manifest  []byte
	WASM      []byte
	Signature []byte
	KeyID     string
	README    []byte
	Assets    map[string][]byte // relative path -> bytes
}

// OCIManifest is a minimal OCI image manifest for a plugin artifact.
// It stores the manifest JSON, WASM, signature, key_id and optional files
// as individual layers with custom media types.
type OCIManifest struct {
	SchemaVersion int                `json:"schemaVersion"`
	MediaType     string             `json:"mediaType"`
	Config        OCIDescriptor      `json:"config"`
	Layers        []OCIDescriptor    `json:"layers"`
	Annotations   map[string]string  `json:"annotations,omitempty"`
}

// OCIDescriptor describes a blob in the OCI manifest.
type OCIDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

const (
	mediaTypePluginArtifact = "application/vnd.nexus.plugin.artifact.v1+json"
	mediaTypePluginManifest = "application/vnd.nexus.plugin.manifest.v1+json"
	mediaTypePluginWASM     = "application/vnd.nexus.plugin.wasm.v1+octet-stream"
	mediaTypePluginSig      = "application/vnd.nexus.plugin.signature.v1+octet-stream"
	mediaTypePluginKeyID    = "application/vnd.nexus.plugin.keyid.v1+text"
	mediaTypePluginREADME   = "text/markdown"
	mediaTypePluginAsset    = "application/vnd.nexus.plugin.asset.v1+octet-stream"
	mediaTypeOCIConfig      = "application/vnd.oci.empty.v1+json"
	emptyConfigDigest       = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// PackFromDir reads a plugin directory and returns an OCIArtifact.
// Expected files: manifest.json, plugin.wasm, signature.sig, key_id (optional),
// README.md (optional), assets/* (optional).
func (p *OCIPackager) PackFromDir(dir string) (*OCIArtifact, error) {
	manifest, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("read manifest.json: %w", err)
	}
	wasm, err := os.ReadFile(filepath.Join(dir, "plugin.wasm"))
	if err != nil {
		return nil, fmt.Errorf("read plugin.wasm: %w", err)
	}
	artifact := &OCIArtifact{
		Manifest: manifest,
		WASM:     wasm,
		Assets:   make(map[string][]byte),
	}
	if b, err := os.ReadFile(filepath.Join(dir, "signature.sig")); err == nil {
		artifact.Signature = b
	}
	if b, err := os.ReadFile(filepath.Join(dir, "key_id")); err == nil {
		artifact.KeyID = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(filepath.Join(dir, "README.md")); err == nil {
		artifact.README = b
	}
	assetsDir := filepath.Join(dir, "assets")
	if entries, err := os.ReadDir(assetsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			b, err := os.ReadFile(filepath.Join(assetsDir, e.Name()))
			if err != nil {
				return nil, fmt.Errorf("read asset %s: %w", e.Name(), err)
			}
			artifact.Assets[e.Name()] = b
		}
	}
	return artifact, nil
}

// PackToLayout writes an OCI artifact as a local OCI layout directory
// (https://github.com/opencontainers/image-spec/blob/main/image-layout.md).
// The layout can be pushed to a registry with PushFromLayout.
func (p *OCIPackager) PackToLayout(artifact *OCIArtifact, layoutDir string) (*OCIManifest, error) {
	if err := os.MkdirAll(filepath.Join(layoutDir, "blobs", "sha256"), 0o755); err != nil {
		return nil, fmt.Errorf("create layout dirs: %w", err)
	}

	manifest := OCIManifest{
		SchemaVersion: 2,
		MediaType:     mediaTypePluginArtifact,
		Config: OCIDescriptor{
			MediaType: mediaTypeOCIConfig,
			Digest:    emptyConfigDigest,
			Size:      0,
		},
		Annotations: map[string]string{
			"org.opencontainers.image.title": pluginNameFromManifest(artifact.Manifest),
		},
	}

	addLayer := func(mediaType, filename string, data []byte) {
		if len(data) == 0 {
			return
		}
		digest, _ := writeBlob(layoutDir, data)
		manifest.Layers = append(manifest.Layers, OCIDescriptor{
			MediaType: mediaType,
			Digest:    digest,
			Size:      int64(len(data)),
			Annotations: map[string]string{
				"org.opencontainers.image.title": filename,
			},
		})
	}

	addLayer(mediaTypePluginManifest, "manifest.json", artifact.Manifest)
	addLayer(mediaTypePluginWASM, "plugin.wasm", artifact.WASM)
	addLayer(mediaTypePluginSig, "signature.sig", artifact.Signature)
	if artifact.KeyID != "" {
		addLayer(mediaTypePluginKeyID, "key_id", []byte(artifact.KeyID))
	}
	addLayer(mediaTypePluginREADME, "README.md", artifact.README)
	for name, data := range artifact.Assets {
		addLayer(mediaTypePluginAsset, path.Join("assets", name), data)
	}

	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	manifestDigest, _ := writeBlob(layoutDir, manifestJSON)

	idx := struct {
		SchemaVersion int               `json:"schemaVersion"`
		Manifests     []OCIDescriptor `json:"manifests"`
	}{
		SchemaVersion: 2,
		Manifests: []OCIDescriptor{{
			MediaType: mediaTypePluginArtifact,
			Digest:    manifestDigest,
			Size:      int64(len(manifestJSON)),
			Annotations: map[string]string{
				"org.opencontainers.image.ref.name": "latest",
			},
		}},
	}
	idxJSON, err := json.Marshal(idx)
	if err != nil {
		return nil, fmt.Errorf("marshal index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(layoutDir, "index.json"), idxJSON, 0o644); err != nil {
		return nil, fmt.Errorf("write index.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(layoutDir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		return nil, fmt.Errorf("write oci-layout: %w", err)
	}
	return &manifest, nil
}

// PullFromLayout reads an OCI layout directory and returns the plugin artifact.
func (p *OCIPackager) PullFromLayout(layoutDir string) (*OCIArtifact, error) {
	idxJSON, err := os.ReadFile(filepath.Join(layoutDir, "index.json"))
	if err != nil {
		return nil, fmt.Errorf("read index.json: %w", err)
	}
	var idx struct {
		Manifests []OCIDescriptor `json:"manifests"`
	}
	if err := json.Unmarshal(idxJSON, &idx); err != nil {
		return nil, fmt.Errorf("parse index.json: %w", err)
	}
	if len(idx.Manifests) == 0 {
		return nil, errors.New("no manifests in OCI layout")
	}
	manifestJSON, err := readBlob(layoutDir, idx.Manifests[0].Digest)
	if err != nil {
		return nil, fmt.Errorf("read manifest blob: %w", err)
	}
	var manifest OCIManifest
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest blob: %w", err)
	}
	return artifactFromManifest(layoutDir, manifest)
}

// PullFromRegistry fetches a plugin artifact from an OCI registry.
// ociURL format: <registry>/<repository>:<tag> or <registry>/<repository>@<digest>.
// Auth may be nil for anonymous access.
func (p *OCIPackager) PullFromRegistry(ctx context.Context, ociURL string, auth *OCIAuth) (*OCIArtifact, error) {
	registry, repo, ref, err := parseOCIURL(ociURL)
	if err != nil {
		return nil, err
	}
	base := "https://" + registry
	if strings.HasPrefix(registry, "localhost:") || strings.HasPrefix(registry, "127.0.0.1:") {
		base = "http://" + registry
	}

	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", base, repo, ref)
	manifestJSON, err := p.doReq(ctx, "GET", manifestURL, auth, nil, "application/vnd.oci.image.manifest.v1+json,"+mediaTypePluginArtifact)
	if err != nil {
		return nil, fmt.Errorf("pull manifest: %w", err)
	}
	var manifest OCIManifest
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}

	// Fetch layers into a temporary layout for reuse of artifactFromManifest.
	tmpDir, err := os.MkdirTemp("", "nexus-oci-pull-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	if err := os.MkdirAll(filepath.Join(tmpDir, "blobs", "sha256"), 0o755); err != nil {
		return nil, err
	}

	for _, layer := range append([]OCIDescriptor{manifest.Config}, manifest.Layers...) {
		if layer.Size == 0 && layer.Digest == emptyConfigDigest {
			continue
		}
		digest := strings.TrimPrefix(layer.Digest, "sha256:")
		blobURL := fmt.Sprintf("%s/v2/%s/blobs/sha256:%s", base, repo, digest)
		data, err := p.doReq(ctx, "GET", blobURL, auth, nil, "")
		if err != nil {
			return nil, fmt.Errorf("pull blob %s: %w", layer.Digest, err)
		}
		if _, err := writeBlob(tmpDir, data); err != nil {
			return nil, err
		}
	}
	return artifactFromManifest(tmpDir, manifest)
}

// PushToRegistry uploads a plugin artifact to an OCI registry.
// ociURL format: <registry>/<repository>:<tag>.
func (p *OCIPackager) PushToRegistry(ctx context.Context, artifact *OCIArtifact, ociURL string, auth *OCIAuth) error {
	registry, repo, ref, err := parseOCIURL(ociURL)
	if err != nil {
		return err
	}
	base := "https://" + registry
	if strings.HasPrefix(registry, "localhost:") || strings.HasPrefix(registry, "127.0.0.1:") {
		base = "http://" + registry
	}

	// Build a temporary layout to obtain manifest + blobs.
	tmpDir, err := os.MkdirTemp("", "nexus-oci-push-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	manifest, err := p.PackToLayout(artifact, tmpDir)
	if err != nil {
		return err
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return err
	}

	// Upload each blob.
	uploaded := make(map[string]bool)
	uploadBlob := func(desc OCIDescriptor, data []byte) error {
		if desc.Size == 0 && desc.Digest == emptyConfigDigest {
			return nil
		}
		if uploaded[desc.Digest] {
			return nil
		}
		uploadURL := fmt.Sprintf("%s/v2/%s/blobs/uploads/", base, repo)
		resp, err := p.doReqResp(ctx, "POST", uploadURL, auth, nil, "")
		if err != nil {
			return fmt.Errorf("initiate blob upload: %w", err)
		}
		loc := resp.Header.Get("Location")
		_ = resp.Body.Close()
		if loc == "" {
			return errors.New("registry did not return upload Location")
		}
		if !strings.Contains(loc, "://") {
			loc = base + loc
		}
		loc += "&digest=" + url.QueryEscape(desc.Digest)
		_, err = p.doReq(ctx, "PUT", loc, auth, data, "application/octet-stream")
		if err != nil {
			return fmt.Errorf("upload blob %s: %w", desc.Digest, err)
		}
		uploaded[desc.Digest] = true
		return nil
	}

	for _, layer := range manifest.Layers {
		data, err := readBlob(tmpDir, layer.Digest)
		if err != nil {
			return err
		}
		if err := uploadBlob(layer, data); err != nil {
			return err
		}
	}
	if err := uploadBlob(manifest.Config, nil); err != nil {
		return err
	}

	// Upload manifest.
	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", base, repo, ref)
	_, err = p.doReq(ctx, "PUT", manifestURL, auth, manifestJSON, mediaTypePluginArtifact)
	if err != nil {
		return fmt.Errorf("upload manifest: %w", err)
	}
	return nil
}

// OCIAuth holds registry authentication.
type OCIAuth struct {
	Type     string // "basic" | "bearer"
	Username string
	Password string
	Token    string
}

// AuthorizationHeader returns the HTTP Authorization header value.
func (a *OCIAuth) AuthorizationHeader() string {
	if a == nil {
		return ""
	}
	switch a.Type {
	case "basic":
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(a.Username+":"+a.Password))
	case "bearer":
		return "Bearer " + a.Token
	}
	return ""
}

func (p *OCIPackager) doReq(ctx context.Context, method, urlStr string, auth *OCIAuth, body []byte, accept string) ([]byte, error) {
	resp, err := p.doReqResp(ctx, method, urlStr, auth, body, accept)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("registry %s %s returned %d: %s", method, urlStr, resp.StatusCode, string(data))
	}
	return data, nil
}

func (p *OCIPackager) doReqResp(ctx context.Context, method, urlStr string, auth *OCIAuth, body []byte, accept string) (*http.Response, error) {
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, bodyReader)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	if hdr := auth.AuthorizationHeader(); hdr != "" {
		req.Header.Set("Authorization", hdr)
	}
	return p.httpClient.Do(req)
}

func parseOCIURL(ociURL string) (registry, repo, ref string, err error) {
	ociURL = strings.TrimPrefix(ociURL, "oci://")
	atIdx := strings.Index(ociURL, "@")
	if atIdx >= 0 {
		registryRepo := ociURL[:atIdx]
		ref = ociURL[atIdx+1:]
		slashIdx := strings.Index(registryRepo, "/")
		if slashIdx < 0 {
			return "", "", "", errors.New("invalid OCI URL: missing repository")
		}
		return registryRepo[:slashIdx], registryRepo[slashIdx+1:], ref, nil
	}
	colonIdx := strings.LastIndex(ociURL, ":")
	slashIdx := strings.Index(ociURL, "/")
	if slashIdx < 0 || colonIdx < slashIdx {
		return "", "", "", errors.New("invalid OCI URL: expected <registry>/<repo>:<tag>")
	}
	return ociURL[:slashIdx], ociURL[slashIdx+1:colonIdx], ociURL[colonIdx+1:], nil
}

func writeBlob(layoutDir string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	path := filepath.Join(layoutDir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return digest, nil
}

func readBlob(layoutDir, digest string) ([]byte, error) {
	digest = strings.TrimPrefix(digest, "sha256:")
	path := filepath.Join(layoutDir, "blobs", "sha256", digest)
	return os.ReadFile(path)
}

func artifactFromManifest(layoutDir string, manifest OCIManifest) (*OCIArtifact, error) {
	artifact := &OCIArtifact{Assets: make(map[string][]byte)}
	for _, layer := range manifest.Layers {
		data, err := readBlob(layoutDir, layer.Digest)
		if err != nil {
			return nil, fmt.Errorf("read layer %s: %w", layer.Digest, err)
		}
		title := ""
		if layer.Annotations != nil {
			title = layer.Annotations["org.opencontainers.image.title"]
		}
		switch layer.MediaType {
		case mediaTypePluginManifest:
			artifact.Manifest = data
		case mediaTypePluginWASM:
			artifact.WASM = data
		case mediaTypePluginSig:
			artifact.Signature = data
		case mediaTypePluginKeyID:
			artifact.KeyID = strings.TrimSpace(string(data))
		case mediaTypePluginREADME:
			artifact.README = data
		case mediaTypePluginAsset:
			if title != "" {
				artifact.Assets[path.Base(title)] = data
			}
		}
	}
	if len(artifact.Manifest) == 0 {
		return nil, errors.New("artifact missing manifest.json layer")
	}
	if len(artifact.WASM) == 0 {
		return nil, errors.New("artifact missing plugin.wasm layer")
	}
	return artifact, nil
}

func pluginNameFromManifest(manifestJSON []byte) string {
	var m struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(manifestJSON, &m)
	return m.Name
}

// PackToTarGz creates a gzipped tar archive of the plugin directory.
// This is a convenience for local distribution and testing; the canonical
// format is the OCI layout (PackToLayout).
func (p *OCIPackager) PackToTarGz(dir string, w io.Writer) error {
	gw := gzip.NewWriter(w)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()
	return filepath.Walk(dir, func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, file)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

// SetHTTPClient replaces the default HTTP client (useful in tests).
func (p *OCIPackager) SetHTTPClient(c *http.Client) {
	p.httpClient = c
}
