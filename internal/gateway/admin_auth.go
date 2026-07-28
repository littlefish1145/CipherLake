package gateway

import (
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type AdminAuthConfig struct {
	Enabled             bool
	Port                string
	TokenFile           string
	AllowedIPs          []string
	TrustedProxyHeaders bool
	mtlsEnabled         bool
	mtlsCertFile        string
	mtlsKeyFile         string
	mtlsCAFile          string
	token               string
	auditLog            *AuditLogger
}

type AuditLogger struct {
	mu      sync.RWMutex
	entries []AuditEntry
	maxSize int
}

type AuditEntry struct {
	Timestamp time.Time `json:"timestamp"`
	UserID    string    `json:"user_id"`
	IP        string    `json:"ip"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	Error     string    `json:"error,omitempty"`
	UserAgent string    `json:"user_agent"`
}

func NewAuditLogger(maxSize int) *AuditLogger {
	if maxSize <= 0 {
		maxSize = 10000
	}
	return &AuditLogger{
		entries: make([]AuditEntry, 0, maxSize),
		maxSize: maxSize,
	}
}

func (a *AuditLogger) Log(entry AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()

	entry.Timestamp = time.Now()
	a.entries = append(a.entries, entry)

	if len(a.entries) > a.maxSize {
		a.entries = a.entries[len(a.entries)-a.maxSize:]
	}
}

func (a *AuditLogger) GetEntries(limit int) []AuditEntry {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if limit <= 0 || limit > len(a.entries) {
		limit = len(a.entries)
	}

	entries := make([]AuditEntry, limit)
	copy(entries, a.entries[len(a.entries)-limit:])
	return entries
}

type AdminAuthenticator struct {
	config *AdminAuthConfig
	users  map[string]*AdminUser
	mu     sync.RWMutex
}

type AdminUser struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Token      string     `json:"token"`
	Role       string     `json:"role"`
	AllowedIPs []string   `json:"allowed_ips"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

type AdminPermissions struct {
	CanViewStatus    bool `json:"can_view_status"`
	CanManageBuckets bool `json:"can_manage_buckets"`
	CanManageUsers   bool `json:"can_manage_users"`
	CanRunTiering    bool `json:"can_run_tiering"`
	CanManageKeys    bool `json:"can_manage_keys"`
	CanRebuildIndex  bool `json:"can_rebuild_index"`
	CanViewMetrics   bool `json:"can_view_metrics"`
	CanViewLogs      bool `json:"can_view_logs"`
}

func NewAdminAuthenticator(config *AdminAuthConfig) (*AdminAuthenticator, error) {
	auth := &AdminAuthenticator{
		config: config,
		users:  make(map[string]*AdminUser),
	}

	if config.TokenFile != "" {
		tokenData, err := os.ReadFile(config.TokenFile)
		if err == nil {
			auth.config.token = strings.TrimSpace(string(tokenData))
		}
	}

	if auth.config.token == "" {
		tokenBytes := make([]byte, 32)
		if _, err := rand.Read(tokenBytes); err != nil {
			return nil, fmt.Errorf("failed to generate admin token: %w", err)
		}
		auth.config.token = hex.EncodeToString(tokenBytes)
		log.Println("[SECURITY WARNING] No admin token configured. A random token was generated and will NOT persist across restarts. Please configure a token file via admin_auth.token_file or set the token file content.")
	}

	auth.users["admin"] = &AdminUser{
		ID:         "admin-001",
		Name:       "admin",
		Token:      auth.config.token,
		Role:       "admin",
		AllowedIPs: config.AllowedIPs,
		CreatedAt:  time.Now(),
	}

	if config.auditLog == nil {
		config.auditLog = NewAuditLogger(10000)
	}
	auth.config.auditLog = config.auditLog

	return auth, nil
}

func (a *AdminAuthenticator) Authenticate(r *http.Request) (*AdminUser, error) {
	clientIP := getClientIP(r)

	if len(a.config.AllowedIPs) > 0 {
		allowed := false
		for _, ip := range a.config.AllowedIPs {
			if ip == "*" || ip == clientIP || isIPInRange(clientIP, ip) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, fmt.Errorf("IP %s not allowed", clientIP)
		}
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return nil, fmt.Errorf("missing authorization header")
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid authorization header format")
	}

	switch strings.ToLower(parts[0]) {
	case "bearer":
		return a.validateBearerToken(parts[1])
	case "mtls":
		return a.validateMTLS(r)
	default:
		return nil, fmt.Errorf("unsupported authorization type")
	}
}

func (a *AdminAuthenticator) validateBearerToken(token string) (*AdminUser, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	for _, user := range a.users {
		if subtle.ConstantTimeCompare([]byte(user.Token), []byte(token)) == 1 {
			if user.ExpiresAt != nil && time.Now().After(*user.ExpiresAt) {
				return nil, fmt.Errorf("token expired")
			}
			return user, nil
		}
	}

	jwtUserID, err := a.parseJWT(token)
	if err == nil && jwtUserID != "" {
		for _, user := range a.users {
			if subtle.ConstantTimeCompare([]byte(user.ID), []byte(jwtUserID)) == 1 {
				return user, nil
			}
		}
	}

	return nil, fmt.Errorf("invalid token")
}

func (a *AdminAuthenticator) parseJWT(tokenString string) (string, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(a.config.token), nil
	})

	if err != nil {
		return "", err
	}

	if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
		if userID, ok := claims["user_id"].(string); ok {
			return userID, nil
		}
	}

	return "", fmt.Errorf("invalid token claims")
}

