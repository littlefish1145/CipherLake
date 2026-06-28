package gateway

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

type AuthConfig struct {
	JWTSecret      string
	RequireAuth    bool
	AnonymousRead  bool
	TokenExpiry    time.Duration
	RefreshExpiry  time.Duration
}

type AuthHandler struct {
	mu              sync.RWMutex
	secretKey       []byte
	users           map[string]*User
	config          *AuthConfig
	failedLogins    map[string]*failedLoginTracker
	failedLoginsMu  sync.RWMutex
	userStorePath   string
	iamBridge       *IAMAuthBridge // Bridge to new IAM system
}

type User struct {
	ID                string              `json:"id"`
	Name              string              `json:"name"`
	PasswordHash      string              `json:"password_hash"`
	SecretKey         string              `json:"secret_key,omitempty"`
	Role              string              `json:"role"`
	Permissions       []string            `json:"permissions"`
	BucketPermissions map[string][]string `json:"bucket_permissions,omitempty"`
}

type Claims struct {
	UserID string   `json:"user_id"`
	Role   string   `json:"role"`
	Perms  []string `json:"permissions"`
	jwt.RegisteredClaims
}

type failedLoginTracker struct {
	count     int
	firstFail time.Time
	blockedUntil time.Time
}

func NewAuthHandler() *AuthHandler {
	return NewAuthHandlerWithConfig(nil)
}

func NewAuthHandlerWithConfig(cfg *AuthConfig) *AuthHandler {
	if cfg == nil {
		cfg = &AuthConfig{
			RequireAuth:   false,
			AnonymousRead: true,
			TokenExpiry:   24 * time.Hour,
			RefreshExpiry: 7 * 24 * time.Hour,
		}
	}

	secretKey := cfg.JWTSecret
	if secretKey == "" {
		secretKey = os.Getenv("NEXUS_JWT_SECRET")
	}
	if secretKey == "" {
		secretKey = os.Getenv("JWT_SECRET")
	}
	if secretKey == "" {
		secretBytes := make([]byte, 32)
		if _, err := rand.Read(secretBytes); err != nil {
			return nil
		}
		secretKey = hex.EncodeToString(secretBytes)
		log.Println("[SECURITY WARNING] No JWT secret configured via NEXUS_JWT_SECRET or JWT_SECRET env vars. A random secret was generated and will NOT persist across restarts. Please set a stable secret for production use.")
	}

	adminPassword := os.Getenv("NEXUS_ADMIN_PASSWORD")
	if adminPassword == "" {
		pwBytes := make([]byte, 16)
		if _, err := rand.Read(pwBytes); err != nil {
			return nil
		}
		adminPassword = base64.StdEncoding.EncodeToString(pwBytes)
		log.Println("[SECURITY WARNING] No admin password configured via NEXUS_ADMIN_PASSWORD env var. A random password was generated. Set NEXUS_ADMIN_PASSWORD for consistent access.")
	}

	hashedPassword, _ := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.DefaultCost)

	adminSecretKey := os.Getenv("NEXUS_ADMIN_SECRET_KEY")

	users := map[string]*User{
		"admin": {
			ID:           "user-001",
			Name:         "admin",
			PasswordHash: string(hashedPassword),
			SecretKey:    adminSecretKey,
			Role:         "admin",
			Permissions:  []string{"read", "write", "delete", "admin"},
		},
	}

	userStorePath := os.Getenv("NEXUS_USER_STORE")
	if userStorePath == "" {
		dataDir := os.Getenv("NEXUS_DATA_DIR")
		if dataDir == "" {
			dataDir = "./data"
		}
		userStorePath = filepath.Join(dataDir, "users.json")
	}

	a := &AuthHandler{
		secretKey:     []byte(secretKey),
		config:        cfg,
		users:         users,
		failedLogins:  make(map[string]*failedLoginTracker),
		userStorePath: userStorePath,
	}

	a.loadUsers()

	return a
}

