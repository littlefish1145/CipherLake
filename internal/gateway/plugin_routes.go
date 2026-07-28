package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"cipherlake/internal/logger"
	pluginpb "cipherlake/proto/plugin"

	"go.uber.org/zap"
)

// handlePluginRoute dispatches requests under /_plugins/<plugin_name>/* to the
// plugin-loader microservice via gRPC. The loader runs the plugin's on_request
// entry point and returns an HTTP response that we mirror back to the client.
//
// Path format: /_plugins/<plugin_name>/<sub-path...>
// The plugin_name is the first path segment after /_plugins/. Everything after
// it is passed to the plugin as the sub-route path.
func (g *S3Gateway) handlePluginRoute(w http.ResponseWriter, r *http.Request) {
	if g.loaderClient == nil {
		http.Error(w, "plugin loader not configured", http.StatusServiceUnavailable)
		return
	}

	pluginName, subPath, ok := splitPluginPath(r.URL.Path)
	if !ok {
		http.Error(w, "invalid plugin route", http.StatusBadRequest)
		return
	}

	// Buffer the request body. Phase 1 keeps it simple; large bodies are
	// rejected by the Gateway's existing maxRequestBodyBytes limit before
	// this handler is reached.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		logger.Error("failed to read plugin request body", zap.Error(err))
		http.Error(w, "failed to read request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	// Collect headers into a single-value map. Multi-value headers are joined
	// with commas, matching HTTP semantics and keeping the proto simple.
	headers := make(map[string]string, len(r.Header))
	for k, vals := range r.Header {
		if len(vals) == 1 {
			headers[k] = vals[0]
		} else {
			headers[k] = strings.Join(vals, ",")
		}
	}

	originalUser := g.getUserID(r)

	req := &pluginpb.InvokeRouteRequest{
		PluginName:    pluginName,
		Method:        r.Method,
		Path:          subPath,
		Host:          r.Host,
		Headers:       headers,
		Body:          body,
		OriginalUser:  originalUser,
	}

	ctx, cancel := context.WithTimeout(r.Context(), g.pluginLoaderCallTimeout())
	defer cancel()

	resp, err := g.loaderClient.InvokeRoute(ctx, req)
	if err != nil {
		logger.Error("plugin loader invoke route failed",
			zap.String("plugin", pluginName),
			zap.String("path", subPath),
			zap.Error(err),
		)
		http.Error(w, "plugin loader unavailable", http.StatusBadGateway)
		return
	}

	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(int(resp.StatusCode))
	if _, err := w.Write(resp.Body); err != nil {
		logger.Warn("failed to write plugin response body",
			zap.String("plugin", pluginName),
			zap.Error(err),
		)
	}
}

// splitPluginPath splits "/_plugins/<plugin_name>/<sub-path...>" into the
// plugin name and the remaining sub-path. The sub-path retains a leading
// slash so that plugins can route with the same path semantics as HTTP.
// Returns ok=false if the path does not contain a plugin name.
func splitPluginPath(path string) (pluginName, subPath string, ok bool) {
	const prefix = "/_plugins/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	trimmed := strings.TrimPrefix(path, prefix)
	if trimmed == "" {
		return "", "", false
	}
	parts := strings.SplitN(trimmed, "/", 2)
	pluginName = parts[0]
	if pluginName == "" {
		return "", "", false
	}
	if len(parts) == 2 {
		subPath = "/" + parts[1]
	}
	return pluginName, subPath, true
}

// pluginLoaderCallTimeout returns the configured Gateway→Loader call timeout.
// Defaults to 500ms per spec §3.12 A3.
func (g *S3Gateway) pluginLoaderCallTimeout() time.Duration {
	if g.config != nil && g.config.PluginLoader.CallTimeout != "" {
		if d, err := time.ParseDuration(g.config.PluginLoader.CallTimeout); err == nil && d > 0 {
			return d
		}
	}
	return 500 * time.Millisecond
}