func (a *AdminAuthenticator) validateMTLS(r *http.Request) (*AdminUser, error) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		return nil, fmt.Errorf("mTLS certificate not provided")
	}

	cert := r.TLS.VerifiedChains[0][0]
	subject := cert.Subject.CommonName

	a.mu.RLock()
	defer a.mu.RUnlock()

	for _, user := range a.users {
		if user.Name == subject {
			return user, nil
		}
	}

	return nil, fmt.Errorf("mTLS certificate subject %q not authorized", subject)
}

func (a *AdminAuthenticator) GenerateToken(userID string, expiry time.Duration) (string, error) {
	claims := jwt.MapClaims{
		"user_id": userID,
		"exp":     time.Now().Add(expiry).Unix(),
		"iat":     time.Now().Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(a.config.token))
}

func (a *AdminAuthenticator) CreateUser(name, role string, allowedIPs []string, expiry time.Duration) (*AdminUser, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	user := &AdminUser{
		ID:         fmt.Sprintf("user-%d", time.Now().UnixNano()),
		Name:       name,
		Token:      fmt.Sprintf("%s-%d", a.config.token, time.Now().UnixNano()),
		Role:       role,
		AllowedIPs: allowedIPs,
		CreatedAt:  time.Now(),
	}

	if expiry > 0 {
		exp := time.Now().Add(expiry)
		user.ExpiresAt = &exp
	}

	a.users[name] = user

	return user, nil
}

func (a *AdminAuthenticator) DeleteUser(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.users, name)
	return nil
}

func (a *AdminAuthenticator) GetPermissions(role string) *AdminPermissions {
	switch role {
	case "admin":
		return &AdminPermissions{
			CanViewStatus:    true,
			CanManageBuckets: true,
			CanManageUsers:   true,
			CanRunTiering:    true,
			CanManageKeys:    true,
			CanRebuildIndex:  true,
			CanViewMetrics:   true,
			CanViewLogs:      true,
		}
	case "operator":
		return &AdminPermissions{
			CanViewStatus:    true,
			CanManageBuckets: false,
			CanManageUsers:   false,
			CanRunTiering:    true,
			CanManageKeys:    false,
			CanRebuildIndex:  true,
			CanViewMetrics:   true,
			CanViewLogs:      true,
		}
	case "viewer":
		return &AdminPermissions{
			CanViewStatus:    true,
			CanManageBuckets: false,
			CanManageUsers:   false,
			CanRunTiering:    false,
			CanManageKeys:    false,
			CanRebuildIndex:  false,
			CanViewMetrics:   true,
			CanViewLogs:      false,
		}
	default:
		return &AdminPermissions{}
	}
}