func (a *AuthHandler) Authenticate(r *http.Request) (*User, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		if a.config.RequireAuth {
			return nil, fmt.Errorf("authentication required")
		}
		return &User{
			ID:          "anonymous",
			Name:        "anonymous",
			Role:        "anonymous",
			Permissions: []string{"read", "write", "delete"},
		}, nil
	}

	authType := strings.SplitN(authHeader, " ", 2)
	if len(authType) != 2 {
		return nil, fmt.Errorf("invalid authorization header format")
	}

	var user *User
	var err error

	switch strings.ToLower(authType[0]) {
	case "basic":
		user, err = a.validateBasicAuth(authType[1])
	case "bearer":
		user, err = a.validateJWT(authType[1])
	case "aws4-hmac-sha256":
		user, err = a.validateAWSSignature(r)
	default:
		return nil, fmt.Errorf("unsupported authorization type: %s", authType[0])
	}

	if err != nil {
		return nil, err
	}

	return user, nil
}

func (a *AuthHandler) SetIAMBridge(bridge *IAMAuthBridge) {
	a.iamBridge = bridge
}

func (a *AuthHandler) RequireAuth(r *http.Request, requiredPerms ...string) (*User, error) {
	return a.RequireAuthForBucket(r, "", requiredPerms...)
}

func (a *AuthHandler) RequireAuthForBucket(r *http.Request, bucket string, requiredPerms ...string) (*User, error) {
	user, err := a.Authenticate(r)
	if err != nil {
		if !a.config.RequireAuth {
			return &User{
				ID:          "anonymous",
				Name:        "anonymous",
				Role:        "anonymous",
				Permissions: []string{"read", "write", "delete"},
			}, nil
		}
		if a.config.AnonymousRead {
			for _, required := range requiredPerms {
				if required != "read" {
					return nil, fmt.Errorf("permission denied: requires %s", required)
				}
			}
			return &User{
				ID:          "anonymous",
				Name:        "anonymous",
				Role:        "anonymous",
				Permissions: []string{"read"},
			}, nil
		}
		return nil, err
	}

	// Admin user always has access
	if user.Role == "admin" {
		return user, nil
	}

	// If IAM bridge is available, use it for fine-grained access control
	if a.iamBridge != nil {
		// Map simple permissions to IAM actions
		for _, perm := range requiredPerms {
			var iamAction string
			switch perm {
			case "read":
				iamAction = "s3:GetObject"
			case "write":
				iamAction = "s3:PutObject"
			case "delete":
				iamAction = "s3:DeleteObject"
			case "admin":
				iamAction = "s3:*"
			default:
				iamAction = "s3:" + perm
			}

			if err := a.iamBridge.CheckIAMAccess(nil, iamAction, bucket, "", r); err != nil {
				// IAM denied - but we still need the IAM user context
				// Try to get IAM user from request context
				continue
			}
		}
	}

	// Fall back to legacy permission check
	if bucket != "" && user.BucketPermissions != nil {
		return a.checkBucketPermission(user, bucket, requiredPerms...)
	}

	for _, required := range requiredPerms {
		hasPerm := false
		for _, perm := range user.Permissions {
			if perm == required || perm == "admin" {
				hasPerm = true
				break
			}
		}
		if !hasPerm {
			return nil, fmt.Errorf("permission denied: requires %s", required)
		}
	}

	return user, nil
}

func (a *AuthHandler) checkBucketPermission(user *User, bucket string, requiredPerms ...string) (*User, error) {
	var bucketPerms []string
	if bp, ok := user.BucketPermissions[bucket]; ok {
		bucketPerms = bp
	} else if bp, ok := user.BucketPermissions["*"]; ok {
		bucketPerms = bp
	}

	if len(bucketPerms) == 0 {
		if len(user.Permissions) > 0 {
			for _, required := range requiredPerms {
				hasPerm := false
				for _, perm := range user.Permissions {
					if perm == required || perm == "admin" {
						hasPerm = true
						break
					}
				}
				if !hasPerm {
					return nil, fmt.Errorf("permission denied: no access to bucket %s", bucket)
				}
			}
			return user, nil
		}
		return nil, fmt.Errorf("permission denied: no access to bucket %s", bucket)
	}

	for _, required := range requiredPerms {
		hasPerm := false
		for _, perm := range bucketPerms {
			if perm == required || perm == "*" {
				hasPerm = true
				break
			}
		}
		if !hasPerm {
			return nil, fmt.Errorf("permission denied: requires %s on bucket %s", required, bucket)
		}
	}

	return user, nil
}