func (a *AdminAuthenticator) RequirePermission(permission string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			user, err := a.Authenticate(r)
			if err != nil {
				a.logAccess(r, 401, err.Error())
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			perms := a.GetPermissions(user.Role)

			switch permission {
			case "view_status":
				if !perms.CanViewStatus {
					a.logAccess(r, 403, "permission denied")
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
			case "manage_buckets":
				if !perms.CanManageBuckets {
					a.logAccess(r, 403, "permission denied")
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
			case "run_tiering":
				if !perms.CanRunTiering {
					a.logAccess(r, 403, "permission denied")
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
			case "manage_keys":
				if !perms.CanManageKeys {
					a.logAccess(r, 403, "permission denied")
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
			case "rebuild_index":
				if !perms.CanRebuildIndex {
					a.logAccess(r, 403, "permission denied")
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
			case "view_logs":
				if !perms.CanViewLogs {
					a.logAccess(r, 403, "permission denied")
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
			}

			a.logAccess(r, 200, "")
			next(w, r)
		}
	}
}

func (a *AdminAuthenticator) logAccess(r *http.Request, status int, err string) {
	if a.config.auditLog != nil {
		a.config.auditLog.Log(AuditEntry{
			UserID:    getUserID(r),
			IP:        getClientIP(r),
			Method:    r.Method,
			Path:      r.URL.Path,
			Status:    status,
			Error:     err,
			UserAgent: r.UserAgent(),
		})
	}
}

func (a *AdminAuthenticator) GetAuditLog(limit int) []AuditEntry {
	if a.config.auditLog != nil {
		return a.config.auditLog.GetEntries(limit)
	}
	return nil
}

func getClientIP(r *http.Request) string {
	cfg := getAdminAuthConfigFromContext(r)
	if cfg != nil && cfg.TrustedProxyHeaders {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.Split(xff, ",")[0]
		}
		if xri := r.Header.Get("X-Real-IP"); xri != "" {
			return xri
		}
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip != "" {
		return ip
	}
	return r.RemoteAddr
}

func getAdminAuthConfigFromContext(r *http.Request) *AdminAuthConfig {
	return nil
}

func getUserID(r *http.Request) string {
	return "unknown"
}

func isIPInRange(ip, cidr string) bool {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	parsedIP := net.ParseIP(ip)
	return ipNet.Contains(parsedIP)
}

type SecureAdminServer struct {
	server    *http.Server
	auth      *AdminAuthenticator
	listener  net.Listener
	wg        sync.WaitGroup
	startOnce sync.Once
	stopOnce  sync.Once
}

func NewSecureAdminServer(port string, auth *AdminAuthenticator, gateway *S3Gateway) (*SecureAdminServer, error) {
	mux := http.NewServeMux()

	server := &http.Server{
		Addr:         port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	s := &SecureAdminServer{
		server: server,
		auth:   auth,
	}

	mux.HandleFunc("/admin/status", auth.RequirePermission("view_status")(s.handleStatus(gateway)))
	mux.HandleFunc("/admin/tiering/decisions", auth.RequirePermission("view_status")(s.handleTieringDecisions(gateway)))
	mux.HandleFunc("/admin/tiering/run", auth.RequirePermission("run_tiering")(s.handleRunTiering(gateway)))
	mux.HandleFunc("/admin/vector/stats", auth.RequirePermission("view_status")(s.handleVectorStats(gateway)))
	mux.HandleFunc("/admin/vector/rebuild", auth.RequirePermission("rebuild_index")(s.handleVectorRebuild(gateway)))
	mux.HandleFunc("/admin/metrics", s.handleMetrics(gateway))
	mux.HandleFunc("/admin/audit", auth.RequirePermission("view_logs")(s.handleAuditLog))

	return s, nil
}

func (s *SecureAdminServer) handleStatus(gateway *S3Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		status := map[string]interface{}{
			"status":    "healthy",
			"timestamp": time.Now().Format(time.RFC3339),
			"version":   "2.0",
		}

		metadata := gateway.GetMetadataStore()
		if metadata != nil {
			status["metadata"] = "connected"
		}

		tiering := gateway.GetTieringManager()
		if tiering != nil {
			hotspots := tiering.GetHotspots()
			status["hotspots_count"] = len(hotspots)
		}

		vector := gateway.GetVectorManager()
		if vector != nil {
			stats := vector.GetStats()
			status["vector_stats"] = stats
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	}
}

func (s *SecureAdminServer) handleTieringDecisions(gateway *S3Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		tiering := gateway.GetTieringManager()
		if tiering == nil {
			http.Error(w, "Tiering not enabled", http.StatusServiceUnavailable)
			return
		}

		decisions, err := tiering.RunTieringDecision(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to run tiering decision: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"decisions": decisions,
			"count":     len(decisions),
		})
	}
}

func (s *SecureAdminServer) handleRunTiering(gateway *S3Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		tiering := gateway.GetTieringManager()
		if tiering == nil {
			http.Error(w, "Tiering not enabled", http.StatusServiceUnavailable)
			return
		}

		decisions, err := tiering.RunTieringDecision(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to run tiering decision: %v", err), http.StatusInternalServerError)
			return
		}

		if err := tiering.ExecuteMigrations(r.Context(), decisions); err != nil {
			http.Error(w, fmt.Sprintf("Failed to execute migrations: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":    "completed",
			"decisions": len(decisions),
		})
	}
}

func (s *SecureAdminServer) handleVectorStats(gateway *S3Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		vector := gateway.GetVectorManager()
		if vector == nil {
			http.Error(w, "Vector not enabled", http.StatusServiceUnavailable)
			return
		}

		stats := vector.GetStats()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats)
	}
}

func (s *SecureAdminServer) handleVectorRebuild(gateway *S3Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		vector := gateway.GetVectorManager()
		if vector == nil {
			http.Error(w, "Vector not enabled", http.StatusServiceUnavailable)
			return
		}

		if err := vector.RebuildIndex(r.Context()); err != nil {
			http.Error(w, fmt.Sprintf("Failed to rebuild index: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "completed",
		})
	}
}

func (s *SecureAdminServer) handleMetrics(gateway *S3Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		metrics := `# HELP cipherlake_up Whether CipherLake is up
# TYPE cipherlake_up gauge
cipherlake_up 1

# HELP cipherlake_requests_total Total number of requests
# TYPE cipherlake_requests_total counter
cipherlake_requests_total{endpoint="/"} 0

# HELP cipherlake_tiering_decisions_total Total number of tiering decisions
# TYPE cipherlake_tiering_decisions_total counter
cipherlake_tiering_decisions_total 0
`

		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(metrics))
	}
}