func (a *AuthHandler) validateBasicAuth(encoded string) (*User, error) {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid basic auth encoding: %w", err)
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid basic auth format")
	}

	accessKey := parts[0]
	secretKey := parts[1]

	if a.isBlocked(accessKey) {
		return nil, fmt.Errorf("account temporarily locked due to too many failed attempts")
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, user := range a.users {
		if subtle.ConstantTimeCompare([]byte(user.Name), []byte(accessKey)) == 1 {
			if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(secretKey)) == nil {
				a.resetFailedLogins(accessKey)
				return user, nil
			}
		}
	}

	a.recordFailedLogin(accessKey)
	return nil, fmt.Errorf("invalid credentials")
}

func (a *AuthHandler) isBlocked(key string) bool {
	a.failedLoginsMu.RLock()
	defer a.failedLoginsMu.RUnlock()
	tracker, exists := a.failedLogins[key]
	if !exists {
		return false
	}
	return time.Now().Before(tracker.blockedUntil)
}

func (a *AuthHandler) recordFailedLogin(key string) {
	a.failedLoginsMu.Lock()
	defer a.failedLoginsMu.Unlock()
	tracker, exists := a.failedLogins[key]
	if !exists {
		tracker = &failedLoginTracker{}
		a.failedLogins[key] = tracker
	}

	if time.Since(tracker.firstFail) > 15*time.Minute {
		tracker.count = 0
		tracker.firstFail = time.Now()
	}

	tracker.count++
	if tracker.count >= 5 {
		tracker.blockedUntil = time.Now().Add(15 * time.Minute)
	}
}

func (a *AuthHandler) resetFailedLogins(key string) {
	a.failedLoginsMu.Lock()
	defer a.failedLoginsMu.Unlock()
	delete(a.failedLogins, key)
}

func (a *AuthHandler) validateJWT(tokenString string) (*User, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return a.secretKey, nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to parse JWT: %w", err)
	}

	if claims, ok := token.Claims.(*Claims); ok && token.Valid {
		return &User{
			ID:          claims.UserID,
			Name:        claims.UserID,
			Role:        claims.Role,
			Permissions: claims.Perms,
		}, nil
	}

	return nil, fmt.Errorf("invalid token")
}

func (a *AuthHandler) validateAWSSignature(r *http.Request) (*User, error) {
	authorization := r.Header.Get("Authorization")
	if authorization != "" {
		if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256") {
			return nil, fmt.Errorf("invalid AWS signature version")
		}
		accessKey := extractAWSAccessKey(authorization)
		if accessKey == "" {
			return nil, fmt.Errorf("missing access key in AWS signature")
		}

		a.mu.RLock()
		var matchedUser *User
		for _, user := range a.users {
			if subtle.ConstantTimeCompare([]byte(user.Name), []byte(accessKey)) == 1 {
				matchedUser = user
				break
			}
		}
		a.mu.RUnlock()

		if matchedUser == nil {
			return nil, fmt.Errorf("invalid AWS credentials")
		}

		if matchedUser.SecretKey == "" {
			return nil, fmt.Errorf("user has no secret key configured for SigV4 signing")
		}

		if err := verifySigV4Header(r, matchedUser.SecretKey); err != nil {
			return nil, fmt.Errorf("AWS signature verification failed")
		}

		return matchedUser, nil
	}

	accessKey := r.URL.Query().Get("X-Amz-Credential")
	if accessKey == "" {
		return nil, fmt.Errorf("missing authorization header or presigned URL parameters")
	}
	parts := strings.Split(accessKey, "/")
	if len(parts) > 0 {
		accessKey = parts[0]
	}
	if accessKey == "" {
		return nil, fmt.Errorf("missing authorization header or presigned URL parameters")
	}

	a.mu.RLock()
	var matchedUser *User
	for _, user := range a.users {
		if subtle.ConstantTimeCompare([]byte(user.Name), []byte(accessKey)) == 1 {
			matchedUser = user
			break
		}
	}
	a.mu.RUnlock()

	if matchedUser == nil {
		return nil, fmt.Errorf("invalid AWS credentials")
	}

	if matchedUser.SecretKey == "" {
		return nil, fmt.Errorf("user has no secret key configured for SigV4 signing")
	}

	if err := verifySigV4PresignedURL(r, matchedUser.SecretKey); err != nil {
		return nil, fmt.Errorf("presigned URL signature verification failed")
	}

	return matchedUser, nil
}