func (s *SecureAdminServer) Start() error {
	var startErr error
	s.startOnce.Do(func() {
		ln, err := net.Listen("tcp", s.server.Addr)
		if err != nil {
			startErr = err
			return
		}
		s.listener = ln
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			_ = s.server.Serve(ln)
		}()
	})
	return startErr
}

func (s *SecureAdminServer) StartTLS(certFile, keyFile string) error {
	var startErr error
	s.startOnce.Do(func() {
		ln, err := tls.Listen("tcp", s.server.Addr, &tls.Config{
			MinVersion: tls.VersionTLS12,
		})
		if err != nil {
			startErr = err
			return
		}
		s.listener = ln
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			_ = s.server.Serve(ln)
		}()
	})
	return startErr
}

func (s *SecureAdminServer) Stop() error {
	s.stopOnce.Do(func() {
		_ = s.server.Close()
	})
	s.wg.Wait()
	return nil
}

func (s *SecureAdminServer) GetAuditLog(limit int) []AuditEntry {
	return s.auth.GetAuditLog(limit)
}

func (s *SecureAdminServer) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	entries := s.auth.GetAuditLog(100)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"entries": entries,
		"count":   len(entries),
	})
}

func loadMTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load certificate: %w", err)
	}

	caCert, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA: %w", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caCertPool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