func extractAWSAccessKey(auth string) string {
	idx := strings.Index(auth, "Credential=")
	if idx == -1 {
		return ""
	}
	credPart := auth[idx+len("Credential="):]
	if commaIdx := strings.Index(credPart, ","); commaIdx != -1 {
		credPart = credPart[:commaIdx]
	}
	credParts := strings.Split(credPart, "/")
	if len(credParts) > 0 {
		return strings.TrimSpace(credParts[0])
	}
	return ""
}

func generateSecretKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand.Read failed: unable to generate secret key")
	}
	return hex.EncodeToString(b)
}

func (a *AuthHandler) GetUserID(r *http.Request) string {
	user, err := a.Authenticate(r)
	if err != nil {
		return "anonymous"
	}
	return user.ID
}

func (a *AuthHandler) GenerateToken(userID string) (string, error) {
	return a.GenerateTokenWithExpiry(userID, a.config.TokenExpiry)
}

func (a *AuthHandler) GenerateTokenWithExpiry(userID string, expiry time.Duration) (string, error) {
	user := a.users[userID]
	if user == nil {
		user = &User{
			ID:          userID,
			Name:        userID,
			Role:        "user",
			Permissions: []string{"read", "write"},
		}
	}

	claims := &Claims{
		UserID: userID,
		Role:   user.Role,
		Perms:  user.Permissions,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "nexus",
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(a.secretKey)
}

func (a *AuthHandler) GenerateRefreshToken(userID string) (string, error) {
	return a.GenerateTokenWithExpiry(userID, a.config.RefreshExpiry)
}

func (a *AuthHandler) RefreshToken(refreshToken string) (string, error) {
	token, err := jwt.ParseWithClaims(refreshToken, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return a.secretKey, nil
	})

	if err != nil {
		return "", fmt.Errorf("invalid refresh token: %w", err)
	}

	if claims, ok := token.Claims.(*Claims); ok && token.Valid {
		return a.GenerateToken(claims.UserID)
	}

	return "", fmt.Errorf("invalid refresh token")
}

func (a *AuthHandler) AddUser(id, name, password, secretKey, role string, permissions []string, bucketPermissions map[string][]string) error {
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}

	if secretKey == "" {
		secretKey = generateSecretKey()
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.users[name]; exists {
		return fmt.Errorf("user %s already exists", name)
	}

	a.users[name] = &User{
		ID:                id,
		Name:              name,
		PasswordHash:      string(hashedPassword),
		SecretKey:         secretKey,
		Role:              role,
		Permissions:       permissions,
		BucketPermissions: bucketPermissions,
	}

	return a.saveUsersLocked()
}

func (a *AuthHandler) RemoveUser(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if name == "admin" {
		return fmt.Errorf("cannot remove admin user")
	}

	if _, exists := a.users[name]; !exists {
		return fmt.Errorf("user %s not found", name)
	}

	delete(a.users, name)
	return a.saveUsersLocked()
}

func (a *AuthHandler) UpdateUser(name, role string, permissions []string, bucketPermissions map[string][]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	user, exists := a.users[name]
	if !exists {
		return fmt.Errorf("user %s not found", name)
	}

	if role != "" {
		user.Role = role
	}
	if permissions != nil {
		user.Permissions = permissions
	}
	if bucketPermissions != nil {
		user.BucketPermissions = bucketPermissions
	}

	return a.saveUsersLocked()
}

func (a *AuthHandler) ChangePassword(name, newPassword string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	user, exists := a.users[name]
	if !exists {
		return fmt.Errorf("user %s not found", name)
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}

	user.PasswordHash = string(hashedPassword)
	return a.saveUsersLocked()
}

func (a *AuthHandler) ListUsers() []*User {
	a.mu.RLock()
	defer a.mu.RUnlock()

	users := make([]*User, 0, len(a.users))
	for _, user := range a.users {
		users = append(users, &User{
			ID:                user.ID,
			Name:              user.Name,
			Role:              user.Role,
			Permissions:       user.Permissions,
			BucketPermissions: user.BucketPermissions,
		})
	}
	return users
}

func (a *AuthHandler) GetUser(name string) (*User, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	user, exists := a.users[name]
	if !exists {
		return nil, fmt.Errorf("user %s not found", name)
	}
	return &User{
		ID:                user.ID,
		Name:              user.Name,
		Role:              user.Role,
		Permissions:       user.Permissions,
		BucketPermissions: user.BucketPermissions,
	}, nil
}

func (a *AuthHandler) loadUsers() {
	if a.userStorePath == "" {
		return
	}

	data, err := os.ReadFile(a.userStorePath)
	if err != nil {
		return
	}

	var storedUsers map[string]*User
	if err := json.Unmarshal(data, &storedUsers); err != nil {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	for name, user := range storedUsers {
		if name == "admin" {
			a.users["admin"].Permissions = user.Permissions
			a.users["admin"].Role = user.Role
			if user.PasswordHash != "" {
				a.users["admin"].PasswordHash = user.PasswordHash
			}
			continue
		}
		a.users[name] = user
	}
}

func (a *AuthHandler) saveUsersLocked() error {
	if a.userStorePath == "" {
		return nil
	}

	data, err := json.MarshalIndent(a.users, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal users: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(a.userStorePath), 0700); err != nil {
		return fmt.Errorf("failed to create user store directory: %w", err)
	}

	tmpFile := a.userStorePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return fmt.Errorf("failed to write user store: %w", err)
	}

	return os.Rename(tmpFile, a.userStorePath)
}

func (a *AuthHandler) SetRequireAuth(require bool) {
	a.config.RequireAuth = require
}

func (a *AuthHandler) SetAnonymousRead(allow bool) {
	a.config.AnonymousRead = allow
}

type AdminAPI struct {
	gateway *S3Gateway
	auth    *AuthHandler
}

func NewAdminAPI(gateway *S3Gateway, auth *AuthHandler) *AdminAPI {
	return &AdminAPI{
		gateway: gateway,
		auth:    auth,
	}
}

func (api *AdminAPI) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/admin/status", api.handleStatus)
	mux.HandleFunc("/admin/tiering/decisions", api.handleTieringDecisions)
	mux.HandleFunc("/admin/tiering/run", api.handleRunTiering)
	mux.HandleFunc("/admin/vector/stats", api.handleVectorStats)
	mux.HandleFunc("/admin/vector/rebuild", api.handleVectorRebuild)
	mux.HandleFunc("/admin/metrics", api.handleMetrics)
	mux.HandleFunc("/admin/users", api.handleUsers)
	mux.HandleFunc("/admin/users/", api.handleUserByName)

	return mux
}

func (api *AdminAPI) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status := map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now().Format(time.RFC3339),
		"version":   "2.0",
	}

	metadata := api.gateway.GetMetadataStore()
	if metadata != nil {
		status["metadata"] = "connected"
	}

	tiering := api.gateway.GetTieringManager()
	if tiering != nil {
		hotspots := tiering.GetHotspots()
		status["hotspots_count"] = len(hotspots)
	}

	vector := api.gateway.GetVectorManager()
	if vector != nil {
		stats := vector.GetStats()
		status["vector_stats"] = stats
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (api *AdminAPI) handleTieringDecisions(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	tiering := api.gateway.GetTieringManager()
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
		"count":    len(decisions),
	})
}

func (api *AdminAPI) handleRunTiering(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	tiering := api.gateway.GetTieringManager()
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

func (api *AdminAPI) handleVectorStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	vector := api.gateway.GetVectorManager()
	if vector == nil {
		http.Error(w, "Vector not enabled", http.StatusServiceUnavailable)
		return
	}

	stats := vector.GetStats()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func (api *AdminAPI) handleVectorRebuild(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	vector := api.gateway.GetVectorManager()
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

func (api *AdminAPI) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	metrics := `# HELP nexus_up Whether Nexus is up
# TYPE nexus_up gauge
nexus_up 1

# HELP nexus_requests_total Total number of requests
# TYPE nexus_requests_total counter
nexus_requests_total{endpoint="/"} 0

# HELP nexus_tiering_decisions_total Total number of tiering decisions
# TYPE nexus_tiering_decisions_total counter
nexus_tiering_decisions_total 0
`

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(metrics))
}

type CreateUserRequest struct {
	ID                string              `json:"id"`
	Name              string              `json:"name"`
	Password          string              `json:"password"`
	SecretKey         string              `json:"secret_key,omitempty"`
	Role              string              `json:"role"`
	Permissions       []string            `json:"permissions"`
	BucketPermissions map[string][]string `json:"bucket_permissions"`
}

type UpdateUserRequest struct {
	Role              string              `json:"role"`
	Permissions       []string            `json:"permissions"`
	BucketPermissions map[string][]string `json:"bucket_permissions"`
}

type ChangePasswordRequest struct {
	Password string `json:"password"`
}

func (api *AdminAPI) requireAdminAuth(w http.ResponseWriter, r *http.Request) bool {
	user, err := api.auth.Authenticate(r)
	if err != nil || user.Role != "admin" {
		http.Error(w, "Unauthorized: admin access required", http.StatusUnauthorized)
		return false
	}
	return true
}

func (api *AdminAPI) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		if !api.requireAdminAuth(w, r) {
			return
		}
		users := api.auth.ListUsers()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"users": users,
			"count": len(users),
		})

	case "POST":
		if !api.requireAdminAuth(w, r) {
			return
		}
		var req CreateUserRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if req.Name == "" || req.Password == "" {
			http.Error(w, "name and password are required", http.StatusBadRequest)
			return
		}
		if req.ID == "" {
			req.ID = fmt.Sprintf("user-%d", time.Now().UnixNano())
		}
		if req.Role == "" {
			req.Role = "user"
		}
		if req.Permissions == nil {
			req.Permissions = []string{"read", "write"}
		}

		if err := api.auth.AddUser(req.ID, req.Name, req.Password, req.SecretKey, req.Role, req.Permissions, req.BucketPermissions); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}

		user, _ := api.auth.GetUser(req.Name)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(user)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (api *AdminAPI) handleUserByName(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/admin/users/")
	if name == "" {
		http.Error(w, "User name is required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case "GET":
		if !api.requireAdminAuth(w, r) {
			return
		}
		user, err := api.auth.GetUser(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(user)

	case "PUT":
		if !api.requireAdminAuth(w, r) {
			return
		}
		var req UpdateUserRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if err := api.auth.UpdateUser(name, req.Role, req.Permissions, req.BucketPermissions); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		user, _ := api.auth.GetUser(name)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(user)

	case "DELETE":
		if !api.requireAdminAuth(w, r) {
			return
		}
		if err := api.auth.RemoveUser(name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case "PATCH":
		if !api.requireAdminAuth(w, r) {
			return
		}
		if r.URL.Query().Get("action") == "change-password" {
			var req ChangePasswordRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "Invalid request body", http.StatusBadRequest)
				return
			}
			if req.Password == "" {
				http.Error(w, "password is required", http.StatusBadRequest)
				return
			}
			if err := api.auth.ChangePassword(name, req.Password); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "Unknown action", http.StatusBadRequest)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
